package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

const (
	adminUser = "op"
	adminPass = "s3cret"
)

func routesConfig(t *testing.T, upstreamURL string, opts ...func(*config.Config)) *config.Config {
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

// newTestServer builds a server from cfg, fails the test if that errors, and
// closes it when the test ends (Close is idempotent, so an explicit Close in the
// test is harmless). Every index it is pointed at must be on loopback: a test
// that reaches the real network is slow, flaky and fails offline.
func newTestServer(tb testing.TB, cfg *config.Config) *Server {
	tb.Helper()
	for _, raw := range append([]string{cfg.IndexURL}, cfg.ExtraIndexURLs...) {
		requireLoopback(tb, raw)
	}
	srv, err := NewServer(cfg)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = srv.Close() })
	return srv
}

// requireLoopback fails the test unless raw is a URL whose host is a loopback
// address. It accepts IP literals only (httptest URLs), so "localhost" fails on
// purpose: a name is resolved, and resolution can leave the host.
func requireLoopback(tb testing.TB, raw string) {
	tb.Helper()
	u, err := url.Parse(raw)
	require.NoError(tb, err, "index URL %q", raw)
	ip := net.ParseIP(u.Hostname())
	require.True(tb, ip != nil && ip.IsLoopback(), "test index URL %q is not on loopback", raw)
}

func newRoutesServer(t *testing.T, upstreamURL string, opts ...func(*config.Config)) http.Handler {
	t.Helper()
	return newTestServer(t, routesConfig(t, upstreamURL, opts...)).Router()
}

func do(router http.Handler, req *http.Request) *http.Response {
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

// TestFileRoute_EncodedTraversalIsNotFound pins A1: an encoded "../" in the
// file name is answered 404 without reading a file planted beside packages/,
// without a redirect and without asking upstream, in fetch and redirect mode.
func TestFileRoute_EncodedTraversalIsNotFound(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(up.Close)

	for _, timeout := range []time.Duration{5 * time.Second, 0} {
		cfg := routesConfig(t, up.URL, func(c *config.Config) { c.DownloadTimeout = timeout })
		require.NoError(t, os.WriteFile(filepath.Join(cfg.CacheDir, "secret"), []byte("planted"), 0o600))
		srv := newTestServer(t, cfg)

		for _, path := range []string{
			"/index/pkg/..%2F..%2Fsecret",
			"/simple/pkg/..%2F..%2Fsecret",
			"/index/pkg/a%5Cb",
			"/index/pkg/.hidden",
		} {
			resp := do(srv.Router(), httptest.NewRequest("GET", path, nil))
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, "%s (timeout %v)", path, timeout)
			assert.Equal(t, "File not found\n", string(body), path)
			assert.Empty(t, resp.Header.Get("Location"), path)
		}
	}
	assert.Zero(t, hits.Load(), "an invalid file name never reaches upstream")
}

// TestEvictRoute_InvalidNameIsBadRequest pins A1 on eviction: a package name
// that is not one path segment is refused with 400 in the JSON error shape.
func TestEvictRoute_InvalidNameIsBadRequest(t *testing.T) {
	router := newRoutesServer(t, "http://127.0.0.1:1", withCredentials)
	req := httptest.NewRequest("DELETE", "/cache/a%2Fb", nil)
	req.SetBasicAuth(adminUser, adminPass)
	resp := do(router, req)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "application/json; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.JSONEq(t, `{"status":"error","message":"Invalid package name"}`, string(body))
}

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
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	router := newRoutesServer(t, up.URL)

	for _, tc := range adminPaths {
		resp := do(router, httptest.NewRequest(tc.method, tc.path, nil))
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode,
			"%s %s must not exist when no credentials are configured", tc.method, tc.path)
	}
}

func TestAdminAuth_WithCredentials_UnauthorizedThenAuthorized(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	router := newRoutesServer(t, up.URL, withCredentials)

	for _, tc := range adminPaths {
		resp := do(router, httptest.NewRequest(tc.method, tc.path, nil))
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"%s %s must require credentials", tc.method, tc.path)
		assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Basic")
	}

	for _, path := range []string{"/admin", "/admin/rows"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth(adminUser, adminPass)
		resp := do(router, req)
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode, path)
	}

	resp := deletePackage(router, "somepkg")
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAdminAuth_EnabledWithoutCredentials_ConstructionFails(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))

	cfg := routesConfig(t, up.URL, func(cfg *config.Config) { cfg.AdminEnabled = true })
	requireLoopback(t, cfg.IndexURL)
	srv, err := NewServer(cfg)
	require.Error(t, err, "enabling the admin interface without credentials must fail construction")
	assert.Nil(t, srv, "a server that cannot be trusted must not be produced")
}

// TestAdminAuth_IndexRoutesStayUnauthenticated catches an over-broad guard:
// whatever the administrative configuration, pip must reach the index without
// credentials.
func TestAdminAuth_IndexRoutesStayUnauthenticated(t *testing.T) {
	const file = "pkg-1.0.0.tar.gz"

	configurations := map[string][]func(*config.Config){
		"no credentials":      nil,
		"credentials":         {withCredentials},
		"enabled+credentials": {withCredentials, func(cfg *config.Config) { cfg.AdminEnabled = true }},
	}

	for name, opts := range configurations {
		t.Run(name, func(t *testing.T) {
			up := newPyPIWith(t, "pkg", file, testPayload(1024))
			router := newRoutesServer(t, up.URL, opts...)

			for _, path := range []string{
				"/simple/pkg/",
				"/index/pkg",
				"/simple/pkg/" + file,
				"/index/pkg/" + file,
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

// TestRoutes_TrailingSlashRedirect pins the mux's behaviour for the PEP 503
// package page: the canonical form ends in a slash, and the bare form is
// redirected to it rather than answered or 404'd.
func TestRoutes_TrailingSlashRedirect(t *testing.T) {
	up := newPyPIWith(t, "numpy", "numpy-1.0.tar.gz", testPayload(64))
	router := newRoutesServer(t, up.URL)

	resp := do(router, httptest.NewRequest(http.MethodGet, "/simple/numpy", nil))
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	assert.Equal(t, "/simple/numpy/", resp.Header.Get("Location"))
	assert.Zero(t, up.IndexHits("numpy"), "a redirect must not consult upstream")
}

// TestRoutes_MethodNotAllowed: a known path with the wrong method is a 405 with
// an Allow header, not a 404.
// TestIndex_FileHrefIsPercentEncoded pins that a file name with a URL
// metacharacter is escaped in both index forms, and that the escaped href
// downloads the file rather than a truncated name.
func TestIndex_FileHrefIsPercentEncoded(t *testing.T) {
	const file = "a#b-1.0-py3-none-any.whl"
	payload := testPayload(64)
	up := NewFakePyPI(t).Add("hashy", file, FakeFile{Body: payload})
	router := newRoutesServer(t, up.URL)
	const want = "/simple/hashy/a%23b-1.0-py3-none-any.whl"

	for _, accept := range []string{"text/html", "application/vnd.pypi.simple.v1+json"} {
		req := httptest.NewRequest(http.MethodGet, "/simple/hashy/", nil)
		req.Header.Set("Accept", accept)
		resp := do(router, req)
		body := string(readBody(t, resp))
		require.Equal(t, http.StatusOK, resp.StatusCode, accept)
		href := regexp.MustCompile(`(?:href="|"url":\s*")([^"]+)"`).FindStringSubmatch(body)
		require.NotNil(t, href, "%s index lists the file: %s", accept, body)
		assert.Equal(t, want, href[1], accept)
	}

	resp := do(router, httptest.NewRequest(http.MethodGet, want, nil))
	got := readBody(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, payload, got)
}

func TestRoutes_MethodNotAllowed(t *testing.T) {
	up := newPyPIWith(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	router := newRoutesServer(t, up.URL)

	resp := do(router, httptest.NewRequest(http.MethodPost, "/health", nil))
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Allow"), "GET")
}

// TestStatusWriter_PreservesReaderFrom is the guard on the sendfile path: the
// wrapper the middleware puts in front of every handler must forward ReadFrom,
// or http.ServeContent silently falls back to a user-space copy.
func TestStatusWriter_PreservesReaderFrom(t *testing.T) {
	var w http.ResponseWriter = &statusWriter{ResponseWriter: httptest.NewRecorder()}
	_, ok := w.(io.ReaderFrom)
	assert.True(t, ok, "statusWriter must implement io.ReaderFrom")

	rc := http.NewResponseController(w)
	assert.NoError(t, rc.Flush(), "Unwrap must expose the underlying writer's Flush")
}

// TestRecoverPanics: a panicking handler answers 500 instead of dropping the
// connection.
func TestRecoverPanics(t *testing.T) {
	h := recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
