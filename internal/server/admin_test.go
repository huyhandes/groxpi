package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/admin"
	"github.com/huyhandes/groxpi/internal/config"
)

// adminFakeFile is one file in the fake index's package listing.
type adminFakeFile struct {
	name   string
	body   []byte
	yanked bool
}

// adminUpstream is the fake-upstream harness for the administrative tests: one
// package with an arbitrary file list, a switch that makes every file body
// fail, and an optional gate that holds file bodies until released.
type adminUpstream struct {
	*httptest.Server
	pkg       string
	files     []adminFakeFile
	failFiles atomic.Bool
	fileHits  atomic.Int64
	gate      chan struct{} // nil: files are served immediately
}

func newAdminUpstream(t *testing.T, pkg string, files ...adminFakeFile) *adminUpstream {
	t.Helper()

	fake := &adminUpstream{pkg: pkg, files: files}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+pkg+"/":
			entries := make([]map[string]any, 0, len(fake.files))
			for _, f := range fake.files {
				entry := map[string]any{
					"filename": f.name,
					"url":      fake.URL + "/files/" + f.name,
					"size":     len(f.body),
					"hashes":   map[string]string{"sha256": sha256Hex(f.body)},
				}
				if f.yanked {
					entry["yanked"] = true
				}
				entries = append(entries, entry)
			}
			if strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json") {
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				_ = json.NewEncoder(w).Encode(map[string]any{"name": pkg, "files": entries})
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, "<html><body>")
			for _, e := range entries {
				_, _ = fmt.Fprintf(w, `<a href=%q>%s</a>`, e["url"], e["filename"])
			}
			_, _ = fmt.Fprint(w, "</body></html>")
		case strings.HasPrefix(r.URL.Path, "/files/"):
			fake.fileHits.Add(1)
			if fake.failFiles.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			name := strings.TrimPrefix(r.URL.Path, "/files/")
			for _, f := range fake.files {
				if f.name == name {
					w.Header().Set("Content-Length", fmt.Sprintf("%d", len(f.body)))
					if fake.gate != nil {
						// Commit the headers so the download is registered as
						// in flight, then hold the body until released.
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
						<-fake.gate
					}
					_, _ = w.Write(f.body)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.Close)
	return fake
}

// adminHarness is the full server with credentials configured, plus the admin
// module it built, so tests can read the same listing the page renders.
type adminHarness struct {
	svc      *admin.Service
	mux      http.Handler
	cacheDir string
}

func newAdminServer(t *testing.T, upstreamURL string) *adminHarness {
	t.Helper()
	cfg := &config.Config{
		IndexURL:        upstreamURL,
		IndexTTL:        time.Minute,
		IndexCacheSize:  1 << 20,
		CacheDir:        t.TempDir(),
		CacheSize:       1 << 30,
		DownloadTimeout: 5 * time.Second,
		LogLevel:        "ERROR",
		AdminUsername:   adminUser,
		AdminPassword:   adminPass,
	}
	srv, err := NewServer(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return &adminHarness{svc: srv.admin, mux: srv.Router(), cacheDir: cfg.CacheDir}
}

func authGet(router http.Handler, path string) *http.Response {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetBasicAuth(adminUser, adminPass)
	return do(router, req)
}

func postPrefetch(router http.Handler, pkg string, auth bool) *http.Response {
	body := url.Values{"package": {pkg}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/admin/prefetch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if auth {
		req.SetBasicAuth(adminUser, adminPass)
	}
	return do(router, req)
}

// waitFor is the bounded wait every asynchronous assertion below uses: prefetch
// is detached, so the test polls the same rows the page polls rather than
// sleeping for a guessed duration.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 500 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// cachedFileNames reads the file names out of the same snapshot the page renders.
func cachedFileNames(h *adminHarness) []string {
	var names []string
	for _, pkg := range h.svc.List(context.Background(), 1).Packages {
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

// TestAdminListing_ShowsInFlightDownload pins the "Downloading now" rows: a
// download that has started but not finished is visible to the operator with
// its progress, and disappears once it lands in the cache.
func TestAdminListing_ShowsInFlightDownload(t *testing.T) {
	up := newAdminUpstream(t, "slow", adminFakeFile{name: "slow-1.0.0.tar.gz", body: testPayload(8192)})
	up.gate = make(chan struct{})
	h := newAdminServer(t, up.URL)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = readBody(t, getFile(h.mux, "slow", "slow-1.0.0.tar.gz"))
	}()

	waitFor(t, "the download to register as in flight", func() bool {
		return len(h.svc.InFlight()) == 1
	})
	body := string(readBody(t, authGet(h.mux, "/admin/rows")))
	assert.Contains(t, body, "Downloading now")
	assert.Contains(t, body, "slow-1.0.0.tar.gz")
	downloading := h.svc.InFlight()
	require.Len(t, downloading, 1)
	assert.Equal(t, "slow", downloading[0].Package)
	assert.Equal(t, int64(8192), downloading[0].Size)
	assert.Equal(t, 1, downloading[0].Requests)

	close(up.gate)
	<-done
	waitFor(t, "the download to leave the in-flight list", func() bool {
		return len(h.svc.InFlight()) == 0
	})
	assert.NotContains(t, string(readBody(t, authGet(h.mux, "/admin/rows"))), "Downloading now")
}
func TestAdminListing_EmptyCacheHasNoRows(t *testing.T) {
	up := newAdminUpstream(t, "pkg", adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(64)})
	router := newAdminServer(t, up.URL).mux

	resp := authGet(router, "/admin")
	body := string(readBody(t, resp))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, body, "data-package=", "an empty cache must render no rows")
	assert.Contains(t, body, "Nothing cached.")
}

// TestAdminListing_Paginates pins ?page=N on the listing: a fixed number of
// packages per page, a package's files never split, next/prev links, and the
// poll staying on the page it was rendered for.
func TestAdminListing_Paginates(t *testing.T) {
	up := newAdminUpstream(t, "pkg", adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(64)})
	h := newAdminServer(t, up.URL)
	for i := range admin.PageSize + 1 {
		for _, f := range []string{"a.whl", "b.tar.gz"} {
			path := filepath.Join(h.cacheDir, "packages", fmt.Sprintf("p%03d", i), f)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
			require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
		}
	}

	first := string(readBody(t, authGet(h.mux, "/admin/rows")))
	assert.Equal(t, admin.PageSize, strings.Count(first, `class="pkg"`))
	assert.Contains(t, first, `data-package="p000"`)
	assert.NotContains(t, first, `data-package="p050"`)
	assert.Contains(t, first, `href="/admin?page=2"`)
	assert.NotContains(t, first, "prev")
	assert.Contains(t, first, "51 package(s)")

	second := string(readBody(t, authGet(h.mux, "/admin/rows?page=2")))
	assert.Equal(t, 1, strings.Count(second, `class="pkg"`))
	assert.Equal(t, 2, strings.Count(second, `class="file" data-package="p050"`), "a package's files stay on one page")
	assert.Contains(t, second, `href="/admin?page=1"`)
	assert.NotContains(t, second, "next")

	assert.Contains(t, string(readBody(t, authGet(h.mux, "/admin?page=2"))), `hx-get="/admin/rows?page=2"`,
		"the poll must keep the page the operator navigated to")
	assert.Equal(t, first, string(readBody(t, authGet(h.mux, "/admin/rows?page=bogus"))), "a malformed page is page 1")
	assert.Equal(t, second, string(readBody(t, authGet(h.mux, "/admin/rows?page=9223372036854775807"))),
		"a page past the end is the last page, and must not overflow")
}

func TestAdminListing_ShowsDownloadedPackageWithSizeAndAge(t *testing.T) {
	payload := testPayload(4096)
	up := newAdminUpstream(t, "listme", adminFakeFile{name: "listme-1.0.0.tar.gz", body: payload})
	srv := newAdminServer(t, up.URL)
	router := srv.mux

	_ = readBody(t, getFile(router, "listme", "listme-1.0.0.tar.gz"))
	waitFor(t, "the download to reach the cache snapshot", func() bool {
		return len(srv.svc.List(context.Background(), 1).Packages) == 1
	})

	body := string(readBody(t, authGet(router, "/admin/rows")))
	assert.Contains(t, body, `data-package="listme"`)
	assert.Contains(t, body, "listme-1.0.0.tar.gz")
	assert.Contains(t, body, "4.0 KB", "the row must carry the file size")

	view := srv.svc.List(context.Background(), 1)
	require.Len(t, view.Packages, 1)
	require.Len(t, view.Packages[0].Files, 1)
	assert.Equal(t, int64(len(payload)), view.Packages[0].Files[0].Size)
	assert.NotEmpty(t, view.Packages[0].Files[0].Age, "the row must carry an age")
}

func TestAdminListing_LastAccessedTracksServes(t *testing.T) {
	up := newAdminUpstream(t, "hitme", adminFakeFile{name: "hitme-1.0.0.tar.gz", body: testPayload(2048)})
	srv := newAdminServer(t, up.URL)
	router := srv.mux
	cacheDir := srv.cacheDir

	_ = readBody(t, getFile(router, "hitme", "hitme-1.0.0.tar.gz"))
	waitCached(t, cacheDir, "hitme", "hitme-1.0.0.tar.gz")
	path := cachedPath(cacheDir, "hitme", "hitme-1.0.0.tar.gz")

	// Age the access far past the touch throttle, so the next serve must bump it.
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
	require.Equal(t, "2d", srv.svc.List(context.Background(), 1).Packages[0].Accessed)

	resp := getFile(router, "hitme", "hitme-1.0.0.tar.gz")
	_ = readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := string(readBody(t, authGet(router, "/admin/rows")))
	assert.Contains(t, body, "Last accessed")
	assert.NotContains(t, body, "Hits")

	view := srv.svc.List(context.Background(), 1)
	require.Len(t, view.Packages, 1)
	assert.Equal(t, "0s", view.Packages[0].Accessed, "a serve must refresh the package's last access")
	assert.Equal(t, "0s", view.Packages[0].Files[0].Accessed)
	assert.Equal(t, "2d", view.Packages[0].Files[0].Age, "the age is the mtime, which a serve must not touch")
	assert.Equal(t, int64(1), up.fileHits.Load(), "only the first request should have gone upstream")
}

// ---------------------------------------------------------------------------
// Prefetch
// ---------------------------------------------------------------------------

func TestPrefetch_ReturnsAcceptedAndFilesAppear(t *testing.T) {
	up := newAdminUpstream(t, "warmme",
		adminFakeFile{name: "warmme-1.0.0.tar.gz", body: testPayload(8192)},
		adminFakeFile{name: "warmme-1.0.0-py3-none-any.whl", body: testPayload(4096)},
	)
	srv := newAdminServer(t, up.URL)
	router := srv.mux

	resp := postPrefetch(router, "warmme", true)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "prefetch must return immediately")

	waitFor(t, "prefetched files to appear in the polled rows", func() bool {
		return strings.Contains(string(readBody(t, authGet(router, "/admin/rows"))), "warmme-1.0.0-py3-none-any.whl")
	})

	assert.Equal(t, []string{"warmme-1.0.0-py3-none-any.whl", "warmme-1.0.0.tar.gz"}, cachedFileNames(srv))
	assert.Empty(t, srv.svc.List(context.Background(), 1).Errors)
}

// TestPrefetch_OnlyNewestRelease also pins PEP 440 ordering: 1.10.0 is newer
// than 1.9.0, which a string comparison gets wrong.
func TestPrefetch_OnlyNewestRelease(t *testing.T) {
	up := newAdminUpstream(t, "manyver",
		adminFakeFile{name: "manyver-1.0.0.tar.gz", body: testPayload(100)},
		adminFakeFile{name: "manyver-1.9.0.tar.gz", body: testPayload(200)},
		adminFakeFile{name: "manyver-1.9.0-py3-none-any.whl", body: testPayload(210)},
		adminFakeFile{name: "manyver-1.10.0.tar.gz", body: testPayload(300)},
		adminFakeFile{name: "manyver-1.10.0-py3-none-any.whl", body: testPayload(310)},
	)
	srv := newAdminServer(t, up.URL)

	resp := postPrefetch(srv.mux, "manyver", true)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	want := []string{"manyver-1.10.0-py3-none-any.whl", "manyver-1.10.0.tar.gz"}
	waitFor(t, "the newest release's files", func() bool { return len(cachedFileNames(srv)) == len(want) })
	assert.Equal(t, want, cachedFileNames(srv), "only the newest release's files may be fetched")
}

func TestPrefetch_SkipsPreReleases(t *testing.T) {
	up := newAdminUpstream(t, "prerel",
		adminFakeFile{name: "prerel-1.10.0.tar.gz", body: testPayload(100)},
		adminFakeFile{name: "prerel-2.0.0rc1.tar.gz", body: testPayload(200)},
		adminFakeFile{name: "prerel-2.0.0b3.tar.gz", body: testPayload(300)},
		adminFakeFile{name: "prerel-3.0.0.dev1.tar.gz", body: testPayload(400)},
	)
	srv := newAdminServer(t, up.URL)

	_ = readBody(t, postPrefetch(srv.mux, "prerel", true))

	want := []string{"prerel-1.10.0.tar.gz"}
	waitFor(t, "the newest final release", func() bool { return len(cachedFileNames(srv)) == len(want) })
	// Give any wrongly-selected pre-release a chance to land before asserting.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, want, cachedFileNames(srv), "a pre-release above the newest final release must not be selected")
}

func TestPrefetch_ExcludesYankedFiles(t *testing.T) {
	up := newAdminUpstream(t, "yanked",
		adminFakeFile{name: "yanked-1.10.0.tar.gz", body: testPayload(100)},
		adminFakeFile{name: "yanked-1.10.0-py3-none-any.whl", body: testPayload(200), yanked: true},
	)
	srv := newAdminServer(t, up.URL)

	_ = readBody(t, postPrefetch(srv.mux, "yanked", true))

	want := []string{"yanked-1.10.0.tar.gz"}
	waitFor(t, "the unyanked file", func() bool { return len(cachedFileNames(srv)) == len(want) })
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, want, cachedFileNames(srv), "a yanked file must not be prefetched")
}

func TestPrefetch_UpstreamFailureLeavesNoFilesAndIsSurfaced(t *testing.T) {
	up := newAdminUpstream(t, "brokenpkg", adminFakeFile{name: "brokenpkg-1.0.0.tar.gz", body: testPayload(4096)})
	up.failFiles.Store(true)

	srv := newAdminServer(t, up.URL)
	router := srv.mux

	resp := postPrefetch(router, "brokenpkg", true)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	waitFor(t, "the failure to appear in the page's error area", func() bool {
		return strings.Contains(string(readBody(t, authGet(router, "/admin"))), "brokenpkg-1.0.0.tar.gz")
	})

	body := string(readBody(t, authGet(router, "/admin")))
	assert.Contains(t, body, "Recent failures")
	assert.Empty(t, cachedFileNames(srv), "a failed prefetch must leave no partial files")
	assertNotCached(t, srv.cacheDir, "brokenpkg", "brokenpkg-1.0.0.tar.gz")
}

func TestPrefetch_UnknownPackageIsSurfaced(t *testing.T) {
	up := newAdminUpstream(t, "known", adminFakeFile{name: "known-1.0.0.tar.gz", body: testPayload(64)})
	srv := newAdminServer(t, up.URL)
	router := srv.mux

	_ = readBody(t, postPrefetch(router, "nosuchpackage", true))

	waitFor(t, "the not-found failure to be surfaced", func() bool {
		return strings.Contains(string(readBody(t, authGet(router, "/admin/rows"))), "nosuchpackage")
	})
}

func TestPrefetch_RequiresPackageName(t *testing.T) {
	up := newAdminUpstream(t, "pkg", adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(64)})
	router := newAdminServer(t, up.URL).mux

	resp := postPrefetch(router, "   ", true)
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// Embedded assets
// ---------------------------------------------------------------------------

// TestAdminPage_MakesNoExternalAssetRequests pins the air-gap property: every
// asset the page references is served from this binary.
func TestAdminPage_MakesNoExternalAssetRequests(t *testing.T) {
	up := newAdminUpstream(t, "pkg", adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(64)})
	router := newAdminServer(t, up.URL).mux

	body := string(readBody(t, authGet(router, "/admin")))
	assert.NotContains(t, body, "//unpkg.com")
	assert.NotContains(t, body, "//cdn.")
	assert.NotContains(t, body, "https://")
	assert.Contains(t, body, `src="/admin/htmx.min.js"`)

	resp := authGet(router, "/admin/htmx.min.js")
	script := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(script), "htmx", "the interaction library must be served from the binary")
}

// ---------------------------------------------------------------------------
// Cross-site rejection
// ---------------------------------------------------------------------------

// postPrefetchSite issues the authenticated prefetch a cross-site form post would
// issue, with the browser's own fetch-metadata declaration attached (or, for the
// empty string, absent as a non-browser client leaves it).
func postPrefetchSite(router http.Handler, pkg, site string) *http.Response {
	body := url.Values{"package": {pkg}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/admin/prefetch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(adminUser, adminPass)
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	return do(router, req)
}

func TestAdminCrossSite_PrefetchRejectedAndHandlerDoesNotRun(t *testing.T) {
	up := newAdminUpstream(t, "xsite", adminFakeFile{name: "xsite-1.0.0.tar.gz", body: testPayload(64)})
	srv := newAdminServer(t, up.URL)
	router := srv.mux

	for _, site := range []string{"cross-site", "same-site"} {
		resp := postPrefetchSite(router, "xsite", site)
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"a %s prefetch must be refused", site)
	}

	// The handler never ran, so it never asked upstream for the package.
	assert.Zero(t, up.fileHits.Load(), "the refused request must not reach the prefetch handler")
	assert.Empty(t, cachedFileNames(srv))
}

func TestAdminCrossSite_SameOriginAbsentAndNoneSucceed(t *testing.T) {
	up := newAdminUpstream(t, "xsite", adminFakeFile{name: "xsite-1.0.0.tar.gz", body: testPayload(64)})
	router := newAdminServer(t, up.URL).mux

	for _, site := range []string{"same-origin", "", "none"} {
		resp := postPrefetchSite(router, "xsite", site)
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode,
			"Sec-Fetch-Site %q must be allowed through", site)
	}
}

// TestAdminCrossSite_CacheDeletionInherits pins that the check lives on the route
// group: nothing wires it to the eviction routes, yet they are covered.
func TestAdminCrossSite_CacheDeletionInherits(t *testing.T) {
	up := newAdminUpstream(t, "keepme", adminFakeFile{name: "keepme-1.0.0.tar.gz", body: testPayload(2048)})
	srv := newAdminServer(t, up.URL)
	router := srv.mux

	_ = readBody(t, getFile(router, "keepme", "keepme-1.0.0.tar.gz"))
	waitCached(t, srv.cacheDir, "keepme", "keepme-1.0.0.tar.gz")

	for _, path := range []string{"/cache/keepme", "/cache/list"} {
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		req.SetBasicAuth(adminUser, adminPass)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp := do(router, req)
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"DELETE %s must inherit the cross-site check", path)
	}

	assert.Equal(t, []string{"keepme-1.0.0.tar.gz"}, cachedFileNames(srv),
		"the refused deletion must not have evicted anything")
}

// ---------------------------------------------------------------------------
// Shutdown: prefetch registration versus the drain (huyhandes/groxpi#41)
// ---------------------------------------------------------------------------

// TestPrefetchRefusedAfterShutdown pins the refusal that closes the late-Add
// window. A handler still parked reading its request body when shutdown begins
// reaches startPrefetch after Close has entered its wait; registering then is
// documented WaitGroup misuse, so the request is refused instead.
func TestPrefetchRefusedAfterShutdown(t *testing.T) {
	up := newAdminUpstream(t, "latecomer", adminFakeFile{name: "latecomer-1.0.0.tar.gz", body: testPayload(64)})
	srv := newAdminServer(t, up.URL)
	router := srv.mux

	// Accepted while serving.
	resp := postPrefetch(router, "latecomer", true)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	srv.svc.Close(context.Background())

	// Refused afterwards, and visibly so rather than silently dropped.
	resp = postPrefetch(router, "latecomer", true)
	body := readBody(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
		"a prefetch arriving after shutdown must be refused, not registered")
	assert.Contains(t, string(body), "Shutting down")
}

// TestCloseDrainsRunningPrefetch pins the ordering the drain exists for. The
// request returns 202 as soon as the prefetch is registered, long before the
// download runs, so without the wait Close would release storage with nothing
// cached — not a failed write, an absent one.
func TestCloseDrainsRunningPrefetch(t *testing.T) {
	up := newAdminUpstream(t, "drainme", adminFakeFile{name: "drainme-1.0.0.tar.gz", body: testPayload(4096)})
	srv := newAdminServer(t, up.URL)

	resp := postPrefetch(srv.mux, "drainme", true)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	srv.svc.Close(context.Background())

	assert.Equal(t, []string{"drainme-1.0.0.tar.gz"}, cachedFileNames(srv),
		"Close must wait for the in-flight prefetch before releasing storage")
}

// TestCloseGivesUpWhenBudgetSpent pins the other half of the drain: a prefetch
// that cannot finish must not hold shutdown open. An unbounded wait here is what
// turns a graceful stop into a SIGKILL once the container's stop grace expires.
// The budget is spent before Close is called rather than waited out, so the test
// asserts the give-up without a real-clock sleep.
func TestCloseGivesUpWhenBudgetSpent(t *testing.T) {
	// A wedged upstream: it answers nothing until the test lets it, so the
	// prefetch it feeds cannot complete on its own.
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer up.Close()

	srv := newAdminServer(t, up.URL)

	resp := postPrefetch(srv.mux, "wedged", true)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Returns rather than hanging: the test failing here looks like a timeout.
	srv.svc.Close(ctx)

	// Let the abandoned prefetch unwind before the temp cache directory goes.
	close(release)
	srv.svc.Close(context.Background())
}
