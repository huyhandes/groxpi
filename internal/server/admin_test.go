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
	name string
	body []byte
}

// adminUpstream is the fake-upstream harness for the administrative tests: one
// package with an arbitrary file list and a count of file requests.
type adminUpstream struct {
	*httptest.Server
	pkg      string
	files    []adminFakeFile
	fileHits atomic.Int64
}

func newAdminUpstream(t *testing.T, pkg string, files ...adminFakeFile) *adminUpstream {
	t.Helper()

	fake := &adminUpstream{pkg: pkg, files: files}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+pkg+"/":
			entries := make([]map[string]any, 0, len(fake.files))
			for _, f := range fake.files {
				entries = append(entries, map[string]any{
					"filename": f.name,
					"url":      fake.URL + "/files/" + f.name,
					"size":     len(f.body),
					"hashes":   map[string]string{"sha256": sha256Hex(f.body)},
				})
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
			name := strings.TrimPrefix(r.URL.Path, "/files/")
			for _, f := range fake.files {
				if f.name == name {
					w.Header().Set("Content-Length", fmt.Sprintf("%d", len(f.body)))
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

// TestAdminRows_RenderPageQuery pins the HTTP side of the listing: ?page=N
// parsing, the next/prev links, the poll staying on the rendered page, and the
// row columns. The paging policy itself is tested in the admin module.
func TestAdminRows_RenderPageQuery(t *testing.T) {
	up := newAdminUpstream(t, "pkg", adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(64)})
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

// ---------------------------------------------------------------------------
// Prefetch
// ---------------------------------------------------------------------------

// TestPrefetch_ReturnsAccepted: the route hands the name to the admin module and
// returns at once. What prefetch selects is tested in the admin module.
func TestPrefetch_ReturnsAccepted(t *testing.T) {
	up := newAdminUpstream(t, "warmme", adminFakeFile{name: "warmme-1.0.0.tar.gz", body: testPayload(64)})
	srv := newAdminServer(t, up.URL)

	resp := postPrefetch(srv.mux, "warmme", true)
	body := readBody(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "prefetch must return immediately")
	assert.Equal(t, "Prefetching warmme", string(body))
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
// Shutdown
// ---------------------------------------------------------------------------

// TestPrefetch_RefusedAfterShutdownIs503: once the admin module refuses
// prefetches, the route says so instead of silently dropping the request.
func TestPrefetch_RefusedAfterShutdownIs503(t *testing.T) {
	up := newAdminUpstream(t, "latecomer", adminFakeFile{name: "latecomer-1.0.0.tar.gz", body: testPayload(64)})
	srv := newAdminServer(t, up.URL)

	srv.svc.Close(context.Background())

	resp := postPrefetch(srv.mux, "latecomer", true)
	body := readBody(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, string(body), "Shutting down")
}
