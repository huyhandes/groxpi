package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/admin"
	"github.com/huyhandes/groxpi/internal/config"
)

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

// cachedFileNames reads the file names out of the same snapshot the page renders.
func cachedFileNames(h *adminHarness) []string {
	var names []string
	for _, pkg := range h.svc.List(context.Background(), 1, "").Packages {
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

// TestAdminRows_RenderPageQuery pins the HTTP side of the listing: ?page=N
// parsing, the next/prev links, the poll staying on the rendered page, and the
// row columns. The paging policy itself is tested in the admin module.
func TestAdminRows_RenderPageQuery(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	h := newAdminServer(t, up.URL)
	assert.Contains(t, string(readBody(t, authGet(h.mux, "/admin"))), "Nothing cached.")

	for i := range admin.PageSize + 1 {
		path := filepath.Join(h.cacheDir, "packages", fmt.Sprintf("p%03d", i), "a.tar.gz")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, testPayload(4096), 0o600))
	}

	first := string(readBody(t, authGet(h.mux, "/admin/rows")))
	assert.Equal(t, admin.PageSize, strings.Count(first, `class="pkg"`))
	assert.Contains(t, first, `data-package="p000"`)
	assert.Contains(t, first, `href="/admin?page=2"`)
	assert.NotContains(t, first, "prev")
	assert.Contains(t, first, "51 package(s)")
	assert.Contains(t, first, "4.0 KB", "a row carries the file size")
	assert.Contains(t, first, "Last accessed")

	second := string(readBody(t, authGet(h.mux, "/admin/rows?page=2")))
	assert.Contains(t, second, `data-package="p050"`)
	assert.Contains(t, second, `href="/admin?page=1"`)
	assert.NotContains(t, second, "next")

	assert.Contains(t, string(readBody(t, authGet(h.mux, "/admin?page=2"))), `hx-get="/admin/rows?page=2"`,
		"the poll must keep the page the operator navigated to")
	assert.Equal(t, first, string(readBody(t, authGet(h.mux, "/admin/rows?page=bogus"))), "a malformed page is page 1")
	assert.Equal(t, second, string(readBody(t, authGet(h.mux, "/admin/rows?page=9223372036854775807"))),
		"a page past the end is the last page, and must not overflow")
}

// TestAdminRows_FilterQuery pins the HTTP side of the filter: ?q= reaches the
// listing (so a match past the first page is found), the page keeps it in the
// box, the poll and the pager links.
func TestAdminRows_FilterQuery(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	h := newAdminServer(t, up.URL)
	plantFile := func(pkg, file string) {
		path := filepath.Join(h.cacheDir, "packages", pkg, file)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, testPayload(16), 0o600))
	}
	for i := range 2*admin.PageSize + 1 {
		plantFile(fmt.Sprintf("p%03d", i), "a.tar.gz")
	}
	plantFile("zz-needle", "needle-1.0.tar.gz")

	rows := string(readBody(t, authGet(h.mux, "/admin/rows?q=Needle")))
	assert.Equal(t, 1, strings.Count(rows, `class="pkg"`))
	assert.Contains(t, rows, `data-package="zz-needle"`)
	assert.Contains(t, rows, "1 package(s)")

	page := string(readBody(t, authGet(h.mux, "/admin?q=needle")))
	assert.Contains(t, page, `value="needle"`, "the box shows the active filter")
	// The poll reads the live filter box instead of a q baked into its URL, so
	// typing filters without a submit and the box is escaped by htmx itself.
	assert.Contains(t, page, `hx-get="/admin/rows?page=1"`, "the poll URL carries no hand-built q")
	assert.Contains(t, page, `hx-include="#filter"`, "the poll sends the filter box")
	assert.Contains(t, page, `hx-trigger="every 3s, input changed delay:300ms from:#filter, search from:#filter"`,
		"typing in the box re-polls")
	assert.Contains(t, page, `<form class="controls" method="get" action="/admin">`, "the no-JS GET form stays")

	paged := string(readBody(t, authGet(h.mux, "/admin/rows?q=p0")))
	assert.Contains(t, paged, `href="/admin?page=2&amp;q=p0"`, "the pager keeps the filter")
}

// TestAdminRows_PagerEscapesQuery pins that a filter with URL metacharacters
// survives the pager link: an unescaped & or # would cut the query short.
func TestAdminRows_PagerEscapesQuery(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	h := newAdminServer(t, up.URL)
	for i := range admin.PageSize + 1 {
		path := filepath.Join(h.cacheDir, "packages", fmt.Sprintf("a&b#%03d", i), "a.tar.gz")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, testPayload(16), 0o600))
	}

	rows := string(readBody(t, authGet(h.mux, "/admin/rows?q="+url.QueryEscape("a&b#"))))
	assert.Contains(t, rows, `href="/admin?page=2&amp;q=a%26b%23"`)
	rows = string(readBody(t, authGet(h.mux, "/admin/rows?page=2&q="+url.QueryEscape("a&b#"))))
	assert.Contains(t, rows, `href="/admin?page=1&amp;q=a%26b%23"`)
}

// TestAdminRows_FilterAppliesToDownloads pins that the filter also narrows the
// "Downloading now" rows: a download in flight for another package is hidden,
// one for the filtered package stays.
func TestAdminRows_FilterAppliesToDownloads(t *testing.T) {
	body := testPayload(65536) // past net/http buffering, so headers flush at the gate
	up := NewFakePyPI(t).Add("slow-pkg", "slow_pkg-1.0.tar.gz",
		FakeFile{Body: body, SHA256: Sum(body), Size: int64(len(body)), Gated: true})
	h := newAdminServer(t, up.URL+"/")
	hs := httptest.NewServer(h.mux)
	t.Cleanup(hs.Close)

	// Hold one upstream download open until the test is done.
	resp, err := hs.Client().Get(hs.URL + "/simple/slow-pkg/slow_pkg-1.0.tar.gz")
	require.NoError(t, err)
	t.Cleanup(func() { up.Release(); _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() })
	require.Eventually(t, func() bool { return len(h.svc.InFlight()) == 1 }, 5*time.Second, time.Millisecond)

	other := string(readBody(t, authGet(h.mux, "/admin/rows?q=other")))
	assert.NotContains(t, other, `class="inflight"`, "a download for another package is filtered out")
	match := string(readBody(t, authGet(h.mux, "/admin/rows?q=Slow_Pkg")))
	assert.Contains(t, match, `class="inflight" data-package="slow-pkg"`, "the filtered package's download stays")
}

// ---------------------------------------------------------------------------
// Prefetch
// ---------------------------------------------------------------------------

// TestPrefetch_ReturnsAccepted: the route hands the name to the admin module and
// returns at once. What prefetch selects is tested in the admin module.
func TestPrefetch_ReturnsAccepted(t *testing.T) {
	up := newPyPIWith(t, "warmme", "warmme-1.0.0.tar.gz", testPayload(64))
	srv := newAdminServer(t, up.URL)

	resp := postPrefetch(srv.mux, "warmme", true)
	body := readBody(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "prefetch must return immediately")
	assert.Equal(t, "Prefetching warmme", string(body))
}

func TestPrefetch_RequiresPackageName(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
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
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
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
	up := newPyPIWith(t, "xsite", "xsite-1.0.0.tar.gz", testPayload(64))
	srv := newAdminServer(t, up.URL)
	router := srv.mux

	for _, site := range []string{"cross-site", "same-site"} {
		resp := postPrefetchSite(router, "xsite", site)
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"a %s prefetch must be refused", site)
	}

	// The handler never ran, so it never asked upstream for the package.
	assert.Zero(t, up.FileHits("xsite-1.0.0.tar.gz"), "the refused request must not reach the prefetch handler")
	assert.Empty(t, cachedFileNames(srv))
}

func TestAdminCrossSite_SameOriginAbsentAndNoneSucceed(t *testing.T) {
	up := newPyPIWith(t, "xsite", "xsite-1.0.0.tar.gz", testPayload(64))
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
	up := newPyPIWith(t, "keepme", "keepme-1.0.0.tar.gz", testPayload(2048))
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
// Shutdown
// ---------------------------------------------------------------------------

// TestPrefetch_RefusedAfterShutdownIs503: once the admin module refuses
// prefetches, the route says so instead of silently dropping the request.
func TestPrefetch_RefusedAfterShutdownIs503(t *testing.T) {
	up := newPyPIWith(t, "latecomer", "latecomer-1.0.0.tar.gz", testPayload(64))
	srv := newAdminServer(t, up.URL)

	srv.svc.Close(context.Background())

	resp := postPrefetch(srv.mux, "latecomer", true)
	body := readBody(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, string(body), "Shutting down")
}
