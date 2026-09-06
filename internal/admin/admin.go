// Package admin is the operator surface: the cache page, prefetch, eviction and
// the credential check in front of them. It is mounted only when credentials are
// configured; otherwise none of its routes exist.
package admin

import (
	"context"
	"crypto/subtle"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/download"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/storage"
	tmpl "github.com/huyhandes/groxpi/templates"
)

// cacheSnapshotter is the listing's data source: the local cache's own
// in-memory index. It is a capability, asked for with a type assertion, because
// only a backend holding real files knows its contents. Nothing here lists the
// object store — the storage listing operation was deleted and stays deleted.
type cacheSnapshotter interface {
	Snapshot() []storage.LRUEntry
}

// Service is the admin module.
type Service struct {
	username, password string
	storage            storage.Storage
	index              *index.Service
	downloads          *download.Service
	templates          *template.Template
	errors             adminErrors

	// prefetches counts the detached prefetch goroutines so shutdown can wait for
	// them: one of them may be inside storage.Put when the signal arrives.
	//
	// prefetchMu guards shuttingDown together with the Add it gates. An Add that
	// lands after Wait has begun is documented WaitGroup misuse, and a handler
	// still parked reading its request body when shutdown starts could otherwise
	// do exactly that: http.Server.Shutdown reports its timeout but does not kill
	// the handler.
	prefetches   sync.WaitGroup
	prefetchMu   sync.Mutex
	shuttingDown bool
	prefetchSF   singleflight.Group
}

// New builds the admin module. A template parse failure is a broken build, not
// a runtime condition, so it fails construction rather than every request.
func New(cfg *config.Config, st storage.Storage, idx *index.Service, dl *download.Service) (*Service, error) {
	t, err := template.New("admin").Funcs(template.FuncMap{
		"bytes": humanBytes,
	}).ParseFS(tmpl.FS, "*.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse admin templates: %w", err)
	}
	return &Service{
		username:  cfg.AdminUsername,
		password:  cfg.AdminPassword,
		storage:   st,
		index:     idx,
		downloads: dl,
		templates: t,
	}, nil
}

// Register mounts the administrative surface behind basic authentication.
//
// That deliberately includes the pre-existing /cache routes, which were open in
// the Python implementation: an authenticated front door on a building with an
// open side entrance is not authentication. The package index routes pip uses
// are registered by other modules and are never behind this check.
func (s *Service) Register(mux *http.ServeMux) {
	// Basic auth rather than a bearer token: the browser prompts for the
	// credential and resends it on every subsequent request, including the ones
	// the page's interaction library issues, so there is no login form and no
	// token in client-side storage. It travels in cleartext, so a deployment needs
	// a TLS-terminating proxy.
	guard := func(h http.HandlerFunc) http.Handler { return rejectCrossSite(s.basicAuth(h)) }

	mux.Handle("GET /admin", guard(s.handleAdminPage))
	mux.Handle("GET /admin/rows", guard(s.handleAdminRows))
	mux.Handle("GET /admin/htmx.min.js", guard(s.handleAdminAsset))
	mux.Handle("POST /admin/prefetch", guard(s.handleAdminPrefetch))

	// Eviction is the pre-existing cache route: the page's Evict button issues the
	// same DELETE an operator can curl.
	mux.Handle("DELETE /cache/list", guard(s.handleCacheList))
	mux.Handle("DELETE /cache/{package}", guard(s.handleCachePackage))
}

// basicAuth answers 401 unless the request carries the configured credential.
// Both comparisons are constant-time so a wrong username and a wrong password
// take the same time to reject.
func (s *Service) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.password)) == 1
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

// Close refuses further prefetches and waits for the ones already running, or
// gives up when ctx expires. A prefetch abandoned here loses at most a cache
// entry the next request re-fetches.
func (s *Service) Close(ctx context.Context) {
	// Refusing first is what makes the Wait below safe: no Add can follow it.
	s.prefetchMu.Lock()
	s.shuttingDown = true
	s.prefetchMu.Unlock()

	done := make(chan struct{})
	go func() { s.prefetches.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("Shutdown budget spent; no longer waiting for prefetches to finish", "error", ctx.Err())
	}
}

// handleCacheList is a no-op kept for API compatibility with the Python
// implementation: the root index is proxied, so there is no cached list to
// invalidate.
func (s *Service) handleCacheList(w http.ResponseWriter, _ *http.Request) {
	writeStatus(w, http.StatusOK, `{"status":"success","data":null}`)
}

func (s *Service) handleCachePackage(w http.ResponseWriter, r *http.Request) {
	packageName := index.NormalizeName(r.PathValue("package"))
	if packageName == "" {
		writeStatus(w, http.StatusBadRequest, `{"status":"error","message":"Package name required"}`)
		return
	}

	s.index.Invalidate(packageName)

	// Dropping the index entry alone would report success while leaving every
	// cached file on disk. Deleting goes through the cache's own path, keyed off
	// the package prefix every storage key already carries.
	if deleter, ok := s.storage.(storage.PrefixDeleter); ok {
		deleted, err := deleter.DeletePrefix(r.Context(), download.StorageKey(packageName, ""))
		if err != nil {
			slog.ErrorContext(r.Context(), "Failed to delete cached files", "error", err, "package", packageName)
			writeStatus(w, http.StatusInternalServerError, `{"status":"error","message":"Failed to delete cached files"}`)
			return
		}
		slog.InfoContext(r.Context(), "Evicted package from cache", "package", packageName, "files_deleted", deleted)
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

// maxAdminErrors bounds the recent-failures area. Prefetch has no job registry,
// so this ring is the only place a failure is surfaced; it is deliberately small
// because it is a display, not a log.
const maxAdminErrors = 20

// adminErrors is a bounded, newest-first ring of prefetch failures.
type adminErrors struct {
	mu      sync.Mutex
	entries []adminError
}

type adminError struct {
	When    string
	Package string
	Message string
}

func (a *adminErrors) record(packageName, message string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.entries = append([]adminError{{
		When:    time.Now().UTC().Format(time.RFC3339),
		Package: packageName,
		Message: message,
	}}, a.entries...)
	if len(a.entries) > maxAdminErrors {
		a.entries = a.entries[:maxAdminErrors]
	}
}

func (a *adminErrors) list() []adminError {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]adminError(nil), a.entries...)
}

// adminView is everything the page renders. Ages are pre-formatted strings: the
// template's job is layout, not arithmetic on timestamps.
type adminView struct {
	Packages    []adminPackage
	Errors      []adminError
	Downloading []adminDownload
	Size        int64
}

type adminPackage struct {
	Name  string
	Size  int64
	Hits  int64
	Age   string
	Files []adminFile
}

type adminFile struct {
	Name string
	Size int64
	Hits int64
	Age  string
}

type adminDownload struct {
	Package  string
	File     string
	Bytes    int64
	Size     int64
	Age      string
	Requests int
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

// snapshotView groups the cache snapshot into the page's rows. Storage keys
// already encode the package and filename in a fixed shape, so grouping is
// string manipulation and needs no second index.
func (s *Service) snapshotView() adminView {
	now := time.Now()
	view := adminView{Errors: s.errors.list()}

	for _, p := range s.downloads.InFlight() {
		view.Downloading = append(view.Downloading, adminDownload{
			Package:  p.Package,
			File:     p.File,
			Bytes:    p.Bytes,
			Size:     p.Size,
			Age:      humanAge(now, p.Started),
			Requests: p.Requests,
		})
	}

	snapshotter, ok := s.storage.(cacheSnapshotter)
	if !ok {
		return view
	}

	byName := make(map[string]*adminPackage)
	for _, entry := range snapshotter.Snapshot() {
		packageName, fileName, ok := splitPackageKey(entry.Key)
		if !ok {
			continue
		}

		pkg := byName[packageName]
		if pkg == nil {
			pkg = &adminPackage{Name: packageName, Age: humanAge(now, entry.CreatedAt)}
			byName[packageName] = pkg
		}
		pkg.Size += entry.Size
		pkg.Hits += entry.Hits
		pkg.Files = append(pkg.Files, adminFile{
			Name: fileName,
			Size: entry.Size,
			Hits: entry.Hits,
			Age:  humanAge(now, entry.CreatedAt),
		})
		view.Size += entry.Size
	}

	view.Packages = make([]adminPackage, 0, len(byName))
	for _, pkg := range byName {
		sort.Slice(pkg.Files, func(i, j int) bool { return pkg.Files[i].Name < pkg.Files[j].Name })
		view.Packages = append(view.Packages, *pkg)
	}
	// The snapshot arrives most-recently-used first, which is not a stable order
	// to poll against: a row would move under the operator's cursor on every
	// refresh. Name order is.
	sort.Slice(view.Packages, func(i, j int) bool { return view.Packages[i].Name < view.Packages[j].Name })

	return view
}

// splitPackageKey pulls the package and file names out of a storage key. The
// shape is fixed at "packages/<name>/<file>"; anything else is not a package
// file and is skipped rather than guessed at.
func splitPackageKey(key string) (packageName, fileName string, ok bool) {
	rest, found := strings.CutPrefix(key, "packages/")
	if !found {
		return "", "", false
	}
	packageName, fileName, found = strings.Cut(rest, "/")
	if !found || packageName == "" || fileName == "" {
		return "", "", false
	}
	return packageName, fileName, true
}

// humanAge renders how long ago something happened, at the resolution an
// operator judging a TTL cares about.
func humanAge(now, then time.Time) string {
	if then.IsZero() {
		return "-"
	}
	age := now.Sub(then)
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd", int(age.Hours())/24)
	}
}

func (s *Service) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	s.renderAdmin(w, r, "admin.html")
}

// handleAdminRows serves the fragment the table polls for. It is the progress
// display for prefetch: files appear here as they land.
func (s *Service) handleAdminRows(w http.ResponseWriter, r *http.Request) {
	s.renderAdmin(w, r, "rows")
}

func (s *Service) renderAdmin(w http.ResponseWriter, r *http.Request, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Rendered into a buffer first so a template failure can still be a 500
	// instead of a half page with a 200 already committed.
	var buf strings.Builder
	if err := s.templates.ExecuteTemplate(&buf, name, s.snapshotView()); err != nil {
		slog.ErrorContext(r.Context(), "Failed to render admin page", "error", err, "template", name)
		http.Error(w, "Failed to render page", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(buf.String()))
}

// handleAdminAsset serves the embedded interaction library. It is inside the
// authenticated group so the page and its script share one credential prompt.
func (s *Service) handleAdminAsset(w http.ResponseWriter, _ *http.Request) {
	body, err := tmpl.FS.ReadFile("htmx.min.js")
	if err != nil {
		http.Error(w, "asset unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(body)
}
