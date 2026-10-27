package index

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/huyhandes/groxpi/internal/config"
)

// fakeIndexServer is an HTTP upstream index that counts how often each package
// page is fetched. Only tests of what the real client does on the wire use it;
// everything else runs on fakeUpstream inside a synctest bubble.
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

// newTestService builds the index module over a real HTTP upstream.
func newTestService(t *testing.T, cfg *config.Config) *Service {
	t.Helper()
	svc := New(cfg)
	t.Cleanup(svc.Close)
	return svc
}

// primaryOnly is a single-index configuration for the fake upstream.
func primaryOnly(ttl time.Duration, cacheBytes int64) *config.Config {
	return &config.Config{IndexURL: primaryURL, IndexTTL: ttl, IndexCacheSize: cacheBytes}
}

// oneFile is the answer of an index that has pkg with a single sdist.
func oneFile(pkg string) []FileInfo {
	return []FileInfo{{Name: pkg + "-1.0.tar.gz", URL: "https://files.test/" + pkg + "-1.0.tar.gz"}}
}

// TestResolve_RepeatedRequestServesIdenticalBytesFromOneFetch covers the
// merged entry: the body is marshalled once at fill time and every later
// request gets the same bytes, with a gzip form that decodes to them.
func TestResolve_RepeatedRequestServesIdenticalBytesFromOneFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := newFakeUpstream()
		up.set(primaryURL, "numpy", oneFile("numpy"), nil)
		svc := newFakeService(t, primaryOnly(time.Hour, 1<<20), up)

		first, err := svc.Resolve(t.Context(), "numpy")
		require.NoError(t, err)
		second, err := svc.Resolve(t.Context(), "numpy")
		require.NoError(t, err)

		assert.Equal(t, string(first.JSON), string(second.JSON), "cached responses must be byte-identical")
		assert.Equal(t, 1, up.queried(primaryURL, "numpy"), "the second request must be served from cache")

		zr, err := gzip.NewReader(bytes.NewReader(second.GZIP))
		require.NoError(t, err)
		unzipped, err := io.ReadAll(zr)
		require.NoError(t, err)
		assert.Equal(t, string(second.JSON), string(unzipped))
	})
}

// TestResolve_ExpiresAfterTheIndexTTL pins the positive TTL: an entry is served
// from cache up to its index's TTL and refetched after it.
func TestResolve_ExpiresAfterTheIndexTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := newFakeUpstream()
		up.set(primaryURL, "numpy", oneFile("numpy"), nil)
		svc := newFakeService(t, primaryOnly(time.Hour, 1<<20), up)

		_, err := svc.Resolve(t.Context(), "numpy")
		require.NoError(t, err)
		time.Sleep(time.Hour)
		_, err = svc.Resolve(t.Context(), "numpy")
		require.NoError(t, err)
		assert.Equal(t, 1, up.queried(primaryURL, "numpy"), "an entry is fresh up to its TTL")

		time.Sleep(time.Nanosecond)
		_, err = svc.Resolve(t.Context(), "numpy")
		require.NoError(t, err)
		assert.Equal(t, 2, up.queried(primaryURL, "numpy"), "an expired package must be refetched")
	})
}

// TestResolve_ByteBudgetForcesRefetch covers the bound: once the budget is
// exceeded the least recently used package is dropped and has to be refetched.
func TestResolve_ByteBudgetForcesRefetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := newFakeUpstream()
		packages := []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff"}
		for _, pkg := range packages {
			up.set(primaryURL, pkg, oneFile(pkg), nil)
		}
		// Small enough that a couple of one-file indexes blow the budget.
		svc := newFakeService(t, primaryOnly(time.Hour, 600), up)

		for _, pkg := range packages {
			_, err := svc.Resolve(t.Context(), pkg)
			require.NoError(t, err)
			time.Sleep(time.Millisecond) // distinguish recency; instant in the bubble
		}

		_, err := svc.Resolve(t.Context(), packages[0])
		require.NoError(t, err)
		assert.Equal(t, 2, up.queried(primaryURL, packages[0]),
			"the earliest package should have been evicted and refetched")
	})
}

// TestResolve_NormalisesTheName pins c8: any spelling of a name is one index
// entry and one upstream query, and either spelling invalidates it.
func TestResolve_NormalisesTheName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := newFakeUpstream()
		up.set(primaryURL, "foo-bar", oneFile("foo-bar"), nil)
		svc := newFakeService(t, primaryOnly(time.Hour, 1<<20), up)

		resolve := func(name string) {
			t.Helper()
			_, err := svc.Resolve(t.Context(), name)
			require.NoError(t, err)
		}

		resolve("Foo_Bar")
		resolve("foo-bar")
		assert.Equal(t, 1, up.queried(primaryURL, "foo-bar"), "both spellings share one upstream query")
		assert.Zero(t, up.queried(primaryURL, "Foo_Bar"), "upstream is only asked for the normalised name")

		svc.Invalidate("Foo.Bar")
		resolve("foo-bar")
		assert.Equal(t, 2, up.queried(primaryURL, "foo-bar"), "invalidating another spelling clears the entry")

		svc.Invalidate("foo-bar")
		resolve("FOO__BAR")
		assert.Equal(t, 3, up.queried(primaryURL, "foo-bar"))
	})
}

// The OpenTelemetry global delegates once per process, so the in-memory meter
// provider is installed once for the index test binary.
var (
	testMetricsOnce sync.Once
	testMetrics     *sdkmetric.ManualReader
)

// indexCacheHits reads the index-layer cache hit counter.
func indexCacheHits(t *testing.T) int64 {
	t.Helper()
	testMetricsOnce.Do(func() {
		testMetrics = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMetrics)))
	})
	var rm metricdata.ResourceMetrics
	require.NoError(t, testMetrics.Collect(context.Background(), &rm))
	var hits int64
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != "groxpi.cache.hits" {
				continue
			}
			for _, dp := range sum.DataPoints {
				if layer, _ := dp.Attributes.Value("cache.layer"); layer.AsString() == "index" {
					hits += dp.Value
				}
			}
		}
	}
	return hits
}

// TestResolve_CachedMissCountsAsAnIndexCacheHit pins that negative caching
// keeps the metric surface: a miss served from cache is an index-layer hit.
func TestResolve_CachedMissCountsAsAnIndexCacheHit(t *testing.T) {
	before := indexCacheHits(t)
	synctest.Test(t, func(t *testing.T) {
		svc := newFakeService(t, primaryOnly(time.Hour, 1<<20), newFakeUpstream())
		for range 3 {
			_, err := svc.Resolve(t.Context(), "ghost")
			require.ErrorIs(t, err, ErrNotFound)
		}
	})
	assert.Equal(t, int64(2), indexCacheHits(t)-before, "the second and third lookups are served from cache")
}

// TestResolve_UpstreamFailureIsRedacted pins that index returns already-redacted
// errors: whether a credentialed index fails at the transport or with an HTTP
// error, neither its password nor a token-as-username reaches the error text.
func TestResolve_UpstreamFailureIsRedacted(t *testing.T) {
	const secret = "sup3rs3cr3t"
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	for _, base := range []string{failing.URL, closed.URL} {
		for _, user := range []*url.Userinfo{url.UserPassword("deploy", secret), url.User(secret)} {
			u, err := url.Parse(base)
			require.NoError(t, err)
			u.User = user
			svc := newTestService(t, &config.Config{
				IndexURL:        u.String(),
				IndexTTL:        time.Minute,
				IndexCacheSize:  1 << 20,
				DownloadTimeout: time.Second,
				LogLevel:        "ERROR",
			})

			_, err = svc.Resolve(t.Context(), "numpy")
			require.Error(t, err, u.Redacted())
			assert.NotContains(t, err.Error(), secret, u.Redacted())
		}
	}
}

// TestResolve_OversizedIndexBodyFailsAndIsNotCached pins the per-package body
// cap: a body past it is an error, never a truncated list, and nothing is cached
// so the next request goes upstream again.
func TestResolve_OversizedIndexBodyFailsAndIsNotCached(t *testing.T) {
	upstream := newFakeIndexServer(t)
	svc := newTestService(t, &config.Config{
		IndexURL:       upstream.URL,
		IndexTTL:       time.Minute,
		IndexCacheSize: 1 << 20,
		LogLevel:       "ERROR",
	})

	saved := maxPackageIndexBytes
	t.Cleanup(func() { maxPackageIndexBytes = saved })
	maxPackageIndexBytes = 16

	_, err := svc.Resolve(t.Context(), "numpy")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "byte limit")

	maxPackageIndexBytes = saved
	_, err = svc.Resolve(t.Context(), "numpy")
	require.NoError(t, err)
	assert.Equal(t, 2, upstream.hitsFor("numpy"), "an oversized body must not be cached")
}

// TestResolve_ZeroTTLCachesNothing pins GROXPI_INDEX_TTL=0 as "do not cache":
// every resolve goes upstream and nothing piles up in the cache.
func TestResolve_ZeroTTLCachesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := newFakeUpstream()
		up.set(primaryURL, "numpy", oneFile("numpy"), nil)
		svc := newFakeService(t, primaryOnly(0, 1<<20), up)

		for i := range 50 {
			_, err := svc.Resolve(t.Context(), "numpy")
			require.NoError(t, err)
			_, err = svc.Resolve(t.Context(), fmt.Sprintf("ghost-%d", i))
			require.ErrorIs(t, err, ErrNotFound)
		}
		n, b := svc.cache.stats()
		assert.Zero(t, n, "TTL 0 must store no entries")
		assert.Zero(t, b)
		assert.Equal(t, 50, up.queried(primaryURL, "numpy"), "every resolve goes upstream")
	})
}

// TestResolve_ExtraTTLIsSweptWhenPrimaryTTLIsZero pins the sweep cadence: it
// runs at the shortest positive TTL in the resolution order, so an extra index's
// entries expire without a read even when the primary's TTL is zero.
func TestResolve_ExtraTTLIsSweptWhenPrimaryTTLIsZero(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := newFakeUpstream()
		up.set(extraURL, "internal", oneFile("internal"), nil)
		svc := newFakeService(t, &config.Config{
			IndexURL:       primaryURL,
			IndexTTL:       0,
			ExtraIndexURLs: []string{extraURL},
			ExtraIndexTTLs: []time.Duration{time.Minute},
			IndexCacheSize: 1 << 20,
		}, up)

		_, err := svc.Resolve(t.Context(), "internal")
		require.NoError(t, err)
		n, _ := svc.cache.stats()
		require.Equal(t, 1, n, "the extra's positive TTL caches the entry")

		time.Sleep(2*time.Minute + time.Nanosecond)
		synctest.Wait()
		n, b := svc.cache.stats()
		assert.Zero(t, n, "the sweep must drop the expired extra entry without a read")
		assert.Zero(t, b)
	})
}

// gatedUpstream holds every query until release is closed, ignoring the query's
// context: the shared fetch runs detached, so nothing should cancel it.
type gatedUpstream struct {
	*fakeUpstream
	release chan struct{}
}

func (g *gatedUpstream) GetPackageFiles(ctx context.Context, index config.Index, pkg string) ([]FileInfo, error) {
	<-g.release
	return g.fakeUpstream.GetPackageFiles(ctx, index, pkg)
}

// TestResolve_WaiterCanLeaveASharedFetch: a caller waiting on a lookup another
// caller started returns as soon as its own context ends, while the shared
// fetch carries on for the callers still waiting and fills the cache.
func TestResolve_WaiterCanLeaveASharedFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := &gatedUpstream{fakeUpstream: newFakeUpstream(), release: make(chan struct{})}
		up.set(primaryURL, "numpy", oneFile("numpy"), nil)
		svc := newFakeService(t, primaryOnly(time.Hour, 1<<20), up)

		ctx, cancel := context.WithCancel(t.Context())
		left := make(chan error, 1)
		go func() { _, err := svc.Resolve(ctx, "numpy"); left <- err }()
		stayed := make(chan error, 1)
		go func() { _, err := svc.Resolve(t.Context(), "numpy"); stayed <- err }()
		synctest.Wait()

		cancel()
		synctest.Wait()
		select {
		case err := <-left:
			require.ErrorIs(t, err, context.Canceled)
		default:
			t.Fatal("a caller whose context ended is still waiting on the shared fetch")
		}

		close(up.release)
		require.NoError(t, <-stayed, "the shared fetch must outlive the caller that started it")
		_, err := svc.Resolve(t.Context(), "numpy")
		require.NoError(t, err)
		assert.Equal(t, 1, up.queried(primaryURL, "numpy"), "the shared fetch filled the cache")
	})
}

// TestProxyRoot_WaiterCanLeaveASharedFetch: as for Resolve, a caller of the
// shared root fetch can leave without failing the callers still waiting.
func TestProxyRoot_WaiterCanLeaveASharedFetch(t *testing.T) {
	release := make(chan struct{})
	var hits sync.Mutex
	n := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Lock()
		n++
		hits.Unlock()
		<-release
		_, _ = io.WriteString(w, "root")
	}))
	t.Cleanup(up.Close)
	svc := newTestService(t, &config.Config{IndexURL: up.URL, IndexCacheSize: 1 << 20})

	ctx, cancel := context.WithCancel(t.Context())
	left := make(chan error, 1)
	go func() { _, err := svc.ProxyRoot(ctx, "", ""); left <- err }()
	require.Eventually(t, func() bool { hits.Lock(); defer hits.Unlock(); return n == 1 },
		5*time.Second, time.Millisecond, "the root fetch never started")
	stayed := make(chan *RootResponse, 1)
	go func() { resp, _ := svc.ProxyRoot(t.Context(), "", ""); stayed <- resp }()

	cancel()
	select {
	case err := <-left:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("a caller whose context ended is still waiting on the shared fetch")
	}

	close(release)
	resp := <-stayed
	require.NotNil(t, resp, "the shared fetch must outlive the caller that started it")
	assert.Equal(t, "root", string(resp.Body))
}
