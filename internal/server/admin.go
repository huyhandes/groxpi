package server

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

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
	Packages []adminPackage
	Errors   []adminError
	Size     int64
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

// adminTemplates is parsed once at construction. A parse failure is a broken
// build, not a runtime condition, so it fails construction rather than every
// request.
func parseAdminTemplates() (*template.Template, error) {
	return template.New("admin").Funcs(template.FuncMap{
		"bytes": humanBytes,
	}).ParseFS(tmpl.FS, "*.html")
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
func (s *Server) snapshotView() adminView {
	view := adminView{Errors: s.adminErrors.list()}

	snapshotter, ok := s.storage.(cacheSnapshotter)
	if !ok {
		return view
	}

	now := time.Now()
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

// humanAge renders how long ago a file was cached, at the resolution an operator
// judging a TTL cares about.
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

func (s *Server) handleAdminPage(c *gin.Context) {
	s.renderAdmin(c, "admin.html")
}

// handleAdminRows serves the fragment the table polls for. It is the progress
// display for prefetch: files appear here as they land.
func (s *Server) handleAdminRows(c *gin.Context) {
	s.renderAdmin(c, "rows")
}

func (s *Server) renderAdmin(c *gin.Context, name string) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := s.adminTemplates.ExecuteTemplate(c.Writer, name, s.snapshotView()); err != nil {
		slog.ErrorContext(c.Request.Context(), "Failed to render admin page", "error", err, "template", name)
		if !c.Writer.Written() {
			c.String(http.StatusInternalServerError, "Failed to render page")
		}
	}
}

// handleAdminAsset serves the embedded interaction library. It is inside the
// authenticated group so the page and its script share one credential prompt.
func (s *Server) handleAdminAsset(c *gin.Context) {
	body, err := tmpl.FS.ReadFile("htmx.min.js")
	if err != nil {
		c.String(http.StatusInternalServerError, "asset unavailable")
		return
	}
	c.Header("Cache-Control", "public, max-age=86400")
	c.Data(http.StatusOK, "text/javascript; charset=utf-8", body)
}
