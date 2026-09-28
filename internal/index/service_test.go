package index

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
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

// fakeIndexServer is an upstream index that counts how often each package page
// is fetched.
type fakeIndexServer struct {
	*httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	total int
}

func newFakeIndexServer(t *testing.T) *fakeIndexServer {
	t.Helper()
	f := &fakeIndexServer{hits: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path
		f.mu.Lock()
		f.hits[name]++
		f.total++
		f.mu.Unlock()

		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><body>
<a href="/files/%s-1.0.tar.gz" data-requires-python="&gt;=3.8">%s-1.0.tar.gz</a>
</body></html>`, name[1:len(name)-1], name[1:len(name)-1])
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeIndexServer) hitsFor(pkg string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits["/"+pkg+"/"]
}

// newTestService builds the index module over the upstream, with the cache
// reachable.
func newTestService(t *testing.T, cfg *config.Config) *Service {
	t.Helper()
	svc := New(cfg)
	t.Cleanup(svc.Close)
	return svc
}

// getIndex resolves a package and returns the JSON body the server writes for
// it.
func getIndex(t *testing.T, svc *Service, pkg string) []byte {
	t.Helper()
	entry, err := svc.Resolve(context.Background(), pkg)
	require.NoError(t, err)
	return entry.JSON
}

// TestIndexCache_RepeatedRequestServesIdenticalBytesFromOneFetch covers the
// merged entry: the body is marshalled once at fill time and every later request
// writes the same bytes.
func TestIndexCache_RepeatedRequestServesIdenticalBytesFromOneFetch(t *testing.T) {
	upstream := newFakeIndexServer(t)
	svc := newTestService(t, &config.Config{
		IndexURL:        upstream.URL,
		CacheDir:        t.TempDir(),
		IndexTTL:        time.Hour,
		IndexCacheSize:  1 << 20,
		DownloadTimeout: time.Second,
		LogLevel:        "ERROR",
	})

	first := getIndex(t, svc, "numpy")
	second := getIndex(t, svc, "numpy")

	assert.Equal(t, string(first), string(second), "cached responses must be byte-identical")
	assert.Equal(t, 1, upstream.hitsFor("numpy"), "the second request must be served from cache")

	// The compressed representation stored alongside the body must decode to it.
	entry, ok := svc.cache.GetPackage("numpy")
	require.True(t, ok, "the package must be cached")
	assert.Equal(t, string(first), string(entry.JSON))
	zr, err := gzip.NewReader(bytes.NewReader(entry.GZIP))
	require.NoError(t, err)
	unzipped, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Equal(t, string(entry.JSON), string(unzipped))
}

// TestIndexCache_ByteBudgetForcesRefetch covers the bound: once the budget is
// exceeded the least recently used package is dropped and has to be refetched.
func TestIndexCache_ByteBudgetForcesRefetch(t *testing.T) {
	upstream := newFakeIndexServer(t)
	svc := newTestService(t, &config.Config{
		IndexURL: upstream.URL,
		CacheDir: t.TempDir(),
		IndexTTL: time.Hour,
		// Small enough that a couple of one-file indices blow the budget.
		IndexCacheSize:  400,
		DownloadTimeout: time.Second,
		LogLevel:        "ERROR",
	})

	packages := []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff"}
	for _, pkg := range packages {
		getIndex(t, svc, pkg)
		time.Sleep(2 * time.Millisecond) // distinguish recency
	}
	assert.LessOrEqual(t, svc.cache.Bytes(), int64(400), "cache must stay inside its budget")

	getIndex(t, svc, packages[0])
	assert.Equal(t, 2, upstream.hitsFor(packages[0]),
		"the earliest package should have been evicted and refetched")
}

// TestIndexCache_SweepExpiresWithoutARequest covers the sweeper: an entry past
// its TTL is gone before anybody asks for it, and the next request refetches.
func TestIndexCache_SweepExpiresWithoutARequest(t *testing.T) {
	upstream := newFakeIndexServer(t)
	svc := newTestService(t, &config.Config{
		IndexURL:        upstream.URL,
		CacheDir:        t.TempDir(),
		IndexTTL:        20 * time.Millisecond,
		IndexCacheSize:  1 << 20,
		DownloadTimeout: time.Second,
		LogLevel:        "ERROR",
	})

	getIndex(t, svc, "numpy")
	require.Equal(t, 1, svc.cache.Len())

	// No requests at all while we wait: only the background sweep can empty this.
	deadline := time.Now().Add(2 * time.Second)
	for svc.cache.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.Zero(t, svc.cache.Len(), "the sweep should have reclaimed the expired entry")

	getIndex(t, svc, "numpy")
	assert.Equal(t, 2, upstream.hitsFor("numpy"), "an expired package must be refetched")
}
