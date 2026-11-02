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
	"regexp"
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
	srv := newTestServer(t, cfg)
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

	assert.Contains(t, string(readBody(t, authGet(h.mux, "/admin?page=2"))),
		`<input id="page" name="page" type="hidden" value="2" form="filter-form">`,
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
	// The table reads the live filter form (Datastar's form content type sends
	// it as the query) instead of a q and page baked into its URL.
	assert.Contains(t, page, `data-on:input__debounce.300ms="document.getElementById('page').value = 1; @get('/admin/rows', {contentType: 'form'})"`,
		"typing in the box reloads the table from the filter form")
	assert.Contains(t, page, `@get('/admin/inflight', {contentType: 'form', selector: '#filter-form'})`,
		"the poll sends the filter form, which carries the table's cache generation")
	assert.Contains(t, page, `method="post" action="/admin/prefetch"`, "the prefetch form posts without JavaScript too")
	assert.Contains(t, page, `data-on:submit__prevent="@post('/admin/prefetch', {contentType: 'form'})"`)
	assert.Contains(t, page, `id="prefetch-status"`)
	assert.Contains(t, page, `<form id="filter-form" class="inline" method="get" action="/admin"`,
		"the no-JS GET form stays")

	paged := string(readBody(t, authGet(h.mux, "/admin/rows?q=p0")))
	assert.Contains(t, paged, `href="/admin?page=2&amp;q=p0"`, "the pager keeps the filter")
}

// TestAdminRows_EvictConfirmsAndReportsFailure pins the Evict button's two
// guards: the browser asks before deleting, naming the package and both
// places it is removed from, and a refused or failed DELETE shows its reply in
// a status line outside the table, which survives the reload that follows.
func TestAdminRows_EvictConfirmsAndReportsFailure(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	h := newAdminServer(t, up.URL)
	path := filepath.Join(h.cacheDir, "packages", "needle", "needle-1.0.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, testPayload(16), 0o600))

	rows := string(readBody(t, authGet(h.mux, "/admin/rows")))
	assert.Contains(t, rows, `data-on:click="confirm('Evict needle? This deletes it from the cache and from the object store, if one is configured.') && @delete('/cache/needle', {contentType: 'form', selector: '#filter-form'})"`)

	page := string(readBody(t, authGet(h.mux, "/admin")))
	assert.Contains(t, page, `id="evict-status"`)
	assert.Less(t, strings.Index(page, `id="evict-status"`), strings.Index(page, `id="cache"`),
		"the status line sits outside the table the reload replaces")
}

// TestAdminPage_FilterRestartsAtPageOne pins that a new filter on page N starts
// at page 1 of the matches: the page travels in a hidden input outside the
// filter form, so a no-JS submit sends only q, while the poll includes it and
// typing in the box resets it to 1. The pager links still work.
func TestAdminPage_FilterRestartsAtPageOne(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	h := newAdminServer(t, up.URL)
	for i := range 2*admin.PageSize + 1 {
		path := filepath.Join(h.cacheDir, "packages", fmt.Sprintf("p%03d", i), "a.tar.gz")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, testPayload(16), 0o600))
	}

	page := string(readBody(t, authGet(h.mux, "/admin?page=2")))
	form := page[strings.Index(page, `<form id="filter-form"`):]
	form = form[:strings.Index(form, "</form>")]
	hidden := `<input id="page" name="page" type="hidden" value="2" form="filter-form">`
	assert.Contains(t, form, `<button type="submit" name="page" value="1">Filter</button>`)
	assert.Less(t, strings.Index(page, `name="page" value="1"`), strings.Index(page, hidden),
		"a plain submit sends the Filter button's page=1 first, and Go reads the first value")
	assert.Contains(t, form, `document.getElementById('page').value = 1`, "typing in the box starts again at page 1")
	assert.Contains(t, string(readBody(t, authGet(h.mux, "/admin?q=p&page=1&page=2"))), `value="1" form="filter-form"`,
		"the first page value wins")
	assert.Contains(t, page, `href="/admin?page=1"`, "the pager links stay")
	assert.Contains(t, page, `href="/admin?page=3"`, "the pager links stay")
}

// TestAdminPage_FilterBoxShowsRawQuery pins that the box shows what the
// operator typed (trimmed), while the listing filters on the normalised form.
func TestAdminPage_FilterBoxShowsRawQuery(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	h := newAdminServer(t, up.URL)
	path := filepath.Join(h.cacheDir, "packages", "zz-needle", "needle-1.0.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, testPayload(16), 0o600))

	page := string(readBody(t, authGet(h.mux, "/admin?q="+url.QueryEscape("  ZZ_Needle "))))
	assert.Contains(t, page, `value="ZZ_Needle"`, "the box keeps the operator's spelling")
	assert.Contains(t, page, `data-package="zz-needle"`, "the filter matches the normalised form")
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

	other := string(readBody(t, authGet(h.mux, "/admin/inflight?q=other")))
	assert.NotContains(t, other, `class="inflight"`, "a download for another package is filtered out")
	match := string(readBody(t, authGet(h.mux, "/admin/inflight?q=Slow_Pkg")))
	assert.Contains(t, match, `class="inflight" data-package="slow-pkg"`, "the filtered package's download stays")
}

// TestAdminInflight_PollSkipsCacheListing pins that the page's periodic poll is
// the in-flight downloads only: it never lists cached packages, so it never
// walks the cache. The package table reloads on a filter change, an explicit
// refresh, or when the poll reports the cache generation changed.
func TestAdminInflight_PollSkipsCacheListing(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	h := newAdminServer(t, up.URL)
	path := filepath.Join(h.cacheDir, "packages", "cached-pkg", "cached_pkg-1.0.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, testPayload(16), 0o600))

	resp := authGet(h.mux, "/admin/inflight")
	poll := string(readBody(t, resp))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, poll, "cached-pkg", "the poll must not list cached packages")
	assert.NotContains(t, poll, "package(s)")
	assert.NotContains(t, poll, `id="cache"`, "a poll without a generation carries no table")

	page := string(readBody(t, authGet(h.mux, "/admin")))
	assert.Contains(t, page, `data-on-interval__duration.3s="!document.hidden && @get('/admin/inflight'`)
	assert.Equal(t, 1, strings.Count(page, "data-on-interval"), "the package table does not poll")
	assert.Contains(t, page, `data-package="cached-pkg"`, "the first render still lists the cache")
}

var genInput = regexp.MustCompile(`<input id="gen" name="gen" type="hidden" value="(\d+)" form="filter-form">`)

// cacheGen is the cache generation a rows render carries for the poll to send.
func cacheGen(t *testing.T, router http.Handler) string {
	t.Helper()
	m := genInput.FindStringSubmatch(string(readBody(t, authGet(router, "/admin/rows"))))
	require.NotNil(t, m, "the package table carries the cache generation")
	return m[1]
}

// pollTrigger is "cache-changed" when the poll's reply carries the reloaded
// package table, the signal that the cache moved since gen.
func pollTrigger(t *testing.T, router http.Handler, gen string) string {
	t.Helper()
	if strings.Contains(string(readBody(t, authGet(router, "/admin/inflight?gen="+gen))), `id="cache"`) {
		return "cache-changed"
	}
	return ""
}

// TestAdminInflight_CacheGenerationTriggersReload pins that the poll compares
// the cache generation the table was rendered at, not a row count: a download
// that starts and finishes between two polls, or an eviction, still reloads the
// table, and a poll at the current generation does not.
func TestAdminInflight_CacheGenerationTriggersReload(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	h := newAdminServer(t, up.URL)

	gen0 := cacheGen(t, h.mux)
	assert.Empty(t, pollTrigger(t, h.mux, gen0), "nothing changed")

	_ = readBody(t, getFile(h.mux, "pkg", "pkg-1.0.0.tar.gz"))
	waitCached(t, h.cacheDir, "pkg", "pkg-1.0.0.tar.gz")
	assert.Equal(t, "cache-changed", pollTrigger(t, h.mux, gen0),
		"a download landed between polls, so the package table reloads")

	gen1 := cacheGen(t, h.mux)
	assert.NotEqual(t, gen0, gen1)
	assert.Empty(t, pollTrigger(t, h.mux, gen1), "the table is current")
	assert.Empty(t, pollTrigger(t, h.mux, gen1), "a repeated poll stays quiet")

	req := httptest.NewRequest(http.MethodDelete, "/cache/pkg", nil)
	req.SetBasicAuth(adminUser, adminPass)
	resp := do(h.mux, req)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "cache-changed", pollTrigger(t, h.mux, gen1),
		"an eviction changes the cache, so the package table reloads")
	assert.Empty(t, pollTrigger(t, h.mux, cacheGen(t, h.mux)))
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
// Datastar
// ---------------------------------------------------------------------------

// TestAdminPage_ShipsNoScript pins that no JavaScript is served from the binary:
// the one script is Datastar at a pinned version from its CDN.
func TestAdminPage_ShipsNoScript(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	router := newAdminServer(t, up.URL).mux

	body := string(readBody(t, authGet(router, "/admin")))
	assert.Equal(t, 1, strings.Count(body, "<script"))
	assert.Contains(t, body, `<script type="module" src="https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.4/bundles/datastar.js"></script>`)
	assert.Equal(t, http.StatusNotFound, authGet(router, "/admin/htmx.min.js").StatusCode)
}

func datastarReq(method, path string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	req.SetBasicAuth(adminUser, adminPass)
	req.Header.Set("Datastar-Request", "true")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return req
}

// TestAdminDatastar_PrefetchAnswersFragment pins that the page's prefetch gets
// its #prefetch-status fragment with a 200, refusals included: Datastar
// patches only a 200 body.
func TestAdminDatastar_PrefetchAnswersFragment(t *testing.T) {
	up := newPyPIWith(t, "warmme", "warmme-1.0.0.tar.gz", testPayload(64))
	router := newAdminServer(t, up.URL).mux

	for pkg, want := range map[string]string{"WarmMe": "Prefetching warmme", " ": "Package name required"} {
		resp := do(router, datastarReq(http.MethodPost, "/admin/prefetch", strings.NewReader(url.Values{"package": {pkg}}.Encode())))
		body := string(readBody(t, resp))
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
		assert.Equal(t, `<p id="prefetch-status" class="status" role="status">`+want+`</p>`, body)
	}
}

// TestAdminDatastar_EvictAnswersTable pins that the page's Evict gets the
// cleared status line and the reloaded table, keeping the filter it sent,
// while a plain DELETE keeps its JSON reply.
func TestAdminDatastar_EvictAnswersTable(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	h := newAdminServer(t, up.URL)
	for _, pkg := range []string{"needle", "needle-two", "other"} {
		path := filepath.Join(h.cacheDir, "packages", pkg, pkg+"-1.0.tar.gz")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, testPayload(16), 0o600))
	}

	resp := do(h.mux, datastarReq(http.MethodDelete, "/cache/needle?q=needle&page=1", nil))
	body := string(readBody(t, resp))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, strings.HasPrefix(body, `<p id="evict-status" class="status" role="status"></p>`), "a success clears the status line")
	assert.Contains(t, body, `<div id="cache">`)
	assert.Contains(t, body, `data-package="needle-two"`)
	assert.NotContains(t, body, `data-package="needle"`, "the evicted package is gone")
	assert.NotContains(t, body, `data-package="other"`, "the table keeps the filter")

	req := httptest.NewRequest(http.MethodDelete, "/cache/other", nil)
	req.SetBasicAuth(adminUser, adminPass)
	resp = do(h.mux, req)
	assert.JSONEq(t, `{"status":"success","data":null}`, string(readBody(t, resp)))
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

func TestAdminCrossSite_PrefetchRejected(t *testing.T) {
	up := newPyPIWith(t, "xsite", "xsite-1.0.0.tar.gz", testPayload(64))
	router := newAdminServer(t, up.URL).mux

	// Only the 403 is checked: prefetch runs asynchronously, so "upstream was not
	// asked" right after the response would pass even if the handler had run.
	for _, site := range []string{"cross-site", "same-site"} {
		resp := postPrefetchSite(router, "xsite", site)
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"a %s prefetch must be refused", site)
	}
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
