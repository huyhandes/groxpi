package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
	"github.com/phuslu/log"
	"golang.org/x/sync/singleflight"

	"github.com/huyhandes/groxpi/internal/cache"
	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/pypi"
	"github.com/huyhandes/groxpi/internal/storage"
	"github.com/huyhandes/groxpi/internal/streaming"
)

// Response buffer pool for reducing allocations
var responseBufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

type Server struct {
	config           *config.Config
	indexCache       *cache.IndexCache
	fileCache        *cache.FileCache
	responseCache    *cache.ResponseCache
	pypiClient       *pypi.Client
	storage          storage.Storage
	router           *gin.Engine
	sf               singleflight.Group // For deduplicating concurrent requests
	streamDownloader streaming.StreamingDownloader
	packageFiles     *PackageFileService // Owns the package-file miss pipeline
}

func New(cfg *config.Config) *Server {
	// Set Gin mode based on log level
	if cfg.LogLevel == "DEBUG" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// Create Gin router
	router := gin.New()

	// Add middleware
	router.Use(gin.Recovery())
	router.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("[%s] %d - %v %s %s\n",
			param.TimeStamp.Format(time.RFC3339),
			param.StatusCode,
			param.Latency,
			param.Method,
			param.Path,
		)
	}))

	// Add compression middleware
	router.Use(gzip.Gzip(gzip.BestSpeed))

	// Note: Templates are not currently used - handlers generate HTML inline
	// This avoids issues with template syntax differences between frameworks

	// Initialize storage backend
	storageBackend, err := initStorage(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize storage")
	}

	// Create HTTP client for streaming downloader with configured timeout
	streamTimeout := cfg.DownloadTimeout
	if streamTimeout <= 0 {
		streamTimeout = 5 * time.Minute // Default 5 minutes for large files
	}
	streamClient := &http.Client{
		Timeout: streamTimeout,
	}

	s := &Server{
		config:           cfg,
		indexCache:       cache.NewIndexCache(),
		fileCache:        cache.NewFileCache(cfg.CacheDir, cfg.CacheSize),
		responseCache:    cache.NewResponseCache(50 * 1024 * 1024), // 50MB response cache
		pypiClient:       pypi.NewClient(cfg),
		storage:          storageBackend,
		router:           router,
		streamDownloader: streaming.NewTeeStreamingDownloader(&storageAdapter{storageBackend}, streamClient),
	}

	s.packageFiles = newPackageFileService(
		cfg,
		storageBackend,
		s.fileCache,
		s.indexCache,
		s.pypiClient,
		s.streamDownloader,
		&s.sf,
	)

	s.setupRoutes()
	return s
}

func (s *Server) Router() *gin.Engine {
	return s.router
}

func (s *Server) setupRoutes() {
	// Home page
	s.router.GET("/", s.handleHome)

	// Package index routes - both /simple/ (PEP 503) and /index/ for compatibility
	s.router.GET("/simple/", s.handleListPackages)
	s.router.GET("/simple/:package/", s.handleListFiles)
	s.router.GET("/simple/:package/:file", s.handleDownloadFile)

	s.router.GET("/index/", s.handleListPackages)
	s.router.GET("/index/:package", s.handleListFiles)
	s.router.GET("/index/:package/:file", s.handleDownloadFile)

	// Cache management
	s.router.DELETE("/cache/list", s.handleCacheList)
	// Explicit method handlers for unsupported methods (Gin doesn't allow Any after DELETE)
	s.router.GET("/cache/list", s.handleCacheListMethodNotAllowed)
	s.router.POST("/cache/list", s.handleCacheListMethodNotAllowed)
	s.router.PUT("/cache/list", s.handleCacheListMethodNotAllowed)
	s.router.PATCH("/cache/list", s.handleCacheListMethodNotAllowed)
	s.router.HEAD("/cache/list", s.handleCacheListMethodNotAllowed)
	s.router.OPTIONS("/cache/list", s.handleCacheListMethodNotAllowed)
	s.router.DELETE("/cache/:package", s.handleCachePackage)

	// Health check
	s.router.GET("/health", s.handleHealth)

	// 404 handler
	s.router.NoRoute(func(c *gin.Context) {
		c.String(http.StatusNotFound, "Not Found")
	})
}

func (s *Server) handleHome(c *gin.Context) {
	// For now, return simple HTML without layout
	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><title>groxpi - PyPI Cache</title></head>
<body>
	<h1>groxpi - PyPI Cache</h1>
	<p>High-performance PyPI caching proxy server written in Go.</p>
	<ul>
		<li>Index URL: %s</li>
		<li>Cache Size: %d MB</li>
		<li>Index TTL: %s</li>
		<li>Version: 1.0.0</li>
	</ul>
	<p><a href="/index/">Browse packages</a> | <a href="/health">Health Check</a></p>
</body>
</html>`, s.config.IndexURL, s.config.CacheSize/(1024*1024), s.config.IndexTTL.String())

	c.Header("Content-Type", "text/html")
	c.String(http.StatusOK, html)
}

func (s *Server) handleListPackages(c *gin.Context) {
	// Check response cache first for JSON requests
	if wantsJSON(c) {
		cacheKey := "json:package-list"
		if cachedJSON, found := s.responseCache.Get(cacheKey); found {
			c.Data(http.StatusOK, "application/vnd.pypi.simple.v1+json", cachedJSON)
			return
		}
	}

	// Check cache for parsed data
	var packages []string
	if cachedData, found := s.indexCache.Get("package-list"); found {
		if cachedPackages, ok := cachedData.([]string); ok {
			packages = cachedPackages
		}
	}

	if len(packages) == 0 {
		// Use singleflight to deduplicate concurrent requests
		result, err, _ := s.sf.Do("package-list", func() (any, error) {
			return s.pypiClient.GetPackageList()
		})

		if err != nil {
			log.Error().Err(err).Msg("Failed to fetch package list")
			packages = []string{} // Use empty list on error
		} else {
			packages = result.([]string)
			// Cache the result
			s.indexCache.Set("package-list", packages, s.config.IndexTTL)
		}
	}

	if wantsJSON(c) {
		// Pre-allocate with exact capacity
		projects := make([]map[string]string, 0, len(packages))
		for _, pkg := range packages {
			projects = append(projects, map[string]string{"name": pkg})
		}

		response := map[string]any{
			"meta": map[string]any{
				"api-version": "1.0",
			},
			"projects": projects,
		}

		// Use streaming JSON encoder for zero-copy optimization
		buf := responseBufferPool.Get().(*bytes.Buffer)
		defer func() {
			buf.Reset()
			responseBufferPool.Put(buf)
		}()

		encoder := sonic.ConfigFastest.NewEncoder(buf)
		if err := encoder.Encode(response); err != nil {
			c.String(http.StatusInternalServerError, "JSON encoding error")
			return
		}

		// Cache the JSON response
		jsonData := buf.Bytes()
		cacheKey := "json:package-list"
		// Make a copy for cache and response since buf will be reused
		responseData := make([]byte, len(jsonData))
		copy(responseData, jsonData)
		s.responseCache.Set(cacheKey, responseData, s.config.IndexTTL)

		c.Data(http.StatusOK, "application/vnd.pypi.simple.v1+json", responseData)
		return
	}

	// Return simple HTML for packages
	html := `<!DOCTYPE html>
<html>
<head><title>Package Index</title></head>
<body>
	<h1>Simple index</h1>
	<p>No packages cached yet. Install a package to populate the cache.</p>
	<p><a href="/">← Back to home</a></p>
</body>
</html>`
	c.Header("Content-Type", "text/html")
	c.String(http.StatusOK, html)
}

func (s *Server) handleListFiles(c *gin.Context) {
	packageName := c.Param("package")

	// Normalize package name
	packageName = normalizePackageName(packageName)

	// Check response cache first for JSON requests
	if wantsJSON(c) {
		cacheKey := "json:package:" + packageName
		if cachedJSON, found := s.responseCache.Get(cacheKey); found {
			c.Data(http.StatusOK, "application/vnd.pypi.simple.v1+json", cachedJSON)
			return
		}
	}

	// Check cache for parsed data
	if cachedData, found := s.indexCache.GetPackage(packageName); found {
		if cachedFiles, ok := cachedData.([]pypi.FileInfo); ok {
			s.renderPackageFiles(c, packageName, cachedFiles)
			return
		}
	}

	// Use singleflight to deduplicate concurrent requests for the same package
	key := "package-files:" + packageName
	result, err, _ := s.sf.Do(key, func() (any, error) {
		return s.pypiClient.GetPackageFiles(packageName)
	})

	if err != nil {
		// If package not found, return 404
		if strings.Contains(err.Error(), "not found") {
			c.String(http.StatusNotFound, "Package not found")
			return
		}
		// Log the error for debugging
		fmt.Printf("Error fetching package %s: %v\n", packageName, err)
		c.String(http.StatusInternalServerError, "Error fetching package: "+err.Error())
		return
	}

	files := result.([]pypi.FileInfo)

	// Cache the result
	s.indexCache.SetPackage(packageName, files, s.config.IndexTTL)

	s.renderPackageFiles(c, packageName, files)
}

func (s *Server) renderPackageFiles(c *gin.Context, packageName string, files []pypi.FileInfo) {
	if wantsJSON(c) {
		// Get buffer from pool
		buf := responseBufferPool.Get().(*bytes.Buffer)
		defer func() {
			buf.Reset()
			responseBufferPool.Put(buf)
		}()

		// Pre-allocate slice with exact capacity
		fileList := make([]map[string]any, 0, len(files))

		for _, file := range files {
			// Use simple map
			fileMap := make(map[string]any, 6)
			fileMap["filename"] = file.Name
			// Rewrite URL to point to proxy instead of direct PyPI
			fileMap["url"] = fmt.Sprintf("/simple/%s/%s", packageName, file.Name)

			if len(file.Hashes) > 0 {
				fileMap["hashes"] = file.Hashes
			}
			if file.RequiresPython != "" {
				fileMap["requires-python"] = file.RequiresPython
			}
			if file.IsYanked() {
				fileMap["yanked"] = true
				yankedReason := file.GetYankedReason()
				if yankedReason != "" {
					fileMap["yanked-reason"] = yankedReason
				}
			}
			fileList = append(fileList, fileMap)
		}

		// Build response structure
		response := map[string]any{
			"meta": map[string]any{
				"api-version": "1.0",
			},
			"name":  packageName,
			"files": fileList,
		}

		// Use streaming JSON encoder for zero-copy optimization
		encoder := sonic.ConfigFastest.NewEncoder(buf)
		if err := encoder.Encode(response); err != nil {
			c.String(http.StatusInternalServerError, "JSON encoding error")
			return
		}

		// Cache the JSON response
		jsonData := buf.Bytes()
		cacheKey := "json:package:" + packageName
		// Make a copy for cache and response since buf will be reused
		responseData := make([]byte, len(jsonData))
		copy(responseData, jsonData)
		s.responseCache.Set(cacheKey, responseData, s.config.IndexTTL)

		c.Data(http.StatusOK, "application/vnd.pypi.simple.v1+json", responseData)
		return
	}

	// Return HTML for package files using string builder for efficiency
	var sb strings.Builder
	sb.Grow(1024 + len(files)*200) // Pre-allocate estimated size

	sb.WriteString(`<!DOCTYPE html>
<html>
<head><title>Links for `)
	sb.WriteString(packageName)
	sb.WriteString(`</title></head>
<body>
	<h1>Links for `)
	sb.WriteString(packageName)
	sb.WriteString(`</h1>
`)

	for _, file := range files {
		sb.WriteString(`	<a href="`)
		// Rewrite URL to point to proxy instead of direct PyPI
		_, _ = fmt.Fprintf(&sb, "/simple/%s/%s", packageName, file.Name)
		sb.WriteString(`"`)

		if file.RequiresPython != "" {
			sb.WriteString(` data-requires-python="`)
			sb.WriteString(file.RequiresPython)
			sb.WriteString(`"`)
		}
		if file.IsYanked() {
			sb.WriteString(` data-yanked="`)
			if reason := file.GetYankedReason(); reason != "" {
				sb.WriteString(reason)
			}
			sb.WriteString(`"`)
		}

		sb.WriteString(`>`)
		sb.WriteString(file.Name)
		sb.WriteString(`</a><br>
`)
	}

	sb.WriteString(`</body>
</html>`)
	c.Header("Content-Type", "text/html")
	c.String(http.StatusOK, sb.String())
}

func (s *Server) handleDownloadFile(c *gin.Context) {
	packageName := normalizePackageName(c.Param("package"))
	fileName := c.Param("file")

	log.Debug().
		Str("package", packageName).
		Str("file", fileName).
		Str("user_agent", c.GetHeader("User-Agent")).
		Str("client_ip", c.ClientIP()).
		Msg("📦 File download request received")

	plan, err := s.packageFiles.Plan(c.Request.Context(), packageName, fileName)
	if err != nil {
		log.Debug().Err(err).Str("package", packageName).Str("file", fileName).Msg("Package index unavailable")
		c.String(http.StatusNotFound, "Package not found")
		return
	}

	s.servePlan(c, plan)
}

// servePlan translates a ServePlan into an HTTP response. It makes no policy
// decisions of its own.
func (s *Server) servePlan(c *gin.Context, plan ServePlan) {
	switch plan.Action {
	case ActionFromStorage:
		if err := s.serveFromStorageOptimized(c, plan.StorageKey); err != nil {
			log.Error().Err(err).Str("storage_key", plan.StorageKey).Msg("Failed to serve from storage")
			// Only a failure that happened before the first body byte can still
			// be reported; anything later would append garbage to the payload.
			if !c.Writer.Written() {
				c.String(http.StatusInternalServerError, "Failed to serve file")
			}
		}
	case ActionFromFileCache:
		c.File(plan.FilePath)
	case ActionStreamAndCache:
		s.streamAndCache(c, plan)
	case ActionRedirect:
		c.Redirect(http.StatusFound, plan.URL)
	default:
		c.String(http.StatusNotFound, "File not found")
	}
}

// streamAndCache streams the upstream file to the client while it is cached.
// Concurrent requests for the same file are deduplicated inside the service:
// only one of them streams, the rest are served the freshly cached object.
func (s *Server) streamAndCache(c *gin.Context, plan ServePlan) {
	// Headers are emitted lazily, immediately before the first body byte, because
	// gin flushes them on the first Write and silently drops anything set later.
	body := &headerWriter{w: c.Writer, header: func() { applyDownloadHeaders(c, plan) }}

	result, led, err := s.packageFiles.Fetch(c.Request.Context(), plan, body)

	switch {
	case err != nil:
		log.Error().
			Err(err).
			Str("package", plan.PackageName).
			Str("file", plan.FileName).
			Str("file_url", plan.URL).
			Int64("file_size", plan.Size).
			Dur("timeout", plan.Timeout).
			Msg("Failed to stream download, redirecting to PyPI")
		if body.wrote {
			// The body is already partly on the wire; a redirect would corrupt it.
			c.Abort()
			return
		}
		c.Redirect(http.StatusFound, plan.URL)
	case !led:
		s.serveDeduplicated(c, plan)
	default:
		log.Info().
			Str("package", plan.PackageName).
			Str("file", plan.FileName).
			Int64("size", result.Size).
			Bool("cached", result.Error == nil).
			Msg("✅ Successfully streamed file to client")
	}
}

// serveDeduplicated handles a request that waited on another request's download:
// the object should now be cached, so re-plan and serve it from there. If it is
// not (async or failed cache write), fall back to the upstream URL.
func (s *Server) serveDeduplicated(c *gin.Context, plan ServePlan) {
	next, err := s.packageFiles.Plan(c.Request.Context(), plan.PackageName, plan.FileName)
	if err == nil && (next.Action == ActionFromStorage || next.Action == ActionFromFileCache) {
		log.Debug().
			Str("package", plan.PackageName).
			Str("file", plan.FileName).
			Msg("✅ Serving from storage after coordinated download")
		s.servePlan(c, next)
		return
	}

	log.Debug().
		Str("package", plan.PackageName).
		Str("file", plan.FileName).
		Msg("⏭️ Redirecting to PyPI after download coordination")
	c.Redirect(http.StatusFound, plan.URL)
}

// applyDownloadHeaders sets everything knowable about the response before the
// first byte of the body is written.
func applyDownloadHeaders(c *gin.Context, plan ServePlan) {
	if plan.ContentType != "" {
		c.Header("Content-Type", plan.ContentType)
	}
	if plan.Size > 0 {
		c.Header("Content-Length", strconv.FormatInt(plan.Size, 10))
	}
	if plan.ETag != "" {
		c.Header("ETag", plan.ETag)
	}
}

// headerWriter defers header emission to the first body write, guaranteeing the
// headers precede the body no matter when the writer is first used.
type headerWriter struct {
	w      io.Writer
	header func()
	once   sync.Once
	wrote  bool
}

func (hw *headerWriter) Write(p []byte) (int, error) {
	hw.once.Do(func() {
		hw.wrote = true
		hw.header()
	})
	return hw.w.Write(p)
}

func (s *Server) handleCacheList(c *gin.Context) {
	// Invalidate both index and response caches
	s.indexCache.InvalidateList()
	s.responseCache.Invalidate("json:package-list")

	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"data":   nil,
	})
}

func (s *Server) handleCacheListMethodNotAllowed(c *gin.Context) {
	if c.Request.Method != "DELETE" {
		c.String(http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	c.Next()
}

func (s *Server) handleCachePackage(c *gin.Context) {
	packageName := c.Param("package")

	if packageName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"message": "Package name required",
		})
		return
	}

	// Invalidate both index and response caches
	s.indexCache.InvalidatePackage(packageName)
	s.responseCache.Invalidate("json:package:" + packageName)

	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"data":   nil,
	})
}

func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    "success",
		"timestamp": time.Now().Unix(),
		"data": gin.H{
			"cache_dir":         s.config.CacheDir,
			"index_url":         s.config.IndexURL,
			"cache_size":        s.config.CacheSize,
			"index_ttl_seconds": int(s.config.IndexTTL.Seconds()),
			"storage_type":      s.config.StorageType,
		},
	})
}

func wantsJSON(c *gin.Context) bool {
	// Check format query parameter
	if format := c.Query("format"); format != "" {
		return strings.Contains(format, "json")
	}

	// Check Accept header
	accept := c.GetHeader("Accept")
	if accept == "" {
		return false
	}

	// Check for JSON preference in Accept header
	return strings.Contains(accept, "application/vnd.pypi.simple") &&
		strings.Contains(accept, "json")
}

func normalizePackageName(name string) string {
	// PyPI package names are case-insensitive and
	// treat hyphens and underscores as equivalent
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	return name
}

// initStorage creates the appropriate storage backend based on configuration
func initStorage(cfg *config.Config) (storage.Storage, error) {
	if cfg.StorageType == "hybrid" {
		// Create hybrid/tiered storage with local L1 cache and S3 L2 cache
		return storage.NewTieredStorage(&storage.TieredConfig{
			LocalCacheDir:  cfg.LocalCacheDir,
			LocalCacheSize: cfg.LocalCacheSize,
			LocalCacheTTL:  cfg.LocalCacheTTL,
			S3Config: &storage.S3Config{
				Endpoint:        cfg.S3Endpoint,
				AccessKeyID:     cfg.S3AccessKeyID,
				SecretAccessKey: cfg.S3SecretAccessKey,
				Region:          cfg.S3Region,
				Bucket:          cfg.S3Bucket,
				Prefix:          cfg.S3Prefix,
				UseSSL:          cfg.S3UseSSL,
				ForcePathStyle:  cfg.S3ForcePathStyle,
				PartSize:        cfg.S3PartSize,
				MaxConnections:  cfg.S3MaxConnections,

				// Performance configuration
				ReadPoolSize:   cfg.S3ReadPoolSize,
				WritePoolSize:  cfg.S3WritePoolSize,
				MetaPoolSize:   cfg.S3MetaPoolSize,
				EnableHTTP2:    cfg.S3EnableHTTP2,
				TransferAccel:  cfg.S3TransferAccel,
				AsyncWrites:    cfg.S3AsyncWrites,
				AsyncWorkers:   cfg.S3AsyncWorkers,
				AsyncQueueSize: cfg.S3AsyncQueueSize,
				ConnectTimeout: cfg.ConnectTimeout,
				RequestTimeout: cfg.DownloadTimeout,
			},
			SyncWorkers:   cfg.TieredSyncWorkers,
			SyncQueueSize: cfg.TieredSyncQueueSize,
		})
	}

	if cfg.StorageType == "s3" {
		return storage.NewS3Storage(&storage.S3Config{
			Endpoint:        cfg.S3Endpoint,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
			Region:          cfg.S3Region,
			Bucket:          cfg.S3Bucket,
			Prefix:          cfg.S3Prefix,
			UseSSL:          cfg.S3UseSSL,
			ForcePathStyle:  cfg.S3ForcePathStyle,
			PartSize:        cfg.S3PartSize,
			MaxConnections:  cfg.S3MaxConnections,

			// Performance configuration
			ReadPoolSize:   cfg.S3ReadPoolSize,
			WritePoolSize:  cfg.S3WritePoolSize,
			MetaPoolSize:   cfg.S3MetaPoolSize,
			EnableHTTP2:    cfg.S3EnableHTTP2,
			TransferAccel:  cfg.S3TransferAccel,
			AsyncWrites:    cfg.S3AsyncWrites,
			AsyncWorkers:   cfg.S3AsyncWorkers,
			AsyncQueueSize: cfg.S3AsyncQueueSize,
			ConnectTimeout: cfg.ConnectTimeout,
			RequestTimeout: cfg.DownloadTimeout,
		})
	}

	// Default to local storage with LRU eviction (no TTL for non-hybrid mode)
	return storage.NewLRULocalStorage(cfg.CacheDir, cfg.CacheSize, 0)
}

// serveFromStorage serves a file from the storage backend
func (s *Server) serveFromStorage(c *gin.Context, storageKey string) error {
	// Read-only serving: it is correct to abandon it when the client goes away.
	ctx := c.Request.Context()

	log.Debug().
		Str("storage_key", storageKey).
		Str("method", c.Request.Method).
		Msg("Starting file serve from storage")

	// Open first: the reader and the metadata both arrive before a single body
	// byte is written, so every header below is still settable.
	reader, info, err := s.storage.Get(ctx, storageKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			log.Debug().Str("key", storageKey).Msg("Object missing from storage")
			c.String(http.StatusNotFound, "File not found")
			return nil
		}
		log.Error().Err(err).Str("key", storageKey).Msg("Failed to get from storage")
		c.String(http.StatusInternalServerError, "Storage error")
		return nil
	}
	defer func() { _ = reader.Close() }()

	// Set headers
	if info.ContentType != "" {
		c.Header("Content-Type", info.ContentType)
	} else {
		c.Header("Content-Type", "application/octet-stream")
	}

	if info.Size > 0 {
		c.Header("Content-Length", fmt.Sprintf("%d", info.Size))
	}

	// Extract filename from storage key
	filename := path.Base(storageKey)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	// Set cache headers for better performance
	c.Header("Cache-Control", "public, max-age=3600")
	if info.ETag != "" {
		c.Header("ETag", fmt.Sprintf(`"%s"`, info.ETag))
	}

	// Handle HEAD requests without reading body
	if c.Request.Method == "HEAD" {
		log.Debug().
			Str("storage_key", storageKey).
			Int64("size", info.Size).
			Msg("Serving HEAD request from storage")
		return nil
	}

	log.Debug().
		Str("storage_key", storageKey).
		Int64("size", info.Size).
		Msg("Starting file stream from storage")

	// Use io.Copy to manually stream the file to the response writer
	// c.Writer is safe for concurrent use (unlike Fiber's context)
	written, err := io.Copy(c.Writer, reader)
	if err != nil {
		log.Error().
			Err(err).
			Str("storage_key", storageKey).
			Int64("bytes_written", written).
			Msg("Failed to stream file from storage")
		return err
	}

	log.Debug().
		Str("storage_key", storageKey).
		Int64("bytes_written", written).
		Msg("File stream completed successfully")

	return nil
}

// serveFromStorageOptimized serves a file from storage, preferring the
// zero-copy path when the backend can name a real file for the kernel to
// sendfile. Backends that cannot fall back to open-then-stream.
func (s *Server) serveFromStorageOptimized(c *gin.Context, storageKey string) error {
	// Read-only serving: it is correct to abandon it when the client goes away.
	ctx := c.Request.Context()

	// Zero-copy is a capability, not a property of every backend: ask, do not
	// assume, and do not branch on a boolean the backend has to lie about.
	if zeroCopy, ok := s.storage.(storage.ZeroCopyCapable); ok {
		if filePath, err := zeroCopy.GetFilePath(ctx, storageKey); err == nil {
			log.Debug().
				Str("storage_key", storageKey).
				Str("file_path", filePath).
				Msg("Using zero-copy file serving")
			c.File(filePath)
			return nil
		}
	}

	// Fall back to streaming from storage
	log.Debug().
		Str("storage_key", storageKey).
		Msg("Using streaming from storage backend")

	return s.serveFromStorage(c, storageKey)
}

// storageAdapter adapts storage.Storage to streaming.StorageWriter
type storageAdapter struct {
	storage storage.Storage
}

func (sa *storageAdapter) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	_, err := sa.storage.Put(ctx, key, reader, size, contentType)
	return err
}
