package server

import (
	"io"
	"net/http"
	"net/http/httptest"
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

func newRoutesServer(t *testing.T, upstreamURL string, opts ...func(*config.Config)) http.Handler {
	t.Helper()
	srv, err := NewServer(routesConfig(t, upstreamURL, opts...))
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return srv.Router()
}

func do(router http.Handler, req *http.Request) *http.Response {
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
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
	up := newCountingUpstream(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
	router := newRoutesServer(t, up.URL)

	for _, tc := range adminPaths {
		resp := do(router, httptest.NewRequest(tc.method, tc.path, nil))
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode,
			"%s %s must not exist when no credentials are configured", tc.method, tc.path)
	}
}

func TestAdminAuth_WithCredentials_UnauthorizedThenAuthorized(t *testing.T) {
	up := newCountingUpstream(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
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
	up := newCountingUpstream(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))

	srv, err := NewServer(routesConfig(t, up.URL, func(cfg *config.Config) { cfg.AdminEnabled = true }))
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
			up := newCountingUpstream(t, "pkg", file, testPayload(1024))
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
	up := newCountingUpstream(t, "numpy", "numpy-1.0.tar.gz", testPayload(64))
	router := newRoutesServer(t, up.URL)

	resp := do(router, httptest.NewRequest(http.MethodGet, "/simple/numpy", nil))
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	assert.Equal(t, "/simple/numpy/", resp.Header.Get("Location"))
	assert.Zero(t, up.indexHits.Load(), "a redirect must not consult upstream")
}

// TestRoutes_MethodNotAllowed: a known path with the wrong method is a 405 with
// an Allow header, not a 404.
func TestRoutes_MethodNotAllowed(t *testing.T) {
	up := newCountingUpstream(t, "pkg", "pkg-1.0.0.tar.gz", testPayload(64))
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
