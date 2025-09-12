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
)

// Response buffer pool for reducing allocations
var responseBufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

type Server struct {
	config        *config.Config
	indexCache    *cache.IndexCache
	fileCache     *cache.FileCache
	responseCache *cache.ResponseCache
	pypiClient    *pypi.Client
	storage       storage.Storage
	app           *fiber.App
	sf            singleflight.Group // For deduplicating concurrent requests
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

	s := &Server{
		config:        cfg,
		indexCache:    cache.NewIndexCache(),
		fileCache:     cache.NewFileCache(cfg.CacheDir, cfg.CacheSize),
		responseCache: cache.NewResponseCache(50 * 1024 * 1024), // 50MB response cache
		pypiClient:    pypi.NewClient(cfg),
		storage:       storageBackend,
		app:           app,
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
		// Make a copy for cache since buf will be reused
		cachedData := make([]byte, len(jsonData))
		copy(cachedData, jsonData)
		s.responseCache.Set(cacheKey, cachedData, s.config.IndexTTL)

		c.Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		return c.Send(jsonData)
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
			fileMap["url"] = file.URL

			if file.Hashes != nil && len(file.Hashes) > 0 {
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
		// Make a copy for cache since buf will be reused
		cachedData := make([]byte, len(jsonData))
		copy(cachedData, jsonData)
		s.responseCache.Set(cacheKey, cachedData, s.config.IndexTTL)

		c.Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		return c.Send(jsonData)
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
		sb.WriteString(file.URL)
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

	// Normalize package name
	packageName = normalizePackageName(packageName)

	// Try to get from cache
	if path, exists := s.fileCache.Get(packageName + "/" + fileName); exists {
		// Serve from cache
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

	// Find the file URL
	var fileURL string
	for _, file := range files {
		if file.Name == fileName {
			fileURL = file.URL
			break
		}
	}

	if fileURL == "" {
		return c.Status(fiber.StatusNotFound).SendString("File not found")
	}

	// Build storage key for the file
	storageKey := fmt.Sprintf("packages/%s/%s", packageName, fileName)

	// Check if file exists in storage
	ctx := context.Background()
	exists, err := s.storage.Exists(ctx, storageKey)
	if err != nil {
		log.Error().Err(err).Str("key", storageKey).Msg("Failed to check storage")
	}

	if exists {
		// Serve from storage
		log.Debug().Str("package", packageName).Str("file", fileName).Msg("Serving from storage cache")
		return s.serveFromStorage(c, storageKey)
	}

	// Check download timeout to decide whether to download or redirect
	if s.config.DownloadTimeout > 0 {
		// Try to download and cache within timeout
		downloadCtx, cancel := context.WithTimeout(ctx, s.config.DownloadTimeout)
		defer cancel()

		if err := s.downloadAndCache(downloadCtx, fileURL, storageKey); err == nil {
			log.Info().Str("package", packageName).Str("file", fileName).Msg("Downloaded and cached file")
			return s.serveFromStorage(c, storageKey)
		}
		log.Warn().Err(err).Str("package", packageName).Msg("Download timeout, redirecting to PyPI")
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
		"status": "success",
		"data": fiber.Map{
			"cache_dir": s.config.CacheDir,
			"index_url": s.config.IndexURL,
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
		})
	}

	// Default to local storage
	return storage.NewLocalStorage(cfg.CacheDir)
}

// downloadAndCache downloads a file from URL and stores it in storage
func (s *Server) downloadAndCache(ctx context.Context, fileURL, storageKey string) error {
	// Download file from PyPI
	req, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// Read the content
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	// Store in storage backend
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	_, err = s.storage.Put(ctx, storageKey, bytes.NewReader(data), int64(len(data)), contentType)
	if err != nil {
		return fmt.Errorf("failed to store in storage: %w", err)
	}

	return nil
}

// serveFromStorage serves a file from the storage backend
func (s *Server) serveFromStorage(c *fiber.Ctx, storageKey string) error {
	ctx := context.Background()

	// Get file from storage
	reader, info, err := s.storage.Get(ctx, storageKey)
	if err != nil {
		log.Error().Err(err).Str("key", storageKey).Msg("Failed to get from storage")
		return c.Status(fiber.StatusInternalServerError).SendString("Storage error")
	}
	defer reader.Close()

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

	// Stream the file
	return c.SendStream(reader)
}
