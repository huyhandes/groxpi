package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

const (
	adminUser = "op"
	adminPass = "s3cret"
)

// adminFakeFile is one file in the fake index's package listing.
type adminFakeFile struct {
	name   string
	body   []byte
	yanked bool
}

// adminUpstream is the fake-upstream harness for the administrative tests: one
// package with an arbitrary file list, and a switch that makes every file body
// fail.
type adminUpstream struct {
	*httptest.Server
	pkg       string
	files     []adminFakeFile
	failFiles atomic.Bool
	fileHits  atomic.Int64
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

// adminConfig is the base configuration for these tests; the option mutates the
// administrative settings under test.
func adminConfig(t *testing.T, upstreamURL string, opts ...func(*config.Config)) *config.Config {
	t.Helper()
	cfg := &config.Config{
		IndexURL:        upstreamURL,
		IndexTTL:        time.Minute,
		IndexCacheSize:  1 << 20,
		CacheDir:        t.TempDir(),
		CacheSize:       1 << 30,
		DownloadTimeout: 5 * time.Second,
		LogLevel:        "ERROR",
	}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}

func withCredentials(cfg *config.Config) {
	cfg.AdminUsername = adminUser
	cfg.AdminPassword = adminPass
}

func newAdminServer(t *testing.T, upstreamURL string, opts ...func(*config.Config)) *Server {
	t.Helper()
	srv, err := NewServer(adminConfig(t, upstreamURL, opts...))
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func do(router *gin.Engine, req *http.Request) *http.Response {
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

func authGet(router *gin.Engine, path string) *http.Response {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetBasicAuth(adminUser, adminPass)
	return do(router, req)
}

func postPrefetch(router *gin.Engine, pkg string, auth bool) *http.Response {
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
func cachedFileNames(srv *Server) []string {
	var names []string
	for _, pkg := range srv.snapshotView().Packages {
		for _, f := range pkg.Files {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// adminPaths is the whole surface that must disappear without credentials —
// including the pre-existing cache routes, which used to be open.
var adminPaths = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/admin"},
	{http.MethodGet, "/admin/rows"},
	{http.MethodGet, "/admin/htmx.min.js"},
	{http.MethodPost, "/admin/prefetch"},
	{http.MethodDelete, "/cache/list"},
	{http.MethodDelete, "/cache/somepkg"},
}

func TestAdminAuth_NoCredentials_SurfaceReturns404(t *testing.T) {
	up := newAdminUpstream(t, "pkg", adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(64)})
	router := newAdminServer(t, up.URL).Router()

	for _, tc := range adminPaths {
		resp := do(router, httptest.NewRequest(tc.method, tc.path, nil))
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode,
			"%s %s must not exist when no credentials are configured", tc.method, tc.path)
	}
}

func TestAdminAuth_WithCredentials_UnauthorizedThenAuthorized(t *testing.T) {
	up := newAdminUpstream(t, "pkg", adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(64)})
	router := newAdminServer(t, up.URL, withCredentials).Router()

	for _, tc := range adminPaths {
		resp := do(router, httptest.NewRequest(tc.method, tc.path, nil))
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"%s %s must require credentials", tc.method, tc.path)
	}

	resp := authGet(router, "/admin")
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resp = authGet(router, "/admin/rows")
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resp = deletePackage(router, "somepkg")
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAdminAuth_EnabledWithoutCredentials_ConstructionFails(t *testing.T) {
	up := newAdminUpstream(t, "pkg", adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(64)})

	srv, err := NewServer(adminConfig(t, up.URL, func(cfg *config.Config) {
		cfg.AdminEnabled = true
	}))
	require.Error(t, err, "enabling the admin interface without credentials must fail construction")
	assert.Nil(t, srv, "a server that cannot be trusted must not be produced")
}

// TestAdminAuth_IndexRoutesStayUnauthenticated is the test that catches an
// over-broad middleware group: whatever the administrative configuration, pip
// must reach the index without credentials.
func TestAdminAuth_IndexRoutesStayUnauthenticated(t *testing.T) {
	file := adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(1024)}

	configurations := map[string][]func(*config.Config){
		"no credentials":       nil,
		"credentials":          {withCredentials},
		"enabled+credentials":  {withCredentials, func(cfg *config.Config) { cfg.AdminEnabled = true }},
		"credentials, enabled": {func(cfg *config.Config) { cfg.AdminEnabled = true }, withCredentials},
	}

	for name, opts := range configurations {
		t.Run(name, func(t *testing.T) {
			up := newAdminUpstream(t, "pkg", file)
			router := newAdminServer(t, up.URL, opts...).Router()

			for _, path := range []string{
				"/simple/pkg/",
				"/index/pkg",
				"/simple/pkg/" + file.name,
				"/index/pkg/" + file.name,
				"/health",
				"/",
			} {
				resp := do(router, httptest.NewRequest(http.MethodGet, path, nil))
				body := readBody(t, resp)
				assert.Equal(t, http.StatusOK, resp.StatusCode,
					"GET %s must succeed without credentials (body: %.80s)", path, body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

func TestAdminListing_EmptyCacheHasNoRows(t *testing.T) {
	up := newAdminUpstream(t, "pkg", adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(64)})
	router := newAdminServer(t, up.URL, withCredentials).Router()

	resp := authGet(router, "/admin")
	body := string(readBody(t, resp))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, body, "data-package=", "an empty cache must render no rows")
	assert.Contains(t, body, "Nothing cached.")
}

func TestAdminListing_ShowsDownloadedPackageWithSizeAndAge(t *testing.T) {
	payload := testPayload(4096)
	up := newAdminUpstream(t, "listme", adminFakeFile{name: "listme-1.0.0.tar.gz", body: payload})
	srv := newAdminServer(t, up.URL, withCredentials)
	router := srv.Router()

	_ = readBody(t, getFile(router, "listme", "listme-1.0.0.tar.gz"))
	waitFor(t, "the download to reach the cache snapshot", func() bool {
		return len(srv.snapshotView().Packages) == 1
	})

	body := string(readBody(t, authGet(router, "/admin/rows")))
	assert.Contains(t, body, `data-package="listme"`)
	assert.Contains(t, body, "listme-1.0.0.tar.gz")
	assert.Contains(t, body, "4.0 KB", "the row must carry the file size")

	view := srv.snapshotView()
	require.Len(t, view.Packages, 1)
	require.Len(t, view.Packages[0].Files, 1)
	assert.Equal(t, int64(len(payload)), view.Packages[0].Files[0].Size)
	assert.NotEmpty(t, view.Packages[0].Files[0].Age, "the row must carry an age")
}

func TestAdminListing_HitCountTracksServes(t *testing.T) {
	up := newAdminUpstream(t, "hitme", adminFakeFile{name: "hitme-1.0.0.tar.gz", body: testPayload(2048)})
	srv := newAdminServer(t, up.URL, withCredentials)
	router := srv.Router()
	cacheDir := srv.config.CacheDir

	// The first request populates the cache; it is a write, not a serve.
	_ = readBody(t, getFile(router, "hitme", "hitme-1.0.0.tar.gz"))
	waitCached(t, cacheDir, "hitme", "hitme-1.0.0.tar.gz")

	const serves = 3
	for range serves {
		resp := getFile(router, "hitme", "hitme-1.0.0.tar.gz")
		_ = readBody(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	view := srv.snapshotView()
	require.Len(t, view.Packages, 1)
	assert.Equal(t, int64(serves), view.Packages[0].Hits,
		"the hit count must reflect how many times the object was served from cache")
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
	srv := newAdminServer(t, up.URL, withCredentials)
	router := srv.Router()

	resp := postPrefetch(router, "warmme", true)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "prefetch must return immediately")

	waitFor(t, "prefetched files to appear in the polled rows", func() bool {
		return strings.Contains(string(readBody(t, authGet(router, "/admin/rows"))), "warmme-1.0.0-py3-none-any.whl")
	})

	assert.Equal(t, []string{"warmme-1.0.0-py3-none-any.whl", "warmme-1.0.0.tar.gz"}, cachedFileNames(srv))
	assert.Empty(t, srv.snapshotView().Errors)
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
	srv := newAdminServer(t, up.URL, withCredentials)

	resp := postPrefetch(srv.Router(), "manyver", true)
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
	srv := newAdminServer(t, up.URL, withCredentials)

	_ = readBody(t, postPrefetch(srv.Router(), "prerel", true))

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
	srv := newAdminServer(t, up.URL, withCredentials)

	_ = readBody(t, postPrefetch(srv.Router(), "yanked", true))

	want := []string{"yanked-1.10.0.tar.gz"}
	waitFor(t, "the unyanked file", func() bool { return len(cachedFileNames(srv)) == len(want) })
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, want, cachedFileNames(srv), "a yanked file must not be prefetched")
}

func TestPrefetch_UpstreamFailureLeavesNoFilesAndIsSurfaced(t *testing.T) {
	up := newAdminUpstream(t, "brokenpkg", adminFakeFile{name: "brokenpkg-1.0.0.tar.gz", body: testPayload(4096)})
	up.failFiles.Store(true)

	srv := newAdminServer(t, up.URL, withCredentials)
	router := srv.Router()

	resp := postPrefetch(router, "brokenpkg", true)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	waitFor(t, "the failure to appear in the page's error area", func() bool {
		return strings.Contains(string(readBody(t, authGet(router, "/admin"))), "brokenpkg-1.0.0.tar.gz")
	})

	body := string(readBody(t, authGet(router, "/admin")))
	assert.Contains(t, body, "Recent failures")
	assert.Empty(t, cachedFileNames(srv), "a failed prefetch must leave no partial files")
	assertNotCached(t, srv.config.CacheDir, "brokenpkg", "brokenpkg-1.0.0.tar.gz")
}

func TestPrefetch_UnknownPackageIsSurfaced(t *testing.T) {
	up := newAdminUpstream(t, "known", adminFakeFile{name: "known-1.0.0.tar.gz", body: testPayload(64)})
	srv := newAdminServer(t, up.URL, withCredentials)
	router := srv.Router()

	_ = readBody(t, postPrefetch(router, "nosuchpackage", true))

	waitFor(t, "the not-found failure to be surfaced", func() bool {
		return strings.Contains(string(readBody(t, authGet(router, "/admin/rows"))), "nosuchpackage")
	})
}

func TestPrefetch_RequiresPackageName(t *testing.T) {
	up := newAdminUpstream(t, "pkg", adminFakeFile{name: "pkg-1.0.0.tar.gz", body: testPayload(64)})
	router := newAdminServer(t, up.URL, withCredentials).Router()

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
	router := newAdminServer(t, up.URL, withCredentials).Router()

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
