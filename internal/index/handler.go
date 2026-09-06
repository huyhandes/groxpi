package index

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/huyhandes/groxpi/internal/config"
)

// Register mounts the index routes: the root listing and the per-package page,
// under /simple/ (PEP 503) and the /index/ aliases kept for proxpi
// compatibility. Package-file downloads are a different module and register
// their own patterns beside these.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /simple/{$}", s.handleListPackages)
	mux.HandleFunc("GET /simple/{package}/{$}", s.handleListFiles)
	mux.HandleFunc("GET /index/{$}", s.handleListPackages)
	mux.HandleFunc("GET /index/{package}", s.handleListFiles)
}

// handleListPackages proxies the upstream root index byte for byte. It is not
// cached and not decoded: the full project list is tens of megabytes, and every
// representation the client can ask for is one the upstream already produces.
func (s *Service) handleListPackages(w http.ResponseWriter, r *http.Request) {
	// ?format= overrides Accept here exactly as it does on a package page, but
	// neither value is forwarded verbatim: both are attacker-controlled and both
	// are part of the singleflight key, so N distinct spellings would be N
	// concurrent multi-megabyte fetches all resident in memory. They collapse to
	// the two representations and one encoding this route actually asks upstream
	// for, which is the whole set it can serve.
	accept := "text/html"
	if wantsJSON(r) {
		accept = JSONContentType
	}
	acceptEncoding := ""
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		acceptEncoding = "gzip"
	}

	root, err := s.proxyRoot(r.Context(), accept, acceptEncoding)
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to proxy root index", "error", config.RedactErrorText(err))
		http.Error(w, "Failed to fetch package list", http.StatusBadGateway)
		return
	}

	h := w.Header()
	h.Set("Vary", "Accept, Accept-Encoding")
	if root.contentEncoding != "" {
		h.Set("Content-Encoding", root.contentEncoding)
	}
	if root.contentType != "" {
		h.Set("Content-Type", root.contentType)
	}
	w.WriteHeader(root.status)
	_, _ = w.Write(root.body)
}

func (s *Service) handleListFiles(w http.ResponseWriter, r *http.Request) {
	packageName := NormalizeName(r.PathValue("package"))

	entry, err := s.Resolve(r.Context(), packageName)
	if err != nil {
		// A miss means every configured index was consulted and none had it.
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "Package not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(r.Context(), "Failed to fetch package files",
			"error", config.RedactErrorText(err), "package", packageName)
		http.Error(w, "Error fetching package: "+config.RedactErrorText(err), http.StatusInternalServerError)
		return
	}

	if wantsJSON(r) {
		writeIndexJSON(w, r, entry)
		return
	}
	renderPackageFilesHTML(w, packageName, entry.Files)
}

// writeIndexJSON serves the body built when the entry was filled, preferring its
// pre-compressed form when the client accepts it. Nothing is compressed on the
// request path.
func writeIndexJSON(w http.ResponseWriter, r *http.Request, entry *Entry) {
	h := w.Header()
	h.Set("Vary", "Accept-Encoding")
	h.Set("Content-Type", JSONContentType)
	// ponytail: substring match, not a q-value parse. "gzip;q=0" is rare enough
	// that the parser can wait for a client that actually sends it.
	if len(entry.GZIP) > 0 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		_, _ = w.Write(entry.GZIP)
		return
	}
	_, _ = w.Write(entry.JSON)
}

// renderPackageFilesHTML renders the index page from the parsed file list. HTML
// is the uncommon content type, so it is produced on demand rather than stored
// as a third copy per package.
func renderPackageFilesHTML(w http.ResponseWriter, packageName string, files []FileInfo) {
	var sb strings.Builder
	sb.Grow(1024 + len(files)*200)

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
		sb.WriteString(ProxyFileURL(packageName, file.Name))
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
		if hashes, ok := file.Metadata(); ok {
			value := "true"
			if sum := hashes["sha256"]; sum != "" {
				value = "sha256=" + sum
			}
			sb.WriteString(` data-core-metadata="` + value + `" data-dist-info-metadata="` + value + `"`)
		}

		sb.WriteString(`>`)
		sb.WriteString(file.Name)
		sb.WriteString(`</a><br>
`)
	}

	sb.WriteString(`</body>
</html>`)
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(sb.String()))
}

// wantsJSON reports whether the client asked for the PEP 691 representation,
// by ?format= first and the Accept header second.
func wantsJSON(r *http.Request) bool {
	if format := r.URL.Query().Get("format"); format != "" {
		return strings.Contains(format, "json")
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/vnd.pypi.simple") && strings.Contains(accept, "json")
}
