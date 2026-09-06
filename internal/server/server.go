// Package server is the transport: it owns the mux, the middleware, the storage
// backend's construction and the shutdown order. Every route belongs to one of
// the modules it wires together — index, download, admin — and is registered by
// that module.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/huyhandes/groxpi/internal/admin"
	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/download"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/logger"
	"github.com/huyhandes/groxpi/internal/storage"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// Server wires the modules together over one storage backend.
type Server struct {
	config    *config.Config
	storage   storage.Storage
	index     *index.Service
	downloads *download.Service
	admin     *admin.Service // nil when no credentials are configured
	handler   http.Handler
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

	st, err := initStorage(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}

	s := &Server{config: cfg, storage: st}
	s.index = index.New(cfg)
	s.downloads = download.New(cfg, st, s.index)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /health", s.handleHealth)
	s.index.Register(mux)
	s.downloads.Register(mux)

	// With no credentials configured nothing is registered, so every admin path
	// falls through to the mux's 404 — the surface does not exist rather than
	// existing unauthenticated.
	if cfg.AdminConfigured() {
		s.admin, err = admin.New(cfg, st, s.index, s.downloads)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		s.admin.Register(mux)
	}

	// No compression middleware: package files are already-compressed archives and
	// index bodies carry their compressed form in the cache entry.
	s.handler = recoverPanics(traceRequests(mux))
	return s, nil
}

// Router returns the handler to mount on an http.Server.
func (s *Server) Router() http.Handler {
	return s.handler
}

// recoverPanics turns a panicking handler into a 500 instead of a dropped
// connection, and logs the stack so the panic is not lost with it.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				slog.ErrorContext(r.Context(), "Handler panicked", "panic", rec, "stack", string(debug.Stack()))
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// traceRequests opens the root span of every request and puts it on the request
// context, so every span the handlers open hangs off it. An upstream trace is
// continued when the client sent W3C headers. It also writes the access log
// line. It is instrumentation only: it reads the response status and changes
// nothing about it.
func traceRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := telemetry.Tracer().Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		r = r.WithContext(ctx)
		next.ServeHTTP(sw, r)

		// The mux fills in the matched pattern on the request it was handed. The
		// pattern, not the path: a span name per package name would be a
		// cardinality explosion in any backend.
		route := r.Pattern
		if route == "" {
			route = r.Method + " unmatched"
		}
		span.SetName(route)
		span.SetAttributes(attribute.Int("http.response.status_code", sw.status()))

		slog.InfoContext(ctx, "request",
			"status", sw.status(), "latency", time.Since(start), "method", r.Method, "path", r.URL.Path)
	})
}

// statusWriter records the status code for the span and the access log. It
// forwards ReadFrom so http.ServeContent still reaches the standard writer's own
// copy path: wrapping the response writer without it would silently hide that
// path behind the wrapper (there is a test).
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusWriter) ReadFrom(src io.Reader) (int64, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(s.ResponseWriter, src)
}

// Unwrap lets http.ResponseController reach the underlying writer's Flush and
// deadline controls.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}

// closeBudget bounds CloseContext when the caller supplies no deadline of its
// own. The shutdown path in main always does; this is for tests and for any
// caller reaching Server through io.Closer.
const closeBudget = 5 * time.Second

// Close is CloseContext under the default budget, so that Server satisfies
// io.Closer.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeBudget)
	defer cancel()
	return s.CloseContext(ctx)
}

// CloseContext drains the admin module's prefetches, stops the index-cache
// sweeper and releases the storage backend — all inside the caller's deadline.
//
// Ordering matters: releasing the backend under a prefetch still writing to it
// would fail that write, so the drain comes first. So does the bound. Every step
// here can block on something remote — a wedged upstream, an unreachable object
// store — and an unbounded shutdown is not a graceful one: it runs past the
// container's stop grace and ends in SIGKILL, losing more than giving up would.
// Overrunning the deadline is reported and stepped over, not waited out.
func (s *Server) CloseContext(ctx context.Context) error {
	if s.admin != nil {
		s.admin.Close(ctx)
	}
	s.index.Close()

	// Bounded for the same reason: the tiered backend drains queued uploads on
	// close, and an unreachable object store makes that drain the longest step in
	// the shutdown. The step keeps running past the deadline: the process is
	// exiting, and interrupting a half-written upload buys nothing.
	done := make(chan error, 1)
	go func() { done <- s.storage.Close() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		slog.Warn("Shutdown budget spent; no longer waiting for the storage backend to close", "error", ctx.Err())
		return nil
	}
}

func (s *Server) handleHome(w http.ResponseWriter, _ *http.Request) {
	adminLink := ""
	if s.config.AdminConfigured() {
		adminLink = ` | <a href="/admin">Cache admin</a>`
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
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
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "success",
		"timestamp": time.Now().Unix(),
		"data": map[string]any{
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

// initStorage creates the appropriate storage backend based on configuration.
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
