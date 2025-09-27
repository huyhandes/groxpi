package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/template/html/v2"
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
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// downloadStatus represents the status of an ongoing download
type downloadStatus struct {
	mu         sync.RWMutex
	inProgress bool
	completed  bool
	storageKey string
	startTime  time.Time
	waitGroup  sync.WaitGroup
	error      error
}

// downloadCoordinator manages concurrent downloads of the same file
type downloadCoordinator struct {
	mu        sync.RWMutex
	downloads map[string]*downloadStatus
}

// newDownloadCoordinator creates a new download coordinator
func newDownloadCoordinator() *downloadCoordinator {
	return &downloadCoordinator{
		downloads: make(map[string]*downloadStatus),
	}
}

// calculateDynamicTimeout calculates appropriate timeout based on file size
func (s *Server) calculateDynamicTimeout(expectedSize int64) time.Duration {
	if expectedSize <= 0 {
		// Use default timeout for unknown sizes
		return s.config.DownloadTimeout
	}

	// Calculate timeout based on minimum transfer speed
	// Use 100 KB/s as minimum acceptable speed for S3 uploads
	const minSpeedBytesPerSec = 100 * 1024

	// Calculate base timeout: file_size / min_speed
	baseTimeout := time.Duration(expectedSize/minSpeedBytesPerSec) * time.Second

	// Add minimum timeout of 2 minutes for network overhead
	minTimeout := 2 * time.Minute
	if baseTimeout < minTimeout {
		baseTimeout = minTimeout
	}

	// Cap maximum timeout at 1 hour to prevent indefinite waits
	maxTimeout := 60 * time.Minute
	if baseTimeout > maxTimeout {
		baseTimeout = maxTimeout
	}

	log.Debug().
		Int64("expected_size", expectedSize).
		Dur("calculated_timeout", baseTimeout).
		Msg("🕐 Calculated dynamic timeout for download")

	return baseTimeout
}

type Server struct {
	config           *config.Config
	indexCache       *cache.IndexCache
	fileCache        *cache.FileCache
	responseCache    *cache.ResponseCache
	pypiClient       *pypi.Client
	storage          storage.Storage
	app              *fiber.App
	sf               singleflight.Group // For deduplicating concurrent requests
	streamDownloader streaming.StreamingDownloader
	downloadCoord    *downloadCoordinator // For coordinating concurrent downloads
}

func New(cfg *config.Config) *Server {
	// Initialize HTML template engine
	engine := html.New("./templates", ".html")
	engine.Reload(cfg.LogLevel == "DEBUG") // Enable hot reload in debug mode

	// Create Fiber app with template engine
	app := fiber.New(fiber.Config{
		Views:                 engine,
		PassLocalsToViews:     true,
		JSONEncoder:           sonic.Marshal,
		JSONDecoder:           sonic.Unmarshal,
		DisableStartupMessage: false,
		ServerHeader:          "groxpi",
		AppName:               "groxpi v1.0.0",
	})

	// Add middleware
	app.Use(recover.New())
	app.Use(logger.New(logger.Config{
		Format: "[${time}] ${status} - ${latency} ${method} ${path}\n",
	}))

	// Add compression middleware
	app.Use(compress.New(compress.Config{
		Level: compress.LevelBestSpeed,
	}))

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
		app:              app,
		streamDownloader: streaming.NewTeeStreamingDownloader(&storageAdapter{storageBackend}, streamClient),
		downloadCoord:    newDownloadCoordinator(),
	}

	s.setupRoutes()
	return s
}

func (s *Server) App() *fiber.App {
	return s.app
}

func (s *Server) setupRoutes() {
	// Home page
	s.app.Get("/", s.handleHome)

	// Package index routes - both /simple/ (PEP 503) and /index/ for compatibility
	s.app.Get("/simple/", s.handleListPackages)
	s.app.Get("/simple/:package/", s.handleListFiles)
	s.app.Get("/simple/:package/:file", s.handleDownloadFile)

	s.app.Get("/index/", s.handleListPackages)
	s.app.Get("/index/:package", s.handleListFiles)
	s.app.Get("/index/:package/:file", s.handleDownloadFile)

	// Cache management
	s.app.Delete("/cache/list", s.handleCacheList)
	s.app.All("/cache/list", s.handleCacheListMethodNotAllowed)
	s.app.Delete("/cache/:package", s.handleCachePackage)

	// Health check
	s.app.Get("/health", s.handleHealth)

	// 404 handler
	s.app.Use(func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusNotFound).SendString("Not Found")
	})
}

func (s *Server) handleHome(c *fiber.Ctx) error {
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

	return c.Type("html").SendString(html)
}

func (s *Server) handleListPackages(c *fiber.Ctx) error {
	// Check response cache first for JSON requests
	if wantsJSON(c) {
		cacheKey := "json:package-list"
		if cachedJSON, found := s.responseCache.Get(cacheKey); found {
			c.Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			return c.Send(cachedJSON)
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
		result, err, _ := s.sf.Do("package-list", func() (interface{}, error) {
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

		response := map[string]interface{}{
			"meta": map[string]interface{}{
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
			return err
		}

		// Cache the JSON response
		jsonData := buf.Bytes()
		cacheKey := "json:package-list"
		// Make a copy for cache and response since buf will be reused
		responseData := make([]byte, len(jsonData))
		copy(responseData, jsonData)
		s.responseCache.Set(cacheKey, responseData, s.config.IndexTTL)

		c.Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		return c.Send(responseData)
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
	return c.Type("html").SendString(html)
}

func (s *Server) handleListFiles(c *fiber.Ctx) error {
	packageName := c.Params("package")

	// Normalize package name
	packageName = normalizePackageName(packageName)

	// Check response cache first for JSON requests
	if wantsJSON(c) {
		cacheKey := "json:package:" + packageName
		if cachedJSON, found := s.responseCache.Get(cacheKey); found {
			c.Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			return c.Send(cachedJSON)
		}
	}

	// Check cache for parsed data
	if cachedData, found := s.indexCache.GetPackage(packageName); found {
		if cachedFiles, ok := cachedData.([]pypi.FileInfo); ok {
			return s.renderPackageFiles(c, packageName, cachedFiles)
		}
	}

	// Use singleflight to deduplicate concurrent requests for the same package
	key := "package-files:" + packageName
	result, err, _ := s.sf.Do(key, func() (interface{}, error) {
		return s.pypiClient.GetPackageFiles(packageName)
	})

	if err != nil {
		// If package not found, return 404
		if strings.Contains(err.Error(), "not found") {
			return c.Status(fiber.StatusNotFound).SendString("Package not found")
		}
		// Log the error for debugging
		fmt.Printf("Error fetching package %s: %v\n", packageName, err)
		return c.Status(fiber.StatusInternalServerError).SendString("Error fetching package: " + err.Error())
	}

	files := result.([]pypi.FileInfo)

	// Cache the result
	s.indexCache.SetPackage(packageName, files, s.config.IndexTTL)

	return s.renderPackageFiles(c, packageName, files)
}

func (s *Server) renderPackageFiles(c *fiber.Ctx, packageName string, files []pypi.FileInfo) error {
	if wantsJSON(c) {
		// Get buffer from pool
		buf := responseBufferPool.Get().(*bytes.Buffer)
		defer func() {
			buf.Reset()
			responseBufferPool.Put(buf)
		}()

		// Pre-allocate slice with exact capacity
		fileList := make([]map[string]interface{}, 0, len(files))

		for _, file := range files {
			// Use simple map to avoid fiber.Map overhead
			fileMap := make(map[string]interface{}, 6)
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
		response := map[string]interface{}{
			"meta": map[string]interface{}{
				"api-version": "1.0",
			},
			"name":  packageName,
			"files": fileList,
		}

		// Use streaming JSON encoder for zero-copy optimization
		encoder := sonic.ConfigFastest.NewEncoder(buf)
		if err := encoder.Encode(response); err != nil {
			return err
		}

		// Cache the JSON response
		jsonData := buf.Bytes()
		cacheKey := "json:package:" + packageName
		// Make a copy for cache and response since buf will be reused
		responseData := make([]byte, len(jsonData))
		copy(responseData, jsonData)
		s.responseCache.Set(cacheKey, responseData, s.config.IndexTTL)

		c.Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		return c.Send(responseData)
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
		sb.WriteString(fmt.Sprintf("/simple/%s/%s", packageName, file.Name))
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
	return c.Type("html").SendString(sb.String())
}

func (s *Server) handleDownloadFile(c *fiber.Ctx) error {
	packageName := c.Params("package")
	fileName := c.Params("file")

	log.Debug().
		Str("package", packageName).
		Str("file", fileName).
		Str("user_agent", c.Get("User-Agent")).
		Str("client_ip", c.IP()).
		Msg("📦 File download request received")

	// Normalize package name
	packageName = normalizePackageName(packageName)

	return s.handleDownloadWithCoordination(c, packageName, fileName)
}

// handleDownloadWithCoordination coordinates concurrent downloads of the same file
func (s *Server) handleDownloadWithCoordination(c *fiber.Ctx, packageName, fileName string) error {
	downloadKey := fmt.Sprintf("%s/%s", packageName, fileName)
	storageKey := fmt.Sprintf("packages/%s/%s", packageName, fileName)

	// Check if file already exists in storage - fast path
	ctx := context.Background()
	if exists, _ := s.storage.Exists(ctx, storageKey); exists {
		log.Debug().Str("package", packageName).Str("file", fileName).Msg("✅ Serving from storage cache")
		return s.serveFromStorageOptimized(c, storageKey)
	}

	// Get or create download status
	s.downloadCoord.mu.Lock()
	status, exists := s.downloadCoord.downloads[downloadKey]
	if !exists {
		status = &downloadStatus{
			storageKey: storageKey,
			startTime:  time.Now(),
		}
		s.downloadCoord.downloads[downloadKey] = status
		status.waitGroup.Add(1)
		status.inProgress = true
		s.downloadCoord.mu.Unlock()

		// First request - handle the download
		log.Info().Str("package", packageName).Str("file", fileName).Msg("🚀 Starting coordinated download")

		// Perform the actual download
		err := s.handleDownloadInternal(c, packageName, fileName)

		// Update status and wake up waiting requests
		status.mu.Lock()
		status.inProgress = false
		status.completed = true
		status.error = err
		status.mu.Unlock()
		status.waitGroup.Done()

		// Clean up after a delay
		go func() {
			time.Sleep(30 * time.Second)
			s.downloadCoord.mu.Lock()
			delete(s.downloadCoord.downloads, downloadKey)
			s.downloadCoord.mu.Unlock()
		}()

		return err
	} else {
		s.downloadCoord.mu.Unlock()

		// Subsequent requests - wait for the download to complete
		log.Debug().Str("package", packageName).Str("file", fileName).Msg("🔄 Waiting for ongoing download")

		// Wait for the download to complete
		status.waitGroup.Wait()

		status.mu.RLock()
		downloadErr := status.error
		status.mu.RUnlock()

		// If the original download succeeded, serve from storage
		if downloadErr == nil {
			if exists, _ := s.storage.Exists(ctx, storageKey); exists {
				log.Debug().Str("package", packageName).Str("file", fileName).Msg("✅ Serving from storage after coordinated download")
				return s.serveFromStorageOptimized(c, storageKey)
			}
		}

		// If download failed, try to get file URL and redirect
		if files, err := s.pypiClient.GetPackageFiles(packageName); err == nil {
			for _, file := range files {
				if file.Name == fileName {
					log.Debug().Str("package", packageName).Str("file", fileName).Msg("⏭️ Redirecting to PyPI after download coordination")
					return c.Redirect(file.URL, fiber.StatusFound)
				}
			}
		}

		return c.Status(fiber.StatusNotFound).SendString("File not found")
	}
}

// handleDownloadInternal performs the actual download logic with streaming and caching
func (s *Server) handleDownloadInternal(c *fiber.Ctx, packageName, fileName string) error {
	// Try to get from file cache first
	if path, exists := s.fileCache.Get(packageName + "/" + fileName); exists {
		log.Debug().
			Str("package", packageName).
			Str("file", fileName).
			Str("cache_path", path).
			Msg("✅ Serving from file cache")
		return c.SendFile(path)
	}

	// Get package files to find the download URL
	var files []pypi.FileInfo
	if cachedData, found := s.indexCache.GetPackage(packageName); found {
		if cachedFiles, ok := cachedData.([]pypi.FileInfo); ok {
			files = cachedFiles
		}
	}

	if len(files) == 0 {
		// Fetch from PyPI
		var err error
		files, err = s.pypiClient.GetPackageFiles(packageName)
		if err != nil {
			return c.Status(fiber.StatusNotFound).SendString("Package not found")
		}
		// Cache the result
		s.indexCache.SetPackage(packageName, files, s.config.IndexTTL)
	}

	// Find the file URL and size
	var fileURL string
	var fileSize int64
	for _, file := range files {
		if file.Name == fileName {
			fileURL = file.URL
			fileSize = file.Size
			break
		}
	}

	if fileURL == "" {
		return c.Status(fiber.StatusNotFound).SendString("File not found")
	}

	// Build storage key for the file
	storageKey := fmt.Sprintf("packages/%s/%s", packageName, fileName)

	log.Debug().
		Str("package", packageName).
		Str("file", fileName).
		Str("storage_key", storageKey).
		Str("file_url", fileURL).
		Str("storage_type", s.config.StorageType).
		Msg("🔍 Checking if file exists in storage")

	// Check if file exists in storage
	ctx := context.Background()
	exists, err := s.storage.Exists(ctx, storageKey)
	if err != nil {
		log.Error().Err(err).Str("key", storageKey).Msg("Failed to check storage")
	}

	log.Debug().
		Str("storage_key", storageKey).
		Bool("exists_in_storage", exists).
		Msg("💾 Storage existence check result")

	if exists {
		// Serve from storage using zero-copy when possible
		log.Debug().Str("package", packageName).Str("file", fileName).Msg("✅ Serving from storage cache")
		return s.serveFromStorageOptimized(c, storageKey)
	}

	// Check download timeout to decide whether to stream or redirect
	if s.config.DownloadTimeout > 0 {
		// Calculate dynamic timeout based on file size
		dynamicTimeout := s.calculateDynamicTimeout(fileSize)

		// Use streaming downloader for simultaneous download and serve
		downloadCtx, cancel := context.WithTimeout(ctx, dynamicTimeout)
		defer cancel()

		log.Info().
			Str("package", packageName).
			Str("file", fileName).
			Str("file_url", fileURL).
			Int64("file_size", fileSize).
			Dur("timeout", dynamicTimeout).
			Msg("🚀 Starting streaming download with simultaneous cache")

		// Stream to client while caching
		result, err := s.streamDownloader.DownloadAndStream(downloadCtx, fileURL, storageKey, c.Response().BodyWriter())
		if err != nil {
			log.Error().
				Err(err).
				Str("package", packageName).
				Str("file", fileName).
				Str("file_url", fileURL).
				Int64("file_size", fileSize).
				Dur("timeout", dynamicTimeout).
				Msg("Failed to stream download, redirecting to PyPI")

			// Fall back to redirect
			return c.Redirect(fileURL, fiber.StatusFound)
		}

		// Set appropriate headers
		if result.ContentType != "" {
			c.Set("Content-Type", result.ContentType)
		}
		if result.Size > 0 {
			c.Set("Content-Length", fmt.Sprintf("%d", result.Size))
		}
		if result.ETag != "" {
			c.Set("ETag", result.ETag)
		}

		log.Info().
			Str("package", packageName).
			Str("file", fileName).
			Int64("size", result.Size).
			Bool("cached", result.Error == nil).
			Msg("✅ Successfully streamed file to client")

		return nil // Response already written
	} else {
		log.Debug().
			Str("package", packageName).
			Str("file", fileName).
			Msg("Download timeout is 0, redirecting directly to PyPI")
	}

	// Redirect to upstream URL
	return c.Redirect(fileURL, fiber.StatusFound)
}

func (s *Server) handleCacheList(c *fiber.Ctx) error {
	// Invalidate both index and response caches
	s.indexCache.InvalidateList()
	s.responseCache.Invalidate("json:package-list")

	return c.JSON(fiber.Map{
		"status": "success",
		"data":   nil,
	})
}

func (s *Server) handleCacheListMethodNotAllowed(c *fiber.Ctx) error {
	if c.Method() != "DELETE" {
		return c.Status(fiber.StatusMethodNotAllowed).SendString("Method Not Allowed")
	}
	return c.Next()
}

func (s *Server) handleCachePackage(c *fiber.Ctx) error {
	packageName := c.Params("package")

	if packageName == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"status":  "error",
			"message": "Package name required",
		})
	}

	// Invalidate both index and response caches
	s.indexCache.InvalidatePackage(packageName)
	s.responseCache.Invalidate("json:package:" + packageName)

	return c.JSON(fiber.Map{
		"status": "success",
		"data":   nil,
	})
}

func (s *Server) handleHealth(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"status":    "success",
		"timestamp": time.Now().Unix(),
		"data": fiber.Map{
			"cache_dir":         s.config.CacheDir,
			"index_url":         s.config.IndexURL,
			"cache_size":        s.config.CacheSize,
			"index_ttl_seconds": int(s.config.IndexTTL.Seconds()),
			"storage_type":      s.config.StorageType,
		},
	})
}

func wantsJSON(c *fiber.Ctx) bool {
	// Check format query parameter
	if format := c.Query("format"); format != "" {
		return strings.Contains(format, "json")
	}

	// Check Accept header
	accept := c.Get("Accept")
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

	// Default to local storage
	return storage.NewLocalStorage(cfg.CacheDir)
}

// serveFromStorage serves a file from the storage backend
func (s *Server) serveFromStorage(c *fiber.Ctx, storageKey string) error {
	ctx := context.Background()

	log.Debug().
		Str("storage_key", storageKey).
		Str("method", c.Method()).
		Msg("Starting file serve from storage")

	// Get file from storage
	reader, info, err := s.storage.Get(ctx, storageKey)
	if err != nil {
		log.Error().Err(err).Str("key", storageKey).Msg("Failed to get from storage")
		return c.Status(fiber.StatusInternalServerError).SendString("Storage error")
	}
	defer func() { _ = reader.Close() }()

	// Set headers
	if info.ContentType != "" {
		c.Set("Content-Type", info.ContentType)
	} else {
		c.Set("Content-Type", "application/octet-stream")
	}

	if info.Size > 0 {
		c.Set("Content-Length", fmt.Sprintf("%d", info.Size))
	}

	// Extract filename from storage key
	filename := path.Base(storageKey)
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	// Set cache headers for better performance
	c.Set("Cache-Control", "public, max-age=3600")
	c.Set("ETag", fmt.Sprintf(`"%s"`, info.ETag))

	// Handle HEAD requests without reading body
	if c.Method() == "HEAD" {
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
	// This gives us more control over the streaming process
	written, err := io.Copy(c.Response().BodyWriter(), reader)
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

// serveFromStorageOptimized serves a file from storage with zero-copy optimizations when possible
func (s *Server) serveFromStorageOptimized(c *fiber.Ctx, storageKey string) error {
	ctx := context.Background()

	// Try to get local file path for zero-copy operations (local storage only)
	if streamStorage, ok := s.storage.(storage.StreamingStorage); ok && streamStorage.SupportsZeroCopy() {
		if filePath, err := streamStorage.GetFilePath(ctx, storageKey); err == nil {
			// Use Fiber's SendFile for zero-copy local file serving
			log.Debug().
				Str("storage_key", storageKey).
				Str("file_path", filePath).
				Msg("Using zero-copy SendFile")
			return c.SendFile(filePath)
		}
	}

	// Fall back to streaming from storage
	log.Debug().
		Str("storage_key", storageKey).
		Msg("Using streaming from storage backend")

	if streamStorage, ok := s.storage.(storage.StreamingStorage); ok {
		// Use optimized streaming
		info, err := streamStorage.StreamingGet(ctx, storageKey, c.Response().BodyWriter())
		if err != nil {
			log.Error().Err(err).Str("key", storageKey).Msg("Failed to stream from storage")
			return c.Status(fiber.StatusInternalServerError).SendString("Storage error")
		}

		// Set headers
		if info.ContentType != "" {
			c.Set("Content-Type", info.ContentType)
		}
		if info.Size > 0 {
			c.Set("Content-Length", fmt.Sprintf("%d", info.Size))
		}
		if info.ETag != "" {
			c.Set("ETag", fmt.Sprintf("\"%s\"", info.ETag))
		}

		return nil
	}

	// Fall back to regular storage serving
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
