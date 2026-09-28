// Package admin is the operator's view of the cache: the listing, prefetch and
// eviction. It is plain Go; the server owns its routes, the credential check and
// the page templates, and builds it only when credentials are configured.
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/storage"
)

// ErrShuttingDown is Prefetch's refusal once Close has begun.
var ErrShuttingDown = errors.New("shutting down; prefetch not accepted")

// Service is the admin module.
type Service struct {
	store   *storage.Store
	index   *index.Service
	noCache bool // redirect mode: prefetch has nothing to warm
	errors  failures

	// prefetches counts the detached prefetch goroutines so shutdown can wait for
	// them: one of them may be writing to storage when the signal arrives.
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

// New builds the admin module.
func New(cfg *config.Config, store *storage.Store, idx *index.Service) *Service {
	return &Service{
		store:   store,
		index:   idx,
		noCache: cfg.DownloadTimeout <= 0,
	}
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

// Evict drops a package's index entry and every cached file of it. Dropping the
// index entry alone would report success while leaving every cached file on
// disk; deleting goes through the cache's own path, keyed off the package prefix
// every storage key already carries.
func (s *Service) Evict(ctx context.Context, packageName string) error {
	s.index.Invalidate(packageName)

	deleted, err := s.store.DeletePrefix(ctx, storage.Key(packageName, ""))
	if err != nil {
		return fmt.Errorf("failed to delete cached files: %w", err)
	}
	slog.InfoContext(ctx, "Evicted package from cache", "package", packageName, "files_deleted", deleted)
	return nil
}

// maxFailures bounds the recent-failures area. Prefetch has no job registry,
// so this ring is the only place a failure is surfaced; it is deliberately small
// because it is a display, not a log.
const maxFailures = 20

// failures is a bounded, newest-first ring of prefetch failures.
type failures struct {
	mu      sync.Mutex
	entries []Failure
}

// Failure is one prefetch failure on the page's recent-failures area.
type Failure struct {
	When    string
	Package string
	Message string
}

func (a *failures) record(packageName, message string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.entries = append([]Failure{{
		When:    time.Now().UTC().Format(time.RFC3339),
		Package: packageName,
		Message: message,
	}}, a.entries...)
	if len(a.entries) > maxFailures {
		a.entries = a.entries[:maxFailures]
	}
}

func (a *failures) list() []Failure {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]Failure(nil), a.entries...)
}

// View is everything the page renders. Ages are pre-formatted strings: the
// template's job is layout, not arithmetic on timestamps.
type View struct {
	Packages    []Package // the current page only
	Errors      []Failure
	Downloading []Download // filled by the caller from InFlight
	Size        int64      // across every page
	Total       int        // packages across every page

	// Page is 1-based; PrevPage and NextPage are 0 when there is no such page.
	Page, PrevPage, NextPage int
}

// PageSize is how many packages one page of the listing shows. Paging is
// by package so a package's file rows never split across pages.
//
// ponytail: fixed page size, make it a query parameter when an operator asks.
const PageSize = 50

// Package is one cached package and its files.
type Package struct {
	Name     string
	Size     int64
	Age      string
	Accessed string // most recent access across the package's files
	Files    []File
}

// File is one cached file.
type File struct {
	Name     string
	Size     int64
	Age      string
	Accessed string
}

// Download is one download in flight.
type Download struct {
	Package  string
	File     string
	Bytes    int64
	Size     int64
	Age      string
	Requests int
}

// List groups one page of the cache snapshot into the page's rows. Storage keys
// already encode the package and filename in a fixed shape, so grouping is
// string manipulation and needs no second index.
func (s *Service) List(_ context.Context, page int) View {
	now := time.Now()
	view := View{Errors: s.errors.list(), Page: page}

	// The listing is the cache's own walk; the durable store is never listed.
	byName := make(map[string]*Package)
	for _, entry := range s.store.Snapshot() {
		packageName, fileName, ok := splitPackageKey(entry.Key)
		if !ok {
			continue
		}

		pkg := byName[packageName]
		if pkg == nil {
			// The snapshot is most recently accessed first, so the first file
			// seen carries the package's last access.
			pkg = &Package{Name: packageName, Age: humanAge(now, entry.CreatedAt), Accessed: humanAge(now, entry.Accessed)}
			byName[packageName] = pkg
		}
		pkg.Size += entry.Size
		pkg.Files = append(pkg.Files, File{
			Name:     fileName,
			Size:     entry.Size,
			Age:      humanAge(now, entry.CreatedAt),
			Accessed: humanAge(now, entry.Accessed),
		})
		view.Size += entry.Size
	}

	view.Packages = make([]Package, 0, len(byName))
	for _, pkg := range byName {
		sort.Slice(pkg.Files, func(i, j int) bool { return pkg.Files[i].Name < pkg.Files[j].Name })
		view.Packages = append(view.Packages, *pkg)
	}
	// The snapshot arrives most-recently-used first, which is not a stable order
	// to poll against: a row would move under the operator's cursor on every
	// refresh. Name order is.
	sort.Slice(view.Packages, func(i, j int) bool { return view.Packages[i].Name < view.Packages[j].Name })

	view.Total = len(view.Packages)
	// Clamp to the last page: a huge ?page= would overflow the offset below.
	page = min(page, max(1, (view.Total+PageSize-1)/PageSize))
	view.Page = page
	lo := (page - 1) * PageSize
	hi := min(lo+PageSize, view.Total)
	view.Packages = view.Packages[lo:hi]
	if page > 1 {
		view.PrevPage = page - 1
	}
	if hi < view.Total {
		view.NextPage = page + 1
	}
	return view
}

// InFlight lists the downloads running now, for the page's "Downloading now"
// rows.
func (s *Service) InFlight() []Download {
	now := time.Now()
	var out []Download
	for _, p := range s.store.InFlight() {
		out = append(out, Download{
			Package:  p.Package,
			File:     p.File,
			Bytes:    p.Bytes,
			Size:     p.Size,
			Age:      humanAge(now, p.Started),
			Requests: p.Requests,
		})
	}
	return out
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
