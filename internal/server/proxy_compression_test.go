package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

// fakeRootIndex serves the upstream root listing in whatever content type the
// client asked for, counting fetches.
type fakeRootIndex struct {
	*httptest.Server
	hits atomic.Int64
}

func newFakeRootIndex(t *testing.T, delay time.Duration) *fakeRootIndex {
	t.Helper()
	f := &fakeRootIndex{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.hits.Add(1)
		time.Sleep(delay)
		if strings.Contains(r.Header.Get("Accept"), "json") {
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_, _ = io.WriteString(w, `{"meta":{"api-version":"1.0"},"projects":[{"name":"flask"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><body><a href="/simple/flask/">flask</a></body></html>`)
	}))
	t.Cleanup(f.Close)
	return f
}

func newProxyServer(t *testing.T, upstreamURL string) *gin.Engine {
	t.Helper()
	srv := New(&config.Config{
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
	up := newFakeRootIndex(t, 20*time.Millisecond)
	router := newProxyServer(t, up.URL)

	const acceptJSON = "application/vnd.pypi.simple.v1+json"
	for range 2 {
		resp := getRoot(router, acceptJSON)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, string(readBody(t, resp)), "flask")
	}
	require.EqualValues(t, 2, up.hits.Load(), "the root index must not be cached")

	before := up.hits.Load()
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
	assert.EqualValues(t, 1, up.hits.Load()-before, "a concurrent burst must share one upstream fetch")
}

// TestRootIndex_HTMLIsTheUpstreamList: a browser-style accept header must get
// the upstream's actual listing, not a fixed string.
func TestRootIndex_HTMLIsTheUpstreamList(t *testing.T) {
	up := newFakeRootIndex(t, 0)
	router := newProxyServer(t, up.URL)

	resp := getRoot(router, "text/html")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	body := string(readBody(t, resp))
	assert.Contains(t, body, "flask")
	assert.NotContains(t, body, "No packages cached yet")
}

// TestCompression_IndexCompressedPackageFileNot: the pre-built compressed index
// body is served when the client accepts it; an already-compressed archive is
// never compressed.
func TestCompression_IndexCompressedPackageFileNot(t *testing.T) {
	payload := testPayload(64 * 1024)
	up := newFakeUpstream(t, upstreamSpec{
		pkg: "zippy", file: "zippy-1.0.0.tar.gz",
		sha256: sha256Hex(payload), indexSize: int64(len(payload)),
		serveFile: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) },
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
