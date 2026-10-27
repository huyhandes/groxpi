package admin_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/admin"
	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/download"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/storage"
)

// fakeFile is one file the fake index lists for its package.
type fakeFile struct {
	name   string
	yanked bool
}

// body is the file's content: its own name, so every file is distinct and its
// size is known without a lookup.
func (f fakeFile) body() []byte { return []byte(f.name) }

// upstream is a one-package PyPI index serving JSON. With hold set, every file
// request first announces itself on reached and then waits for release to be
// closed, so a test can act while a download is provably running.
type upstream struct {
	*httptest.Server
	pkg       string
	files     []fakeFile
	failFiles bool
	hold      bool
	reached   chan struct{}
	release   chan struct{}
	indexHits atomic.Int64
	fileHits  atomic.Int64
}

func newUpstream(t *testing.T, pkg string, files ...fakeFile) *upstream {
	t.Helper()
	u := &upstream{pkg: pkg, files: files, reached: make(chan struct{}, 16), release: make(chan struct{})}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+pkg+"/":
			u.indexHits.Add(1)
			entries := make([]map[string]any, 0, len(u.files))
			for _, f := range u.files {
				sum := sha256.Sum256(f.body())
				entries = append(entries, map[string]any{
					"filename": f.name,
					"url":      u.URL + "/files/" + f.name,
					"size":     len(f.body()),
					"hashes":   map[string]string{"sha256": hex.EncodeToString(sum[:])},
					"yanked":   f.yanked,
				})
			}
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_ = json.NewEncoder(w).Encode(map[string]any{"name": pkg, "files": entries})
		case strings.HasPrefix(r.URL.Path, "/files/"):
			u.fileHits.Add(1)
			if u.hold {
				u.reached <- struct{}{}
				<-u.release
			}
			if u.failFiles {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			name := strings.TrimPrefix(r.URL.Path, "/files/")
			for _, f := range u.files {
				if f.name == name {
					w.Header().Set("Content-Length", fmt.Sprint(len(f.body())))
					_, _ = w.Write(f.body())
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(u.Close)
	// Registered after u.Close, so it runs first: a held request must be let go
	// before the server can shut down.
	t.Cleanup(func() {
		select {
		case <-u.release:
		default:
			close(u.release)
		}
	})
	return u
}

// harness is the admin module over real index, download and local storage.
type harness struct {
	svc   *admin.Service
	store *storage.Store
	idx   *index.Service
	dir   string
}

// newHarness builds the modules over cacheDir, which may already hold files
// (planted under packages/) for the cache's scan to find.
func newHarness(t *testing.T, indexURL, cacheDir string) *harness {
	t.Helper()
	cfg := &config.Config{IndexURL: indexURL, IndexTTL: time.Minute, IndexCacheSize: 1 << 20, DownloadTimeout: 5 * time.Second}
	idx := index.New(cfg)
	t.Cleanup(idx.Close)
	cache, err := storage.NewLocalStorage(cacheDir, 1<<30, 0)
	require.NoError(t, err)
	store := storage.NewStore(cache, nil, download.New(cfg, idx), filepath.Join(cacheDir, "spool"), false)
	t.Cleanup(func() { _ = store.Close() })
	svc := admin.New(store, idx)
	t.Cleanup(func() { svc.Close(context.Background()) })
	return &harness{svc: svc, store: store, idx: idx, dir: cacheDir}
}

func plant(t *testing.T, dir, pkg, file string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, "packages", pkg, file)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, body, 0o600))
	return path
}

// cachedFiles reads the file names out of the listing the page renders.
func (h *harness) cachedFiles(t *testing.T) []string {
	t.Helper()
	names := []string{}
	for _, pkg := range h.svc.List(t.Context(), 1, "").Packages {
		for _, f := range pkg.Files {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

func TestList_Empty(t *testing.T) {
	h := newHarness(t, newUpstream(t, "p").URL, t.TempDir())

	view := h.svc.List(t.Context(), 1, "")
	assert.Empty(t, view.Packages)
	assert.Zero(t, view.Total)
	assert.Zero(t, view.Size)
	assert.Zero(t, view.PrevPage)
	assert.Zero(t, view.NextPage)
}

// TestList_Paginates: a fixed number of packages per page, a package's files
// never split across pages, and a page past the end clamped to the last.
func TestList_Paginates(t *testing.T) {
	dir := t.TempDir()
	for i := range admin.PageSize + 1 {
		for _, f := range []string{"a.whl", "b.tar.gz"} {
			plant(t, dir, fmt.Sprintf("p%03d", i), f, []byte("x"))
		}
	}
	h := newHarness(t, newUpstream(t, "p").URL, dir)

	first := h.svc.List(t.Context(), 1, "")
	require.Len(t, first.Packages, admin.PageSize)
	assert.Equal(t, "p000", first.Packages[0].Name)
	assert.Equal(t, admin.PageSize+1, first.Total)
	assert.Equal(t, int64(2*(admin.PageSize+1)), first.Size, "the size spans every page")
	assert.Equal(t, 1, first.Page)
	assert.Zero(t, first.PrevPage)
	assert.Equal(t, 2, first.NextPage)

	second := h.svc.List(t.Context(), 2, "")
	require.Len(t, second.Packages, 1)
	assert.Equal(t, "p050", second.Packages[0].Name)
	assert.Len(t, second.Packages[0].Files, 2, "a package's files stay on one page")
	assert.Equal(t, 1, second.PrevPage)
	assert.Zero(t, second.NextPage)

	assert.Equal(t, second, h.svc.List(t.Context(), int(^uint(0)>>1), ""), "a page past the end is the last page")
}

// TestList_FiltersBeforePaging: the filter searches every cached package, not
// just the page on screen, and the totals describe the matches.
func TestList_FiltersBeforePaging(t *testing.T) {
	dir := t.TempDir()
	for i := range admin.PageSize + 1 {
		plant(t, dir, fmt.Sprintf("p%03d", i), "a.whl", []byte("x"))
	}
	plant(t, dir, "zz-needle", "needle-1.0.tar.gz", []byte("xyz"))
	h := newHarness(t, newUpstream(t, "p").URL, dir)

	view := h.svc.List(t.Context(), 1, "  NEEDLE ")
	require.Len(t, view.Packages, 1, "a match past the first page must be found from page 1")
	assert.Equal(t, "zz-needle", view.Packages[0].Name)
	assert.Equal(t, 1, view.Total)
	assert.Equal(t, int64(3), view.Size)
	assert.Zero(t, view.NextPage)
	assert.Equal(t, "needle", view.Query)

	assert.Empty(t, h.svc.List(t.Context(), 1, "absent").Packages)
	assert.Equal(t, admin.PageSize+2, h.svc.List(t.Context(), 1, "").Total, "an empty filter lists everything")
}

func TestList_SizeAndAge(t *testing.T) {
	file := fakeFile{name: "listme-1.0.0.tar.gz"}
	up := newUpstream(t, "listme", file)
	h := newHarness(t, up.URL, t.TempDir())

	require.NoError(t, h.svc.Prefetch(t.Context(), "listme"))
	h.svc.Close(t.Context())

	view := h.svc.List(t.Context(), 1, "")
	require.Len(t, view.Packages, 1)
	pkg := view.Packages[0]
	assert.Equal(t, "listme", pkg.Name)
	assert.Equal(t, int64(len(file.body())), pkg.Size)
	assert.Equal(t, int64(len(file.body())), view.Size)
	require.Len(t, pkg.Files, 1)
	assert.Equal(t, file.name, pkg.Files[0].Name)
	assert.Equal(t, int64(len(file.body())), pkg.Files[0].Size)
	assert.Equal(t, "0s", pkg.Files[0].Age)
}

// TestList_LastAccessedTracksServes: a serve refreshes the last access, never
// the age, and is a cache hit.
func TestList_LastAccessedTracksServes(t *testing.T) {
	dir := t.TempDir()
	path := plant(t, dir, "hitme", "hitme-1.0.0.tar.gz", []byte("cached"))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
	up := newUpstream(t, "hitme", fakeFile{name: "hitme-1.0.0.tar.gz"})
	h := newHarness(t, up.URL, dir)

	require.Equal(t, "2d", h.svc.List(t.Context(), 1, "").Packages[0].Accessed)

	obj, err := h.store.Open(t.Context(), "hitme", "hitme-1.0.0.tar.gz")
	require.NoError(t, err)
	require.NoError(t, obj.Close())

	view := h.svc.List(t.Context(), 1, "")
	require.Len(t, view.Packages, 1)
	assert.Equal(t, "0s", view.Packages[0].Accessed, "a serve must refresh the package's last access")
	assert.Equal(t, "0s", view.Packages[0].Files[0].Accessed)
	assert.Equal(t, "2d", view.Packages[0].Files[0].Age, "the age is the mtime, which a serve must not touch")
	assert.Zero(t, up.fileHits.Load(), "the serve was a cache hit")
}

// TestInFlight_ShowsRunningDownload: a download that has started but not
// finished is listed with its package, file and requester count, and is gone
// once it lands in the cache.
func TestInFlight_ShowsRunningDownload(t *testing.T) {
	up := newUpstream(t, "slow", fakeFile{name: "slow-1.0.0.tar.gz"})
	up.hold = true
	h := newHarness(t, up.URL, t.TempDir())

	require.NoError(t, h.svc.Prefetch(t.Context(), "slow"))
	<-up.reached

	downloading := h.svc.InFlight()
	require.Len(t, downloading, 1)
	assert.Equal(t, "slow", downloading[0].Package)
	assert.Equal(t, "slow-1.0.0.tar.gz", downloading[0].File)
	assert.Equal(t, 1, downloading[0].Requests)

	close(up.release)
	h.svc.Close(t.Context())
	assert.Empty(t, h.svc.InFlight())
	assert.Equal(t, []string{"slow-1.0.0.tar.gz"}, h.cachedFiles(t))
}

// ---------------------------------------------------------------------------
// Prefetch
// ---------------------------------------------------------------------------

// TestPrefetch_SelectsNewestFinalRelease is the whole selection policy, seen
// through what lands in the cache: every file of the newest final release under
// PEP 440 ordering, with yanked files and pre-releases excluded.
func TestPrefetch_SelectsNewestFinalRelease(t *testing.T) {
	tests := []struct {
		name    string
		files   []fakeFile
		want    []string
		failure string // a recorded failure message fragment, "" for none
	}{
		{
			name: "only the newest release, 1.10 above 1.9",
			files: []fakeFile{
				{name: "pkg-1.0.0.tar.gz"},
				{name: "pkg-1.9.0.tar.gz"},
				{name: "pkg-1.9.0-py3-none-any.whl"},
				{name: "pkg-1.10.0.tar.gz"},
				{name: "pkg-1.10.0-py3-none-any.whl"},
			},
			want: []string{"pkg-1.10.0-py3-none-any.whl", "pkg-1.10.0.tar.gz"},
		},
		{
			name: "pre-releases above it are skipped",
			files: []fakeFile{
				{name: "pkg-1.10.0.tar.gz"},
				{name: "pkg-2.0.0rc1.tar.gz"},
				{name: "pkg-2.0.0b3.tar.gz"},
				{name: "pkg-3.0.0.dev1.tar.gz"},
			},
			want: []string{"pkg-1.10.0.tar.gz"},
		},
		{
			name: "yanked files are excluded",
			files: []fakeFile{
				{name: "pkg-1.10.0.tar.gz"},
				{name: "pkg-1.10.0-py3-none-any.whl", yanked: true},
			},
			want: []string{"pkg-1.10.0.tar.gz"},
		},
		{
			name: "a yanked newest release falls back to the next",
			files: []fakeFile{
				{name: "pkg-1.0.0.tar.gz"},
				{name: "pkg-2.0.0.tar.gz", yanked: true},
			},
			want: []string{"pkg-1.0.0.tar.gz"},
		},
		{
			name: "nothing selectable is a recorded failure",
			files: []fakeFile{
				{name: "pkg-2.0.0rc1.tar.gz"},
				{name: "pkg-1.0.0.tar.gz", yanked: true},
				{name: "pkg.tar.gz"},
			},
			want:    []string{},
			failure: "no downloadable final release",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, newUpstream(t, "pkg", tt.files...).URL, t.TempDir())

			require.NoError(t, h.svc.Prefetch(t.Context(), "pkg"))
			h.svc.Close(t.Context())

			assert.Equal(t, tt.want, h.cachedFiles(t))
			errs := h.svc.List(t.Context(), 1, "").Errors
			if tt.failure == "" {
				assert.Empty(t, errs)
				return
			}
			require.Len(t, errs, 1)
			assert.Equal(t, "pkg", errs[0].Package)
			assert.Contains(t, errs[0].Message, tt.failure)
		})
	}
}

func TestPrefetch_UpstreamFailureLeavesNoFilesAndIsSurfaced(t *testing.T) {
	up := newUpstream(t, "broken", fakeFile{name: "broken-1.0.0.tar.gz"})
	up.failFiles = true
	h := newHarness(t, up.URL, t.TempDir())

	require.NoError(t, h.svc.Prefetch(t.Context(), "broken"))
	h.svc.Close(t.Context())

	errs := h.svc.List(t.Context(), 1, "").Errors
	require.Len(t, errs, 1)
	assert.Equal(t, "broken", errs[0].Package)
	assert.Contains(t, errs[0].Message, "broken-1.0.0.tar.gz")
	assert.Empty(t, h.cachedFiles(t), "a failed prefetch must leave no files")
	entries, _ := os.ReadDir(filepath.Join(h.dir, "packages", "broken"))
	assert.Empty(t, entries, "nor any spool residue")
}

func TestPrefetch_UnknownPackageIsSurfaced(t *testing.T) {
	h := newHarness(t, newUpstream(t, "known", fakeFile{name: "known-1.0.0.tar.gz"}).URL, t.TempDir())

	require.NoError(t, h.svc.Prefetch(t.Context(), "nosuchpackage"))
	h.svc.Close(t.Context())

	errs := h.svc.List(t.Context(), 1, "").Errors
	require.Len(t, errs, 1)
	assert.Equal(t, "nosuchpackage", errs[0].Package)
	assert.Contains(t, errs[0].Message, "not found on any configured index")
}

// TestPrefetch_DuplicatesRunOnce: duplicate submissions while a prefetch of the
// same package runs are skipped, and once it has ended a new submission runs.
// Any spelling of the name is the same package. Files fail so nothing is
// cached and every run must reach upstream.
func TestPrefetch_DuplicatesRunOnce(t *testing.T) {
	up := newUpstream(t, "dup", fakeFile{name: "dup-1.0.0.tar.gz"})
	up.hold, up.failFiles = true, true
	h := newHarness(t, up.URL, t.TempDir())

	require.NoError(t, h.svc.Prefetch(t.Context(), "dup"))
	<-up.reached // the first run is now parked at upstream
	var wg sync.WaitGroup
	for _, name := range []string{"dup", "DUP", "Dup", "dup", "dUp"} {
		wg.Go(func() { assert.NoError(t, h.svc.Prefetch(t.Context(), name)) })
	}
	wg.Wait()
	up.release <- struct{}{} // let the first run's request go; the rest stay held

	// Nothing marks the end of a run except Close, which also ends submission, so
	// resubmit until a second run reaches upstream. Dedupe guarantees it is the
	// only one: no further run can start while it is held.
	require.Eventually(t, func() bool {
		require.NoError(t, h.svc.Prefetch(t.Context(), "dup"))
		select {
		case <-up.reached:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond)
	close(up.release)
	h.svc.Close(t.Context())

	assert.Equal(t, int64(2), up.fileHits.Load(), "one run per prefetch window, not one per submission")
	assert.Len(t, h.svc.List(t.Context(), 1, "").Errors, 2)
}

// TestClose_DrainsRunningPrefetch pins the ordering the drain exists for:
// without the wait, Close would return with nothing cached.
func TestClose_DrainsRunningPrefetch(t *testing.T) {
	h := newHarness(t, newUpstream(t, "drainme", fakeFile{name: "drainme-1.0.0.tar.gz"}).URL, t.TempDir())

	require.NoError(t, h.svc.Prefetch(t.Context(), "drainme"))
	h.svc.Close(t.Context())

	assert.Equal(t, []string{"drainme-1.0.0.tar.gz"}, h.cachedFiles(t))
}

// TestClose_GivesUpWhenBudgetSpent: a prefetch that cannot finish must not hold
// shutdown open past its budget.
func TestClose_GivesUpWhenBudgetSpent(t *testing.T) {
	up := newUpstream(t, "wedged", fakeFile{name: "wedged-1.0.0.tar.gz"})
	up.hold = true
	h := newHarness(t, up.URL, t.TempDir())

	require.NoError(t, h.svc.Prefetch(t.Context(), "wedged"))
	<-up.reached

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h.svc.Close(ctx) // returns rather than hanging; a failure here is a timeout

	close(up.release)
	h.svc.Close(t.Context())
}

func TestPrefetch_RefusedAfterShutdown(t *testing.T) {
	up := newUpstream(t, "late", fakeFile{name: "late-1.0.0.tar.gz"})
	h := newHarness(t, up.URL, t.TempDir())

	h.svc.Close(t.Context())

	err := h.svc.Prefetch(t.Context(), "late")
	require.ErrorIs(t, err, admin.ErrShuttingDown)
	assert.Zero(t, up.indexHits.Load(), "a refused prefetch does no work")
}

// TestPrefetch_RedirectModeRefuses: over a store built in redirect mode,
// prefetch has nothing to warm and records "caching is disabled" instead of
// fetching.
func TestPrefetch_RedirectModeRefuses(t *testing.T) {
	up := newUpstream(t, "p", fakeFile{name: "p-1.0.tar.gz"})
	cache, err := storage.NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	store := storage.NewStore(cache, nil, nil, "", true)
	t.Cleanup(func() { _ = store.Close() })
	idx := index.New(&config.Config{IndexURL: up.URL, IndexTTL: time.Minute, IndexCacheSize: 1 << 20})
	t.Cleanup(idx.Close)

	svc := admin.New(store, idx)
	require.NoError(t, svc.Prefetch(t.Context(), "p"))
	svc.Close(t.Context())

	errs := svc.List(t.Context(), 1, "").Errors
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Message, "caching is disabled")
	assert.Zero(t, up.fileHits.Load(), "nothing was fetched")
}

// ---------------------------------------------------------------------------
// Evict
// ---------------------------------------------------------------------------

// TestEvict_DeletesCachedFiles: eviction removes the files, proven by the next
// open going upstream again.
func TestEvict_DeletesCachedFiles(t *testing.T) {
	up := newUpstream(t, "evictme", fakeFile{name: "evictme-1.0.0.tar.gz"})
	h := newHarness(t, up.URL, t.TempDir())
	require.NoError(t, h.svc.Prefetch(t.Context(), "evictme"))
	h.svc.Close(t.Context())
	require.Equal(t, int64(1), up.fileHits.Load())

	require.NoError(t, h.svc.Evict(t.Context(), "evictme"))
	assert.Empty(t, h.cachedFiles(t))

	obj, err := h.store.Open(t.Context(), "evictme", "evictme-1.0.0.tar.gz")
	require.NoError(t, err)
	require.NoError(t, obj.Close())
	assert.Equal(t, int64(2), up.fileHits.Load(), "the evicted file was fetched again")
}

// TestEvict_ClearsIndexEntry: eviction also drops the index entry, so the next
// index lookup cannot list deleted files from a stale entry.
func TestEvict_ClearsIndexEntry(t *testing.T) {
	up := newUpstream(t, "indexevict", fakeFile{name: "indexevict-1.0.0.tar.gz"})
	h := newHarness(t, up.URL, t.TempDir())
	_, err := h.idx.Resolve(t.Context(), "indexevict")
	require.NoError(t, err)
	_, err = h.idx.Resolve(t.Context(), "indexevict")
	require.NoError(t, err)
	require.Equal(t, int64(1), up.indexHits.Load(), "the second lookup was cached")

	require.NoError(t, h.svc.Evict(t.Context(), "indexevict"))

	_, err = h.idx.Resolve(t.Context(), "indexevict")
	require.NoError(t, err)
	assert.Equal(t, int64(2), up.indexHits.Load(), "the index entry survived eviction")
}

// TestEvict_InvalidNameDeletesNothing pins A1 on the eviction side: a name that
// is not one path segment is refused before the index or the cache is touched,
// so a prefix spanning another package's directory is never deleted.
func TestEvict_InvalidNameDeletesNothing(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":  "a/b",
			"files": []map[string]any{{"filename": "b-1.0.tar.gz", "url": "https://example.invalid/b-1.0.tar.gz"}},
		})
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	planted := plant(t, dir, "a/b", "b-1.0.tar.gz", []byte("keep"))
	h := newHarness(t, up.URL, dir)
	_, err := h.idx.Resolve(t.Context(), "a/b")
	require.NoError(t, err)
	require.Equal(t, int64(1), hits.Load())

	err = h.svc.Evict(t.Context(), "a/b")
	require.ErrorIs(t, err, storage.ErrInvalidKey)

	_, err = os.Stat(planted)
	assert.NoError(t, err, "nothing under the cache was deleted")
	_, err = h.idx.Resolve(t.Context(), "a/b")
	require.NoError(t, err)
	assert.Equal(t, int64(1), hits.Load(), "the index entry was left intact")
}

// TestNormalizeQuery pins that a filter is folded the way cached package names
// are (PEP 503), so typing the name as it appears on PyPI finds the cache row.
func TestNormalizeQuery(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"  Requests ":    "requests",
		"typing_ext":     "typing-ext",
		"Zope.Interface": "zope-interface",
		"a__-.b":         "a-b",
		"ext_":           "ext-",
	}
	for in, want := range cases {
		if got := admin.NormalizeQuery(in); got != want {
			t.Errorf("NormalizeQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
