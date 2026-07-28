package server

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/huyhandes/groxpi/internal/cache"
	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/logger"
	"github.com/huyhandes/groxpi/internal/pypi"
	"github.com/huyhandes/groxpi/internal/storage"
	"github.com/huyhandes/groxpi/internal/streaming"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

type Server struct {
	config     *config.Config
	indexCache *cache.IndexCache
	pypiClient *pypi.Client
	storage    storage.Storage
	router     *gin.Engine
	// packageFiles owns index resolution, the package-file miss pipeline and the
	// single singleflight.Group that deduplicates both.
	packageFiles *PackageFileService

	// Administrative surface. Nil templates mean it was never mounted.
	adminTemplates *template.Template
	adminErrors    adminErrors
	// prefetches counts the detached prefetch goroutines so shutdown can wait for
	// them: one of them may be inside storage.Put when the signal arrives.
	prefetches sync.WaitGroup
}

// New builds a server or terminates the process. It is the entry point's
// constructor; anything that needs to observe a construction failure calls
// NewServer.
func New(cfg *config.Config) *Server {
	s, err := NewServer(cfg)
	if err != nil {
		logger.Fatal("Failed to construct server", "error", err)
	}
	return s
}

// NewServer builds a server, returning an error rather than producing one that
// cannot be trusted. A misconfigured administrative surface fails here: serving
// an open management panel because a password was missing is worse than not
// starting.
func NewServer(cfg *config.Config) (*Server, error) {
	if cfg.AdminEnabled && !cfg.AdminConfigured() {
		return nil, errors.New("admin interface is enabled but GROXPI_ADMIN_USERNAME and GROXPI_ADMIN_PASSWORD are not both set")
	}

	// Set Gin mode based on log level
	if cfg.LogLevel == "DEBUG" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// Create Gin router. Let gin answer a known path reached with the wrong
	// method: it responds 405 with an Allow header instead of falling through to
	// the 404 handler.
	router := gin.New()
	router.HandleMethodNotAllowed = true

	// Add middleware
	router.Use(gin.Recovery())
	router.Use(traceRequests())
	router.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("[%s] %d - %v %s %s\n",
			param.TimeStamp.Format(time.RFC3339),
			param.StatusCode,
			param.Latency,
			param.Method,
			param.Path,
		)
	}))

	// No compression middleware: package files are already-compressed archives and
	// index bodies carry their compressed form in the cache entry.

	// Initialize storage backend
	storageBackend, err := initStorage(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
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
		config: cfg,
		// Expired entries are swept at the index TTL cadence: an entry outlives its
		// TTL by at most one interval, and nothing scans on the read path.
		indexCache: cache.NewIndexCache(cfg.IndexCacheSize, cfg.IndexTTL),
		pypiClient: pypi.NewClient(cfg),
		storage:    storageBackend,
		router:     router,
	}

	s.packageFiles = newPackageFileService(
		cfg,
		storageBackend,
		s.indexCache,
		s.pypiClient,
		streaming.NewTeeStreamingDownloader(storageBackend, streamClient),
	)

	if cfg.AdminConfigured() {
		if s.adminTemplates, err = parseAdminTemplates(); err != nil {
			return nil, fmt.Errorf("failed to parse admin templates: %w", err)
		}
	}

	s.setupRoutes()
	return s, nil
}

func (s *Server) Router() *gin.Engine {
	return s.router
}

// traceRequests opens the root span of every request and puts it on the request
// context, so every span the handlers open hangs off it. An upstream trace is
// continued when the client sent W3C headers. It is instrumentation only: it
// reads the response status and changes nothing about it.
func traceRequests() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := otel.GetTextMapPropagator().Extract(c.Request.Context(),
			propagation.HeaderCarrier(c.Request.Header))

		// The route pattern, not the path: a span name per package name would be a
		// cardinality explosion in any backend.
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		ctx, span := telemetry.Tracer().Start(ctx, c.Request.Method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		c.Request = c.Request.WithContext(ctx)
		c.Next()

		span.SetAttributes(attribute.Int("http.response.status_code", c.Writer.Status()))
	}
}

// Close waits for detached prefetches, stops the index-cache sweeper and
// releases the storage backend. Called from the shutdown path: closing the
// backend under a prefetch that is still writing to it would fail that write.
func (s *Server) Close() error {
	s.prefetches.Wait()
	s.indexCache.Close()
	return s.storage.Close()
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

	// Health check
	s.router.GET("/health", s.handleHealth)

	s.setupAdminRoutes()

	// 404 handler
	s.router.NoRoute(func(c *gin.Context) {
		c.String(http.StatusNotFound, "Not Found")
	})
}

// setupAdminRoutes mounts the administrative surface behind basic
// authentication, or not at all.
//
// With no credentials configured nothing is registered, so every path below
// falls through to NoRoute and answers 404 — the surface does not exist rather
// than existing unauthenticated. That deliberately includes the pre-existing
// /cache routes, which were open and are now in the same group: an authenticated
// front door on a building with an open side entrance is not authentication.
// Requiring credentials there is a breaking change against the Python
// implementation's API.
//
// The group covers only these paths. The package index routes pip uses are
// registered above and are never inside it, in any configuration.
func (s *Server) setupAdminRoutes() {
	if !s.config.AdminConfigured() {
		return
	}

	// Basic auth rather than a bearer token: the browser prompts for the
	// credential and resends it on every subsequent request, including the ones
	// the page's interaction library issues, so there is no login form and no
	// token in client-side storage. It travels in cleartext, so a deployment needs
	// a TLS-terminating proxy.
	admin := s.router.Group("", rejectCrossSite, gin.BasicAuth(gin.Accounts{
		s.config.AdminUsername: s.config.AdminPassword,
	}))

	admin.GET("/admin", s.handleAdminPage)
	admin.GET("/admin/rows", s.handleAdminRows)
	admin.GET("/admin/htmx.min.js", s.handleAdminAsset)
	admin.POST("/admin/prefetch", s.handleAdminPrefetch)

	// Eviction is the pre-existing cache route: the page's Evict button issues the
	// same DELETE an operator can curl.
	admin.DELETE("/cache/list", s.handleCacheList)
	admin.DELETE("/cache/:package", s.handleCachePackage)
}

// rejectCrossSite refuses admin requests that the browser itself reports as
// originating from another site, so a form on an attacker's page cannot ride the
// operator's cached basic-auth credentials.
//
// ponytail: the header is the whole defence — no token, no session, no cookie.
// When Sec-Fetch-Site is absent the request proceeds: that covers curl, scripts
// and browsers too old to send it. Deliberate, and documented as a limitation —
// every browser able to mount the attack sends the header.
func rejectCrossSite(c *gin.Context) {
	switch c.GetHeader("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		c.Next()
	default: // cross-site, same-site
		c.String(http.StatusForbidden, "Forbidden: cross-site request")
		c.Abort()
	}
}

func (s *Server) handleHome(c *gin.Context) {
	adminLink := ""
	if s.config.AdminConfigured() {
		adminLink = ` | <a href="/admin">Cache admin</a>`
	}

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
	<p><a href="/index/">Browse packages</a> | <a href="/health">Health Check</a>%s</p>
</body>
</html>`, config.RedactURL(s.config.IndexURL), s.config.CacheSize/(1024*1024), s.config.IndexTTL.String(), adminLink)

	c.Header("Content-Type", "text/html")
	c.String(http.StatusOK, html)
}

// handleListPackages proxies the upstream root index byte for byte. It is not
// cached and not decoded: the full project list is tens of megabytes, and every
// representation the client can ask for is one the upstream already produces.
func (s *Server) handleListPackages(c *gin.Context) {
	// ?format= overrides Accept here exactly as it does on a package page, but
	// neither value is forwarded verbatim: both are attacker-controlled and both
	// are part of the singleflight key, so N distinct spellings would be N
	// concurrent multi-megabyte fetches all resident in memory. They collapse to
	// the two representations and one encoding this route actually asks upstream
	// for, which is the whole set it can serve.
	accept := "text/html"
	if wantsJSON(c) {
		accept = jsonContentType
	}
	acceptEncoding := ""
	if strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
		acceptEncoding = "gzip"
	}

	root, err := s.packageFiles.ProxyRoot(c.Request.Context(), accept, acceptEncoding)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "Failed to proxy root index", "error", redactErrorText(err))
		c.String(http.StatusBadGateway, "Failed to fetch package list")
		return
	}

	c.Header("Vary", "Accept, Accept-Encoding")
	if root.contentEncoding != "" {
		c.Header("Content-Encoding", root.contentEncoding)
	}
	c.Data(root.status, root.contentType, root.body)
}

func (s *Server) handleListFiles(c *gin.Context) {
	packageName := pypi.NormalizeName(c.Param("package"))

	// One index-resolution path, shared with the download handler: cache lookup,
	// deduplicated upstream fetch, cache fill.
	entry, err := s.packageFiles.resolveIndex(c.Request.Context(), packageName)
	if err != nil {
		// A miss means every configured index was consulted and none had it.
		if errors.Is(err, pypi.ErrNotFound) {
			c.String(http.StatusNotFound, "Package not found")
			return
		}
		slog.ErrorContext(c.Request.Context(), "Failed to fetch package files",
			"error", redactErrorText(err), "package", packageName)
		c.String(http.StatusInternalServerError, "Error fetching package: "+redactErrorText(err))
		return
	}

	if wantsJSON(c) {
		writeIndexJSON(c, entry)
		return
	}
	renderPackageFilesHTML(c, packageName, entry.Files)
}

// writeIndexJSON serves the body built when the entry was filled, preferring its
// pre-compressed form when the client accepts it. Nothing is compressed on the
// request path.
func writeIndexJSON(c *gin.Context, entry *cache.Entry) {
	c.Header("Vary", "Accept-Encoding")
	// ponytail: substring match, not a q-value parse. "gzip;q=0" is rare enough
	// that the parser can wait for a client that actually sends it.
	if len(entry.GZIP) > 0 && strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
		c.Header("Content-Encoding", "gzip")
		c.Data(http.StatusOK, jsonContentType, entry.GZIP)
		return
	}
	c.Data(http.StatusOK, jsonContentType, entry.JSON)
}

// renderPackageFilesHTML renders the index page from the parsed file list. HTML
// is the uncommon content type, so it is produced on demand rather than stored
// as a third copy per package.
func renderPackageFilesHTML(c *gin.Context, packageName string, files []pypi.FileInfo) {
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
		sb.WriteString(proxyFileURL(packageName, file.Name))
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
	packageName := pypi.NormalizeName(c.Param("package"))
	fileName := c.Param("file")

	slog.DebugContext(c.Request.Context(), "📦 File download request received",
		"package", packageName,
		"file", fileName,
		"user_agent", c.GetHeader("User-Agent"),
		"client_ip", c.ClientIP())

	plan, err := s.packageFiles.Plan(c.Request.Context(), packageName, fileName)
	if err != nil {
		slog.DebugContext(c.Request.Context(), "Package index unavailable",
			"error", redactErrorText(err), "package", packageName, "file", fileName)
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
		if err := s.serveFromStorage(c, plan.StorageKey); err != nil {
			slog.ErrorContext(c.Request.Context(), "Failed to serve from storage",
				"error", err, "storage_key", plan.StorageKey)
			// Only a failure that happened before the first body byte can still
			// be reported; anything later would append garbage to the payload.
			if !c.Writer.Written() {
				c.String(http.StatusInternalServerError, "Failed to serve file")
			}
		}
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
		// Both the message and the URL are redacted: a file URL resolved against a
		// credentialed index carries that index's password, and the downloader's
		// error string embeds the URL it failed on.
		slog.ErrorContext(c.Request.Context(), "Failed to stream download, redirecting to PyPI",
			"error", redactErrorText(err),
			"package", plan.PackageName,
			"file", plan.FileName,
			"file_url", config.RedactURL(plan.URL),
			"file_size", plan.Size,
			"timeout", plan.Timeout)
		if body.wrote {
			// The body is already partly on the wire; a redirect would corrupt it.
			c.Abort()
			return
		}
		telemetry.Redirect(c.Request.Context(), telemetry.RedirectFetchFailed)
		c.Redirect(http.StatusFound, plan.URL)
	case !led:
		// The leader's download populated the cache; the service decides whether
		// this request can now be served from it or has to go upstream.
		s.servePlan(c, s.packageFiles.PlanAfterFetch(c.Request.Context(), plan))
	default:
		slog.InfoContext(c.Request.Context(), "✅ Successfully streamed file to client",
			"package", plan.PackageName,
			"file", plan.FileName,
			"size", result.Size,
			"cached", result.Error == nil)
	}
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

// handleCacheList is a no-op kept for API compatibility with the Python
// implementation: the root index is proxied, so there is no cached list to
// invalidate.
func (s *Server) handleCacheList(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"data":   nil,
	})
}

func (s *Server) handleCachePackage(c *gin.Context) {
	packageName := pypi.NormalizeName(c.Param("package"))

	if packageName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status":  "error",
			"message": "Package name required",
		})
		return
	}

	s.indexCache.InvalidatePackage(packageName)

	// Dropping the index entry alone would report success while leaving every
	// cached file on disk. Deleting goes through the cache's own path, keyed off
	// the package prefix every storage key already carries.
	if deleter, ok := s.storage.(storage.PrefixDeleter); ok {
		deleted, err := deleter.DeletePrefix(c.Request.Context(), storageKeyFor(packageName, ""))
		if err != nil {
			slog.ErrorContext(c.Request.Context(), "Failed to delete cached files",
				"error", err,
				"package", packageName)
			c.JSON(http.StatusInternalServerError, gin.H{
				"status":  "error",
				"message": "Failed to delete cached files",
			})
			return
		}
		slog.InfoContext(c.Request.Context(), "Evicted package from cache",
			"package", packageName,
			"files_deleted", deleted)
	}

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
			"cache_dir": s.config.CacheDir,
			// Redacted at the call site: an unauthenticated probe must not be able to
			// read an index's credentials out of this payload.
			"index_url":         config.RedactURL(s.config.IndexURL),
			"extra_index_urls":  redactedIndexes(s.config.ExtraIndexURLs),
			"cache_size":        s.config.CacheSize,
			"index_ttl_seconds": int(s.config.IndexTTL.Seconds()),
			"storage_type":      s.config.StorageType,
		},
	})
}

// redactedIndexes renders a configured index list for display. Always non-nil so
// the health payload carries an empty array rather than a null.
func redactedIndexes(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		out = append(out, config.RedactURL(u))
	}
	return out
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

// initStorage creates the appropriate storage backend based on configuration
func initStorage(cfg *config.Config) (storage.Storage, error) {
	// Both S3-backed modes take the same client configuration; build it once.
	s3Config := &storage.S3Config{
		Endpoint:        cfg.S3Endpoint,
		AccessKeyID:     cfg.S3AccessKeyID,
		SecretAccessKey: cfg.S3SecretAccessKey,
		Region:          cfg.S3Region,
		Bucket:          cfg.S3Bucket,
		Prefix:          cfg.S3Prefix,
		UseSSL:          cfg.S3UseSSL,
		ForcePathStyle:  cfg.S3ForcePathStyle,
		EnableHTTP2:     cfg.S3EnableHTTP2,
		ConnectTimeout:  cfg.ConnectTimeout,
		RequestTimeout:  cfg.DownloadTimeout,
	}

	switch cfg.StorageType {
	case "hybrid":
		// Hybrid/tiered storage with a local L1 cache and S3 as L2.
		return storage.NewTieredStorage(&storage.TieredConfig{
			LocalCacheDir:  cfg.LocalCacheDir,
			LocalCacheSize: cfg.LocalCacheSize,
			LocalCacheTTL:  cfg.LocalCacheTTL,
			S3Config:       s3Config,
			SyncWorkers:    cfg.TieredSyncWorkers,
			SyncQueueSize:  cfg.TieredSyncQueueSize,
		})
	case "s3":
		return storage.NewS3Storage(s3Config)
	default:
		// Local storage with LRU eviction (no TTL for non-hybrid mode).
		return storage.NewLRULocalStorage(cfg.CacheDir, cfg.CacheSize, 0)
	}
}

// serveFromStorage serves a cached object. When the backend can name a real file
// on disk the path is handed to gin's c.File so net/http does the serving, which
// buys range and If-Modified-Since handling for free. Backends that cannot name a
// file are opened and streamed here.
//
// Serving by path is not a kernel-level zero copy: gin's writer exposes neither
// the file nor the ReaderFrom hooks net/http needs to skip user space, so the
// bytes still pass through it. The win is delegated correctness, not a saved
// copy.
func (s *Server) serveFromStorage(c *gin.Context, storageKey string) error {
	// Read-only serving: it is correct to abandon it when the client goes away.
	ctx := c.Request.Context()

	// Zero-copy is a capability, not a property of every backend: ask, do not
	// assume, and do not branch on a boolean the backend has to lie about.
	if zeroCopy, ok := s.storage.(storage.ZeroCopyCapable); ok {
		if filePath, err := zeroCopy.GetFilePath(ctx, storageKey); err == nil {
			slog.DebugContext(ctx, "Serving local file by path via net/http",
				"storage_key", storageKey,
				"file_path", filePath)
			c.File(filePath)
			return nil
		}
	}

	slog.DebugContext(ctx, "Starting file serve from storage",
		"storage_key", storageKey,
		"method", c.Request.Method)

	// Open first: the reader and the metadata both arrive before a single body
	// byte is written, so every header below is still settable.
	reader, info, err := s.storage.Get(ctx, storageKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			slog.DebugContext(ctx, "Object missing from storage", "key", storageKey)
			c.String(http.StatusNotFound, "File not found")
			return nil
		}
		slog.ErrorContext(ctx, "Failed to get from storage", "error", err, "key", storageKey)
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
	if etag := quoteETag(info.ETag); etag != "" {
		c.Header("ETag", etag)
	}

	slog.DebugContext(ctx, "Starting file stream from storage", "storage_key", storageKey, "size", info.Size)

	// Use io.Copy to manually stream the file to the response writer
	// c.Writer is safe for concurrent use (unlike Fiber's context)
	written, err := io.Copy(c.Writer, reader)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to stream file from storage",
			"error", err,
			"storage_key", storageKey,
			"bytes_written", written)
		return err
	}

	slog.DebugContext(ctx, "File stream completed successfully",
		"storage_key", storageKey,
		"bytes_written", written)

	return nil
}
