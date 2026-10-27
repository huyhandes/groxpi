// Package server is the transport: it owns the mux, every route and handler, the
// middleware, the admin templates and the shutdown order; storage.New builds the
// storage backend. The index, download, storage and admin modules are plain Go
// services it calls.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/huyhandes/groxpi/internal/admin"
	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/download"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/storage"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// Server wires the modules together over one storage backend.
type Server struct {
	config    *config.Config
	store     *storage.Store
	index     *index.Service
	admin     *admin.Service     // nil when no credentials are configured
	templates *template.Template // admin page; nil without admin
	handler   http.Handler
}

// NewServer builds a server, returning an error rather than producing one that
// cannot be trusted. A misconfigured administrative surface fails here: serving
// an open management panel because a password was missing is worse than not
// starting.
func NewServer(cfg *config.Config) (*Server, error) {
	if cfg.AdminEnabled && !cfg.AdminConfigured() {
		return nil, errors.New("admin interface is enabled but GROXPI_ADMIN_USERNAME and GROXPI_ADMIN_PASSWORD are not both set")
	}

	s := &Server{config: cfg}
	s.index = index.New(cfg)
	store, err := storage.New(cfg, download.New(cfg, s.index))
	if err != nil {
		s.index.Close()
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}
	s.store = store

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /simple/{$}", s.handleListPackages)
	mux.HandleFunc("GET /simple/{package}/{$}", s.handleListFiles)
	mux.HandleFunc("GET /index/{$}", s.handleListPackages)
	mux.HandleFunc("GET /index/{package}", s.handleListFiles)
	mux.HandleFunc("GET /index/{package}/{$}", s.handleListFiles)
	mux.HandleFunc("GET /simple/{package}/{file}", s.handleFile)
	mux.HandleFunc("GET /index/{package}/{file}", s.handleFile)

	// With no credentials configured nothing is registered, so every admin path
	// falls through to the mux's 404 — the surface does not exist rather than
	// existing unauthenticated.
	if cfg.AdminConfigured() {
		s.templates, err = parseAdminTemplates()
		if err != nil {
			s.index.Close()
			_ = s.store.Close()
			return nil, err
		}
		s.admin = admin.New(s.store, s.index)
		s.registerAdmin(mux)
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

	// Bounded for the same reason: the store waits for detached downloads,
	// including each one's upload to the durable store, and an unreachable
	// object store makes that wait the longest step in the shutdown. The step
	// keeps running past the deadline: the process is exiting, and
	// interrupting a half-written upload buys nothing. An upload cut off here is
	// only missing from S3; the file is already in the cache (hybrid) or
	// refetched on the next miss (s3).
	done := make(chan error, 1)
	go func() { done <- s.store.Close() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		slog.Warn("Shutdown budget spent; no longer waiting for the storage backend to close", "error", ctx.Err())
		return nil
	}
}

// handleFile serves a package file through the read-through store: a cached
// object goes to http.ServeContent (ranges, conditionals); a download in flight
// is streamed as it arrives, Range ignored. A miss the store will not or could
// not fetch is redirected upstream.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pkg := r.PathValue("package")
	file := r.PathValue("file")

	obj, err := s.store.Open(ctx, pkg, file)
	if errors.Is(err, storage.ErrInvalidKey) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	if err != nil {
		var re *storage.RedirectError
		switch {
		case errors.As(err, &re):
			s.redirect(w, r, re, pkg, file)
		case errors.Is(err, download.ErrNotListed):
			http.Error(w, "File not found", http.StatusNotFound)
		case errors.Is(err, storage.ErrStorage):
			// The store logged it, once per flight.
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		default:
			slog.DebugContext(ctx, "Package index unavailable",
				"error", config.RedactErrorText(err), "package", pkg, "file", file)
			http.Error(w, "Package not found", http.StatusNotFound)
		}
		return
	}
	defer func() { _ = obj.Close() }()

	h := w.Header()
	h.Set("Content-Type", obj.ContentType)
	if !obj.InFlight {
		// The bare ReadSeekCloser, not obj: on a local hit it is the *os.File the
		// stdlib's copy path looks for, which the Object wrapper would hide.
		http.ServeContent(w, r, obj.Name, obj.ModTime, obj.ReadSeekCloser)
		return
	}
	if obj.Size > 0 {
		h.Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	}
	// HEAD drains too: the response ends once the file is cached.
	var dst io.Writer = w
	if r.Method == http.MethodHead {
		dst = io.Discard
	}
	if _, err := io.Copy(dst, obj); err != nil {
		// The status line is gone; aborting is the only way to tell the client
		// the body is bad rather than hand it a clean end. A cancelled request
		// context means the client left (net/http cancels it when a write to the
		// connection fails): ordinary, so it logs below ERROR.
		if ctx.Err() != nil {
			slog.InfoContext(ctx, "Client disconnected mid-download, aborting response",
				"error", config.RedactErrorText(err), "package", pkg, "file", file)
			panic(http.ErrAbortHandler)
		}
		slog.ErrorContext(ctx, "Download failed mid-stream, aborting response",
			"error", config.RedactErrorText(err), "package", pkg, "file", file)
		panic(http.ErrAbortHandler)
	}
}

// redirect sends the client upstream for a miss the store did not serve, and
// records why. The URL carries no userinfo: a private index in redirect mode
// answers the client 401 rather than the proxy handing out its password. A
// target that did not parse is never handed out.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, re *storage.RedirectError, pkg, file string) {
	ctx := r.Context()
	if re.Reason == telemetry.RedirectFetchFailed {
		slog.ErrorContext(ctx, "Failed to fetch file, redirecting upstream",
			"error", config.RedactErrorText(re.Err), "package", pkg, "file", file,
			"file_url", config.RedactURL(re.URL))
	}
	if re.URL == "" {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	if re.Stripped {
		slog.WarnContext(ctx, "Stripped index credentials from upstream redirect",
			"file_url", config.RedactURL(re.URL))
	}
	telemetry.Redirect(ctx, re.Reason)
	http.Redirect(w, r, re.URL, http.StatusFound)
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
		<li>Version: %s</li>
	</ul>
	<p><a href="/index/">Browse packages</a> | <a href="/health">Health Check</a>%s</p>
</body>
</html>`, config.RedactURL(s.config.IndexURL), s.store.Describe().CacheSize/(1024*1024), s.config.IndexTTL.String(), config.Version, adminLink)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// Storage facts come from the store, which knows the cache it really uses:
	// hybrid's local cache, none in s3.
	d := s.store.Describe()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "success",
		"timestamp": time.Now().Unix(),
		"data": map[string]any{
			"cache_dir": d.CacheDir,
			// Redacted at the call site: an unauthenticated probe must not be able to
			// read an index's credentials out of this payload.
			"index_url":         config.RedactURL(s.config.IndexURL),
			"extra_index_urls":  redactedIndexes(s.config.ExtraIndexURLs),
			"cache_size":        d.CacheSize,
			"index_ttl_seconds": int(s.config.IndexTTL.Seconds()),
			"storage_type":      d.Mode,
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
