package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/huyhandes/groxpi/internal/admin"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/storage"
	tmpl "github.com/huyhandes/groxpi/templates"
)

// parseAdminTemplates parses the embedded admin page. A parse failure is a
// broken build, not a runtime condition, so it fails construction rather than
// every request.
func parseAdminTemplates() (*template.Template, error) {
	t, err := template.New("admin").Funcs(template.FuncMap{
		"bytes": humanBytes,
	}).ParseFS(tmpl.FS, "*.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse admin templates: %w", err)
	}
	return t, nil
}

// registerAdmin mounts the administrative surface behind basic authentication.
//
// That deliberately includes the pre-existing /cache routes, which were open in
// the Python implementation: an authenticated front door on a building with an
// open side entrance is not authentication. The package index routes pip uses
// are never behind this check.
func (s *Server) registerAdmin(mux *http.ServeMux) {
	// Basic auth rather than a bearer token: the browser prompts for the
	// credential and resends it on every subsequent request, including the ones
	// the page's interaction library issues, so there is no login form and no
	// token in client-side storage. It travels in cleartext, so a deployment needs
	// a TLS-terminating proxy.
	guard := func(h http.HandlerFunc) http.Handler { return rejectCrossSite(s.basicAuth(h)) }

	mux.Handle("GET /admin", guard(s.handleAdminPage))
	mux.Handle("GET /admin/rows", guard(s.handleAdminRows))
	mux.Handle("GET /admin/inflight", guard(s.handleAdminInflight))
	mux.Handle("GET /admin/htmx.min.js", guard(handleAdminAsset))
	mux.Handle("POST /admin/prefetch", guard(s.handleAdminPrefetch))

	// Eviction is the pre-existing cache route: the page's Evict button issues the
	// same DELETE an operator can curl. Python proxpi's DELETE /cache/list
	// (invalidate the root list) needs no route of its own: the root index is
	// proxied, so there is nothing to invalidate, and the package route evicts a
	// package named "list" instead of shadowing it.
	mux.Handle("DELETE /cache/{package}", guard(s.handleCachePackage))
}

// basicAuth answers 401 unless the request carries the configured credential.
// Both comparisons are constant-time so a wrong username and a wrong password
// take the same time to reject.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.config.AdminUsername)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.config.AdminPassword)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="groxpi admin"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rejectCrossSite refuses admin requests that the browser itself reports as
// originating from another site, so a form on an attacker's page cannot ride the
// operator's cached basic-auth credentials. It runs before the credential check,
// so a cross-site request is refused without one.
//
// ponytail: absent Sec-Fetch-Site proceeds — see docs/api-endpoints.md.
func rejectCrossSite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
			next.ServeHTTP(w, r)
		default: // cross-site, same-site
			http.Error(w, "Forbidden: cross-site request", http.StatusForbidden)
		}
	})
}

func (s *Server) handleCachePackage(w http.ResponseWriter, r *http.Request) {
	// The mux never matches an empty {package}, so there is no empty-name case.
	packageName := r.PathValue("package")
	err := s.admin.Evict(r.Context(), packageName)
	if errors.Is(err, storage.ErrInvalidKey) {
		writeStatus(w, http.StatusBadRequest, `{"status":"error","message":"Invalid package name"}`)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to delete cached files", "error", err, "package", packageName)
		writeStatus(w, http.StatusInternalServerError, `{"status":"error","message":"Failed to delete cached files"}`)
		return
	}
	writeStatus(w, http.StatusOK, `{"status":"success","data":null}`)
}

// writeStatus writes one of the fixed JSON bodies above. They are literals
// because none of them carries a value that varies.
func writeStatus(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// handleAdminPrefetch accepts a prefetch and returns immediately; the admin
// module runs it detached from the request.
func (s *Server) handleAdminPrefetch(w http.ResponseWriter, r *http.Request) {
	packageName := strings.TrimSpace(r.PostFormValue("package"))
	if packageName == "" {
		http.Error(w, "Package name required", http.StatusBadRequest)
		return
	}

	if err := s.admin.Prefetch(r.Context(), packageName); errors.Is(err, admin.ErrShuttingDown) {
		http.Error(w, "Shutting down; prefetch not accepted", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("Prefetching " + index.NormalizeName(packageName))) // #nosec G705 -- normalised name in a text/plain body
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	view := s.listView(r)
	view.Downloading = s.inFlight(view.Query)
	s.render(w, r, "admin.html", view)
}

// handleAdminRows serves the package table fragment. It walks the cache, so the
// page reloads it on a filter change, a refresh, or a finished download, never
// on a timer.
func (s *Server) handleAdminRows(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "rows", s.listView(r))
}

// handleAdminInflight serves the fragment the page polls: in-flight downloads
// and recent failures, both in memory, so the poll never walks the cache. The
// page sends the cache generation its package table was rendered at; a
// different one now means a file landed or went, however briefly its download
// was in flight, so the response tells the page to reload the package table.
func (s *Server) handleAdminInflight(w http.ResponseWriter, r *http.Request) {
	view := admin.View{Errors: s.admin.Failures(), Query: admin.NormalizeQuery(r.URL.Query().Get("q"))}
	view.Downloading = s.inFlight(view.Query)
	if gen, err := strconv.ParseUint(r.URL.Query().Get("gen"), 10, 64); err == nil && gen != s.admin.Generation() {
		w.Header().Set("HX-Trigger", "cache-changed")
	}
	s.render(w, r, "inflight", view)
}

// inFlight is the downloads running now whose package matches the filter.
func (s *Server) inFlight(query string) []admin.Download {
	var out []admin.Download
	for _, d := range s.admin.InFlight() {
		if strings.Contains(d.Package, query) {
			out = append(out, d)
		}
	}
	return out
}

// listView is the requested page of the cache listing.
func (s *Server) listView(r *http.Request) admin.View {
	// A missing or malformed page is the first page, not an error.
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	return s.admin.List(r.Context(), page, r.URL.Query().Get("q"))
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, view admin.View) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Rendered into a buffer first so a template failure can still be a 500
	// instead of a half page with a 200 already committed.
	var buf strings.Builder
	if err := s.templates.ExecuteTemplate(&buf, name, view); err != nil {
		slog.ErrorContext(r.Context(), "Failed to render admin page", "error", err, "template", name)
		http.Error(w, "Failed to render page", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(buf.String()))
}

// handleAdminAsset serves the embedded interaction library. It is inside the
// authenticated group so the page and its script share one credential prompt.
func handleAdminAsset(w http.ResponseWriter, _ *http.Request) {
	body, err := tmpl.FS.ReadFile("htmx.min.js")
	if err != nil {
		http.Error(w, "asset unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(body)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, units := float64(n), []string{"KB", "MB", "GB", "TB", "PB"}
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f EB", value/unit)
}
