package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/index"
)

// handleListPackages proxies the upstream root index byte for byte. It is not
// cached and not decoded: the full project list is tens of megabytes, and every
// representation the client can ask for is one the upstream already produces.
func (s *Server) handleListPackages(w http.ResponseWriter, r *http.Request) {
	// ?format= overrides Accept here exactly as it does on a package page, but
	// neither value is forwarded verbatim: both are attacker-controlled and both
	// are part of the singleflight key, so N distinct spellings would be N
	// concurrent multi-megabyte fetches all resident in memory. They collapse to
	// the two representations and one encoding this route actually asks upstream
	// for, which is the whole set it can serve.
	accept := "text/html"
	if wantsJSON(r) {
		accept = index.JSONContentType
	}
	acceptEncoding := ""
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		acceptEncoding = "gzip"
	}

	root, err := s.index.ProxyRoot(r.Context(), accept, acceptEncoding)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left: nobody to answer
		}
		http.Error(w, "Failed to fetch package list", http.StatusBadGateway)
		return
	}

	h := w.Header()
	h.Set("Vary", "Accept, Accept-Encoding")
	if root.ContentEncoding != "" {
		h.Set("Content-Encoding", root.ContentEncoding)
	}
	if root.ContentType != "" {
		h.Set("Content-Type", root.ContentType)
	}
	w.WriteHeader(root.Status)
	_, _ = w.Write(root.Body)
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	packageName := r.PathValue("package")

	entry, err := s.index.Resolve(r.Context(), packageName)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left: nobody to answer
		}
		// A miss means every configured index was consulted and none had it.
		if errors.Is(err, index.ErrNotFound) {
			http.Error(w, "Package not found", http.StatusNotFound)
			return
		}
		// index logged the failure once, for every waiter.
		http.Error(w, "Error fetching package: "+config.RedactErrorText(err), http.StatusInternalServerError)
		return
	}

	if wantsJSON(r) {
		writeIndexJSON(w, r, entry)
		return
	}
	// The answer depends on Accept, so a shared cache must key on it; the HTML
	// body is never compressed, so Accept-Encoding does not matter here.
	w.Header().Set("Vary", "Accept")
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write(index.RenderSimpleHTML(packageName, entry.Files)) // #nosec G705 -- every value escaped by RenderSimpleHTML
}

// writeIndexJSON serves the body built when the entry was filled, preferring its
// pre-compressed form when the client accepts it. Nothing is compressed on the
// request path.
func writeIndexJSON(w http.ResponseWriter, r *http.Request, entry *index.Entry) {
	h := w.Header()
	h.Set("Vary", "Accept, Accept-Encoding")
	h.Set("Content-Type", index.JSONContentType)
	// ponytail: substring match, not a q-value parse. "gzip;q=0" is rare enough
	// that the parser can wait for a client that actually sends it.
	if len(entry.GZIP) > 0 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		_, _ = w.Write(entry.GZIP) // #nosec G705 -- gzipped JSON, Content-Type set above
		return
	}
	_, _ = w.Write(entry.JSON) // #nosec G705 -- encoding/json output, Content-Type set above
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
