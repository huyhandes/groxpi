package download

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strconv"
	"sync"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/storage"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// Register mounts the package-file route under /simple/ and its /index/ alias.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /simple/{package}/{file}", s.handleDownload)
	mux.HandleFunc("GET /index/{package}/{file}", s.handleDownload)
}

func (s *Service) handleDownload(w http.ResponseWriter, r *http.Request) {
	packageName := index.NormalizeName(r.PathValue("package"))
	fileName := r.PathValue("file")

	slog.DebugContext(r.Context(), "📦 File download request received",
		"package", packageName,
		"file", fileName,
		"user_agent", r.UserAgent(),
		"client_ip", r.RemoteAddr)

	plan, err := s.Plan(r.Context(), packageName, fileName)
	if err != nil {
		slog.DebugContext(r.Context(), "Package index unavailable",
			"error", config.RedactErrorText(err), "package", packageName, "file", fileName)
		http.Error(w, "Package not found", http.StatusNotFound)
		return
	}

	s.servePlan(w, r, plan)
}

// servePlan translates a ServePlan into an HTTP response. It makes no policy
// decisions of its own.
func (s *Service) servePlan(w http.ResponseWriter, r *http.Request, plan ServePlan) {
	switch plan.Action {
	case ActionFromStorage:
		s.serveFromStorage(w, r, plan)
	case ActionStreamAndCache:
		s.streamAndCache(w, r, plan)
	case ActionRedirect:
		http.Redirect(w, r, plan.URL, http.StatusFound)
	default:
		http.Error(w, "File not found", http.StatusNotFound)
	}
}

// serveFromStorage serves a cached object. When the backend can name a real file
// on disk it is handed to http.ServeContent, which does range and conditional
// requests and, on a plain net/http response writer, lets the kernel copy the
// file to the socket. Backends that cannot name a file are opened and streamed.
func (s *Service) serveFromStorage(w http.ResponseWriter, r *http.Request, plan ServePlan) {
	// Read-only serving: it is correct to abandon it when the client goes away.
	ctx := r.Context()
	key := plan.StorageKey

	// Zero-copy is a capability, not a property of every backend: ask, do not
	// assume, and do not branch on a boolean the backend has to lie about.
	if zeroCopy, ok := s.storage.(storage.ZeroCopyCapable); ok {
		if filePath, err := zeroCopy.GetFilePath(ctx, key); err == nil {
			if f, err := os.Open(filePath); err == nil {
				defer func() { _ = f.Close() }()
				if st, err := f.Stat(); err == nil {
					slog.DebugContext(ctx, "Serving local file via http.ServeContent",
						"storage_key", key, "file_path", filePath)
					w.Header().Set("Content-Type", contentTypeForFile(plan.FileName))
					http.ServeContent(w, r, plan.FileName, st.ModTime(), f)
					return
				}
			}
		}
	}

	// Open first: the reader and the metadata both arrive before a single body
	// byte is written, so every header below is still settable.
	reader, info, err := s.storage.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			slog.DebugContext(ctx, "Object missing from storage", "key", key)
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "Failed to get from storage", "error", err, "key", key)
		http.Error(w, "Storage error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = reader.Close() }()

	h := w.Header()
	if info.ContentType != "" {
		h.Set("Content-Type", info.ContentType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	if info.Size > 0 {
		h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	}
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, path.Base(key)))
	h.Set("Cache-Control", "public, max-age=3600")
	if etag := quoteETag(info.ETag); etag != "" {
		h.Set("ETag", etag)
	}

	if written, err := io.Copy(w, reader); err != nil {
		// Only a failure before the first body byte can still be reported;
		// anything later would append garbage to the payload.
		slog.ErrorContext(ctx, "Failed to stream file from storage",
			"error", err, "storage_key", key, "bytes_written", written)
		if written == 0 {
			http.Error(w, "Failed to serve file", http.StatusInternalServerError)
		}
	}
}

// streamAndCache streams the upstream file to the client while it is cached.
// Concurrent requests for the same file are deduplicated inside Fetch: only one
// of them streams, the rest are served the freshly cached object.
func (s *Service) streamAndCache(w http.ResponseWriter, r *http.Request, plan ServePlan) {
	// Headers are emitted lazily, immediately before the first body byte, because
	// net/http commits them on the first Write and ignores anything set later.
	body := &headerWriter{w: w, header: func() { applyDownloadHeaders(w, plan) }}

	result, led, err := s.Fetch(r.Context(), plan, body)

	switch {
	case err != nil:
		// Both the message and the URL are redacted: a file URL resolved against a
		// credentialed index carries that index's password, and the downloader's
		// error string embeds the URL it failed on.
		slog.ErrorContext(r.Context(), "Failed to stream download, redirecting to PyPI",
			"error", config.RedactErrorText(err),
			"package", plan.PackageName,
			"file", plan.FileName,
			"file_url", config.RedactURL(plan.URL),
			"file_size", plan.Size,
			"timeout", plan.Timeout)
		if body.wrote {
			// The body is already partly on the wire; a redirect would corrupt it.
			return
		}
		telemetry.Redirect(r.Context(), telemetry.RedirectFetchFailed)
		http.Redirect(w, r, plan.URL, http.StatusFound)
	case !led:
		// The leader's download populated the cache; the service decides whether
		// this request can now be served from it or has to go upstream.
		s.servePlan(w, r, s.PlanAfterFetch(r.Context(), plan))
	default:
		slog.InfoContext(r.Context(), "✅ Successfully streamed file to client",
			"package", plan.PackageName,
			"file", plan.FileName,
			"size", result.Size,
			"cached", result.Error == nil)
	}
}

// applyDownloadHeaders sets everything knowable about the response before the
// first byte of the body is written.
func applyDownloadHeaders(w http.ResponseWriter, plan ServePlan) {
	h := w.Header()
	if plan.ContentType != "" {
		h.Set("Content-Type", plan.ContentType)
	}
	if plan.Size > 0 {
		h.Set("Content-Length", strconv.FormatInt(plan.Size, 10))
	}
	if plan.ETag != "" {
		h.Set("ETag", plan.ETag)
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
