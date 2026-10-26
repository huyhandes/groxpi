package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

func newProxyServer(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	srv := newTestServer(t, &config.Config{
		IndexURL:        upstreamURL,
		CacheDir:        t.TempDir(),
		CacheSize:       1 << 30,
		IndexTTL:        time.Hour,
		IndexCacheSize:  1 << 20,
		DownloadTimeout: time.Second,
		LogLevel:        "ERROR",
	})
	t.Cleanup(func() { _ = srv.Close() })
	return srv.Router()
}

func getRoot(router http.Handler, accept string) *http.Response {
	req := httptest.NewRequest(http.MethodGet, "/simple/", nil)
	req.Header.Set("Accept", accept)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

// TestRootIndex_NotCachedButCoalesced pins both halves of the root-proxy
// contract: requests spaced in time each reach upstream, a concurrent burst
// reaches it once.
func TestRootIndex_NotCachedButCoalesced(t *testing.T) {
	up := NewFakePyPI(t).SetDelay(20 * time.Millisecond)
	router := newProxyServer(t, up.URL)

	const acceptJSON = "application/vnd.pypi.simple.v1+json"
	for range 2 {
		resp := getRoot(router, acceptJSON)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, string(readBody(t, resp)), "demo")
	}
	require.EqualValues(t, 2, up.RootHits(), "the root index must not be cached")

	before := up.RootHits()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := getRoot(router, acceptJSON)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, up.RootHits()-before, "a concurrent burst must share one upstream fetch")
}

// TestRootIndex_HTMLIsTheUpstreamList: a browser-style accept header must get
// the upstream's actual listing, not a fixed string.
func TestRootIndex_HTMLIsTheUpstreamList(t *testing.T) {
	up := NewFakePyPI(t)
	router := newProxyServer(t, up.URL)

	resp := getRoot(router, "text/html")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	body := string(readBody(t, resp))
	assert.Contains(t, body, "demo")
	assert.NotContains(t, body, "No packages cached yet")
}

// TestCompression_IndexCompressedPackageFileNot: the pre-built compressed index
// body is served when the client accepts it; an already-compressed archive is
// never compressed.
func TestCompression_IndexCompressedPackageFileNot(t *testing.T) {
	payload := testPayload(64 * 1024)
	up := NewFakePyPI(t).Add("zippy", "zippy-1.0.0.tar.gz", FakeFile{SHA256: Sum(payload), Size: int64(len(payload)),
		Handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) },
	})
	router, _ := newCorrectnessServer(t, up.URL, time.Second)

	req := httptest.NewRequest(http.MethodGet, "/simple/zippy/?format=application/vnd.pypi.simple.v1+json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))

	zr, err := gzip.NewReader(bytes.NewReader(readBody(t, resp)))
	require.NoError(t, err)
	decoded, err := io.ReadAll(zr)
	require.NoError(t, err)

	// The compressed form must decode to exactly the plain form.
	assert.Equal(t, string(getIndex(t, router, "zippy")), string(decoded))

	req = httptest.NewRequest(http.MethodGet, "/simple/zippy/zippy-1.0.0.tar.gz", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp = w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, resp.Header.Get("Content-Encoding"), "gzip")
	assert.Equal(t, payload, readBody(t, resp))
}
