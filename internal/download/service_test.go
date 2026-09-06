package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/index"
	"github.com/huyhandes/groxpi/internal/storage"
)

// --- test doubles -----------------------------------------------------------

// fakeStorage is a minimal in-memory storage.Storage for decision-tree tests.
// It implements the core interface and nothing else: no GetFilePath, so it must
// not be picked up as ZeroCopyCapable and the handler has to take the plain
// open-then-stream path.
type fakeStorage struct {
	mu        sync.Mutex
	objects   map[string][]byte
	existsErr error
	etag      string // reported verbatim, so tests can mimic each backend's shape
}

func newFakeStorage(keys ...string) *fakeStorage {
	fs := &fakeStorage{objects: make(map[string][]byte, len(keys))}
	for _, k := range keys {
		fs.objects[k] = []byte("cached")
	}
	return fs
}

func (f *fakeStorage) Exists(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.existsErr != nil {
		return false, f.existsErr
	}
	_, ok := f.objects[key]
	return ok, nil
}

func (f *fakeStorage) Put(_ context.Context, key string, reader io.Reader, _ int64, _ string) (*storage.ObjectInfo, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
	return &storage.ObjectInfo{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeStorage) Get(_ context.Context, key string) (io.ReadCloser, *storage.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", storage.ErrNotFound, key)
	}
	return io.NopCloser(bytes.NewReader(data)), &storage.ObjectInfo{Key: key, Size: int64(len(data)), ETag: f.etag}, nil
}

func (f *fakeStorage) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakeStorage) Stat(_ context.Context, key string) (*storage.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, key)
	}
	return &storage.ObjectInfo{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeStorage) Close() error { return nil }

var _ storage.Storage = (*fakeStorage)(nil)

// fakeResolver is the Resolver double: a fixed package listing plus a counter.
type fakeResolver struct {
	files map[string][]index.FileInfo
	calls atomic.Int64
}

func (f *fakeResolver) Resolve(_ context.Context, packageName string) (*index.Entry, error) {
	f.calls.Add(1)
	files, ok := f.files[packageName]
	if !ok {
		return nil, fmt.Errorf("%w: %s", index.ErrNotFound, packageName)
	}
	return &index.Entry{Files: files}, nil
}

func resolverWith(pkg string, files ...index.FileInfo) *fakeResolver {
	return &fakeResolver{files: map[string][]index.FileInfo{pkg: files}}
}

// fakeUpstream is a file host: one body, an optional delay before it answers,
// a failure switch and a hit counter.
type fakeUpstream struct {
	*httptest.Server
	body  []byte
	delay time.Duration
	fail  atomic.Bool
	calls atomic.Int64
}

func newFakeUpstream(t *testing.T, body []byte, delay time.Duration) *fakeUpstream {
	t.Helper()
	up := &fakeUpstream{body: body, delay: delay}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up.calls.Add(1)
		time.Sleep(up.delay)
		if up.fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(up.body)))
		_, _ = w.Write(up.body)
	}))
	t.Cleanup(up.Close)
	return up
}

func newTestService(t *testing.T, st storage.Storage, resolver Resolver, downloadTimeout time.Duration) *Service {
	t.Helper()
	return New(&config.Config{DownloadTimeout: downloadTimeout}, st, resolver)
}

func streamPlan(url string) ServePlan {
	return ServePlan{Action: ActionStreamAndCache, PackageName: "p", FileName: "f", StorageKey: "packages/p/f", URL: url, Size: -1, Timeout: time.Minute}
}

// --- Plan decision tree ------------------------------------------------------

func TestPlan_DecisionTree(t *testing.T) {
	const pkg = "numpy"
	const file = "numpy-1.26.0.tar.gz"
	const key = "packages/numpy/numpy-1.26.0.tar.gz"

	upstream := index.FileInfo{
		Name:   file,
		URL:    "https://files.example.org/numpy-1.26.0.tar.gz",
		Size:   4096,
		Hashes: map[string]string{"sha256": "abc123"},
	}

	tests := []struct {
		name            string
		storage         *fakeStorage
		resolver        *fakeResolver
		downloadTimeout time.Duration
		wantAction      ServeAction
		wantErr         bool
		wantResolves    int64
		check           func(t *testing.T, plan ServePlan)
	}{
		{
			name:            "storage hit serves the cached object without resolving",
			storage:         newFakeStorage(key),
			resolver:        resolverWith(pkg, upstream),
			downloadTimeout: time.Minute,
			wantAction:      ActionFromStorage,
			wantResolves:    0,
		},
		{
			name:            "storage miss resolves and streams",
			storage:         newFakeStorage(),
			resolver:        resolverWith(pkg, upstream),
			downloadTimeout: time.Minute,
			wantAction:      ActionStreamAndCache,
			wantResolves:    1,
			check: func(t *testing.T, plan ServePlan) {
				assert.Equal(t, upstream.URL, plan.URL)
				assert.Equal(t, int64(4096), plan.Size)
				assert.Equal(t, "application/gzip", plan.ContentType)
				assert.Equal(t, `"abc123"`, plan.ETag)
				assert.Equal(t, "abc123", plan.SHA256)
				assert.Positive(t, plan.Timeout)
			},
		},
		{
			name:            "file missing from the index is not found",
			storage:         newFakeStorage(),
			resolver:        resolverWith(pkg, index.FileInfo{Name: "other-1.0.0.tar.gz", URL: "https://example.org/other"}),
			downloadTimeout: time.Minute,
			wantAction:      ActionNotFound,
			wantResolves:    1,
		},
		{
			name:            "unknown package surfaces an error",
			storage:         newFakeStorage(),
			resolver:        &fakeResolver{files: map[string][]index.FileInfo{}},
			downloadTimeout: time.Minute,
			wantAction:      ActionNotFound,
			wantErr:         true,
			wantResolves:    1,
		},
		{
			name:            "zero download timeout redirects upstream",
			storage:         newFakeStorage(),
			resolver:        resolverWith(pkg, upstream),
			downloadTimeout: 0,
			wantAction:      ActionRedirect,
			wantResolves:    1,
			check: func(t *testing.T, plan ServePlan) {
				assert.Equal(t, upstream.URL, plan.URL)
				assert.Zero(t, plan.Timeout)
			},
		},
		{
			name:            "unknown upstream size is reported as -1",
			storage:         newFakeStorage(),
			resolver:        resolverWith(pkg, index.FileInfo{Name: file, URL: upstream.URL}),
			downloadTimeout: 3 * time.Second,
			wantAction:      ActionStreamAndCache,
			wantResolves:    1,
			check: func(t *testing.T, plan ServePlan) {
				assert.Equal(t, int64(-1), plan.Size)
				assert.Equal(t, 3*time.Second, plan.Timeout)
			},
		},
		{
			name: "storage errors do not abort the pipeline",
			storage: func() *fakeStorage {
				fs := newFakeStorage(key)
				fs.existsErr = errors.New("backend down")
				return fs
			}(),
			resolver:        resolverWith(pkg, upstream),
			downloadTimeout: time.Minute,
			wantAction:      ActionStreamAndCache,
			wantResolves:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(t, tt.storage, tt.resolver, tt.downloadTimeout)

			plan, err := svc.Plan(context.Background(), pkg, file)

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantAction, plan.Action, "action mismatch: got %d", plan.Action)
			assert.Equal(t, pkg, plan.PackageName)
			assert.Equal(t, file, plan.FileName)
			assert.Equal(t, key, plan.StorageKey)
			assert.Equal(t, tt.wantResolves, tt.resolver.calls.Load(), "resolver calls")
			if tt.check != nil {
				tt.check(t, plan)
			}
		})
	}
}

// TestPlan_ETagQuotedOnce: the index hash may arrive bare or quoted; the plan
// carries exactly one layer of quotes either way.
func TestPlan_ETagQuotedOnce(t *testing.T) {
	const pkg, file = "numpy", "numpy-1.26.0.tar.gz"

	for name, hash := range map[string]string{
		"bare index hash":   "abc123",
		"quoted index hash": `"abc123"`,
	} {
		t.Run(name, func(t *testing.T) {
			resolver := resolverWith(pkg, index.FileInfo{Name: file, URL: "https://files.example.org/" + file, Hashes: map[string]string{"sha256": hash}})
			plan, err := newTestService(t, newFakeStorage(), resolver, time.Minute).Plan(context.Background(), pkg, file)
			require.NoError(t, err)
			assert.Equal(t, `"abc123"`, plan.ETag)
		})
	}
}

func TestQuoteETag(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"bare hash is quoted", "abc123", `"abc123"`},
		{"already quoted is left alone", `"abc123"`, `"abc123"`},
		{"lone quote is still quoted", `"`, `"""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, quoteETag(tt.in))
		})
	}
}

// --- Fetch deduplication -----------------------------------------------------

func TestFetch_SingleCallerStreams(t *testing.T) {
	st := newFakeStorage()
	up := newFakeUpstream(t, []byte("payload"), 0)
	svc := newTestService(t, st, resolverWith("p"), time.Minute)

	var buf bytes.Buffer
	result, led, err := svc.Fetch(context.Background(), streamPlan(up.URL), &buf)

	require.NoError(t, err)
	assert.True(t, led, "the only caller must lead the fetch")
	require.NotNil(t, result)
	assert.Equal(t, int64(len("payload")), result.Size)
	assert.Equal(t, "payload", buf.String())
	assert.Equal(t, int64(1), up.calls.Load())
	assert.Empty(t, svc.InFlight(), "the registry entry must be gone once the leader returns")
}

// TestFetch_ConcurrentDedup is the coalescing contract: concurrent requests for
// the same uncached file must not each hit upstream.
func TestFetch_ConcurrentDedup(t *testing.T) {
	st := newFakeStorage()
	up := newFakeUpstream(t, []byte("payload"), 50*time.Millisecond)
	svc := newTestService(t, st, resolverWith("p"), time.Minute)
	plan := streamPlan(up.URL)

	const callers = 10
	var (
		wg      sync.WaitGroup
		leaders atomic.Int64
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			result, led, err := svc.Fetch(context.Background(), plan, &buf)
			assert.NoError(t, err)
			if led {
				leaders.Add(1)
				assert.NotNil(t, result)
				assert.Equal(t, "payload", buf.String())
			} else {
				assert.Nil(t, result, "followers must not receive the leader's result")
				assert.Zero(t, buf.Len(), "followers must not have a body written")
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), up.calls.Load(), "exactly one upstream download for %d concurrent callers", callers)
	assert.Equal(t, int64(1), leaders.Load(), "exactly one caller leads")

	exists, err := st.Exists(context.Background(), plan.StorageKey)
	require.NoError(t, err)
	assert.True(t, exists, "the leader must have populated storage for the followers")
}

// TestFetch_InFlightRegistry pins the admin view of a running download: it is
// listed while running, counts the coalesced requests, and disappears after.
func TestFetch_InFlightRegistry(t *testing.T) {
	up := newFakeUpstream(t, []byte("payload"), 150*time.Millisecond)
	svc := newTestService(t, newFakeStorage(), resolverWith("p"), time.Minute)
	plan := streamPlan(up.URL)

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = svc.Fetch(context.Background(), plan, io.Discard)
		}()
	}

	require.Eventually(t, func() bool {
		running := svc.InFlight()
		return len(running) == 1 && running[0].Requests == 3
	}, time.Second, 5*time.Millisecond, "the download and its three coalesced requests must be listed while running")
	running := svc.InFlight()[0]
	assert.Equal(t, "p", running.Package)
	assert.Equal(t, "f", running.File)
	assert.False(t, running.Started.IsZero())

	wg.Wait()
	assert.Empty(t, svc.InFlight())
}

func TestFetch_LeaderFailurePropagates(t *testing.T) {
	up := newFakeUpstream(t, nil, 20*time.Millisecond)
	up.fail.Store(true)
	svc := newTestService(t, newFakeStorage(), resolverWith("p"), time.Minute)
	plan := streamPlan(up.URL)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = svc.Fetch(context.Background(), plan, io.Discard)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		assert.Error(t, err, "caller %d should see the shared failure", i)
	}
	assert.Equal(t, int64(1), up.calls.Load())
}

// TestFetch_SurvivesClientCancellation pins the rule that the cache-population
// write is not cancelled just because the triggering client disconnected.
func TestFetch_SurvivesClientCancellation(t *testing.T) {
	st := newFakeStorage()
	up := newFakeUpstream(t, []byte("payload"), 30*time.Millisecond)
	svc := newTestService(t, st, resolverWith("p"), time.Minute)
	plan := streamPlan(up.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client already gone

	result, led, err := svc.Fetch(ctx, plan, io.Discard)
	require.NoError(t, err)
	assert.True(t, led)
	require.NotNil(t, result)

	exists, err := st.Exists(context.Background(), plan.StorageKey)
	require.NoError(t, err)
	assert.True(t, exists, "cache population must complete despite client cancellation")
}

// TestFetch_NoStaleDedupState: dedup bookkeeping must not outlive the download
// it coordinated.
func TestFetch_NoStaleDedupState(t *testing.T) {
	up := newFakeUpstream(t, []byte("payload"), 0)
	svc := newTestService(t, newFakeStorage(), resolverWith("p"), time.Minute)
	plan := streamPlan(up.URL)

	_, led, err := svc.Fetch(context.Background(), plan, io.Discard)
	require.NoError(t, err)
	require.True(t, led)

	_, led, err = svc.Fetch(context.Background(), plan, io.Discard)
	require.NoError(t, err)
	assert.True(t, led, "dedup state must be released once the download completes")
	assert.Equal(t, int64(2), up.calls.Load())
}

// --- Warm ---------------------------------------------------------------------

func TestWarm(t *testing.T) {
	const pkg, file = "p", "p-1.0.0.tar.gz"
	body := []byte("payload")
	up := newFakeUpstream(t, body, 0)
	listed := index.FileInfo{Name: file, URL: up.URL + "/" + file, Size: int64(len(body))}

	t.Run("downloads an uncached listed file into storage", func(t *testing.T) {
		st := newFakeStorage()
		svc := newTestService(t, st, resolverWith(pkg, listed), time.Minute)
		require.NoError(t, svc.Warm(context.Background(), pkg, file))
		exists, _ := st.Exists(context.Background(), StorageKey(pkg, file))
		assert.True(t, exists)
	})
	t.Run("is a no-op for a cached file", func(t *testing.T) {
		before := up.calls.Load()
		svc := newTestService(t, newFakeStorage(StorageKey(pkg, file)), resolverWith(pkg, listed), time.Minute)
		require.NoError(t, svc.Warm(context.Background(), pkg, file))
		assert.Equal(t, before, up.calls.Load())
	})
	t.Run("reports ErrNotListed for an unknown file", func(t *testing.T) {
		svc := newTestService(t, newFakeStorage(), resolverWith(pkg, listed), time.Minute)
		assert.ErrorIs(t, svc.Warm(context.Background(), pkg, "nope.whl"), ErrNotListed)
	})
	t.Run("reports ErrCachingDisabled when downloads redirect", func(t *testing.T) {
		svc := newTestService(t, newFakeStorage(), resolverWith(pkg, listed), 0)
		assert.ErrorIs(t, svc.Warm(context.Background(), pkg, file), ErrCachingDisabled)
	})
}

// --- HTTP surface --------------------------------------------------------------

// TestHandler_StreamAndCache_FailedFetchRedirects: a fetch that fails before the
// first body byte falls back to a redirect rather than an empty 200.
func TestHandler_StreamAndCache_FailedFetchRedirects(t *testing.T) {
	up := newFakeUpstream(t, nil, 0)
	up.fail.Store(true)
	svc := newTestService(t, newFakeStorage(), resolverWith("p"), time.Minute)
	plan := streamPlan(up.URL + "/f")

	w := httptest.NewRecorder()
	svc.streamAndCache(w, httptest.NewRequest(http.MethodGet, "/index/p/f", nil), plan)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusFound, resp.StatusCode, "the failed fetch must fall back to upstream")
	assert.Equal(t, plan.URL, resp.Header.Get("Location"))
}

// TestServeFromStorage_ETagQuotedOnce pins the double-quoting bug: a local
// backend reports a bare hash while an S3 backend echoes the API's already quoted
// form, and both must come out with exactly one layer of quotes.
func TestServeFromStorage_ETagQuotedOnce(t *testing.T) {
	const key = "packages/numpy/numpy-1.26.0.tar.gz"
	tests := []struct {
		name       string
		backend    string
		wantHeader string
	}{
		{"local-shaped bare hash", "abc123", `"abc123"`},
		{"s3-shaped quoted etag", `"abc123"`, `"abc123"`},
		{"no etag omits the header", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStorage()
			st.etag = tt.backend
			_, err := st.Put(context.Background(), key, strings.NewReader("payload"), 7, "")
			require.NoError(t, err)

			w := httptest.NewRecorder()
			svc := &Service{storage: st}
			svc.serveFromStorage(w, httptest.NewRequest(http.MethodGet, "/"+key, nil), ServePlan{StorageKey: key, FileName: "numpy-1.26.0.tar.gz"})

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, tt.wantHeader, w.Result().Header.Get("ETag"))
		})
	}
}

// newIndexedMux mounts the download module over a real index module pointed at
// a fake PyPI that lists one file.
func newIndexedMux(t *testing.T, pkg, file string, content []byte, serveFile http.HandlerFunc) (*http.ServeMux, *storage.LRULocalStorage) {
	t.Helper()
	return newIndexedMuxWith(t, pkg, file, content, nil, serveFile)
}

// newIndexedMuxWithMetadata is newIndexedMux with the index advertising a PEP
// 658 metadata file whose content is metadata.
func newIndexedMuxWithMetadata(t *testing.T, pkg, file string, metadata []byte, serveFile http.HandlerFunc) (*http.ServeMux, *storage.LRULocalStorage) {
	t.Helper()
	return newIndexedMuxWith(t, pkg, file, []byte("wheel"), map[string]string{"sha256": sha256Hex(metadata)}, serveFile)
}

func newIndexedMuxWith(t *testing.T, pkg, file string, content []byte, metadata map[string]string, serveFile http.HandlerFunc) (*http.ServeMux, *storage.LRULocalStorage) {
	t.Helper()
	var mock *httptest.Server
	mock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+pkg+"/" {
			entry := map[string]any{
				"filename": file,
				"url":      mock.URL + "/files/" + file,
				"size":     len(content),
				"hashes":   map[string]string{"sha256": sha256Hex(content)},
			}
			if metadata != nil {
				entry["core-metadata"] = metadata
			}
			w.Header().Set("Content-Type", index.JSONContentType)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": pkg, "files": []map[string]any{entry}})
			return
		}
		serveFile(w, r)
	}))
	t.Cleanup(mock.Close)

	cfg := &config.Config{IndexURL: mock.URL, IndexTTL: time.Hour, IndexCacheSize: 1 << 20, DownloadTimeout: 10 * time.Second}
	st, err := storage.NewLRULocalStorage(t.TempDir(), 1<<30, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	idx := index.New(cfg)
	t.Cleanup(idx.Close)

	mux := http.NewServeMux()
	New(cfg, st, idx).Register(mux)
	return mux, st
}

// TestHandler_HeadersPrecedeBody covers the bug where Content-Type and
// Content-Length were set after the body had been streamed and silently dropped.
func TestHandler_HeadersPrecedeBody(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 2048)
	mux, _ := newIndexedMux(t, "headertest", "headertest-1.0.0.tar.gz", content, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/index/headertest/headertest-1.0.0.tar.gz", nil))
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, body, len(content))
	assert.Equal(t, "application/gzip", resp.Header.Get("Content-Type"))
	assert.Equal(t, strconv.Itoa(len(content)), resp.Header.Get("Content-Length"))
	assert.Equal(t, `"`+sha256Hex(content)+`"`, resp.Header.Get("ETag"))
}

// TestHandler_ServesFollowersFromStorage is the transport-level coalescing
// contract: every concurrent caller gets the whole file, upstream is hit once.
func TestHandler_ServesFollowersFromStorage(t *testing.T) {
	content := bytes.Repeat([]byte("y"), 64*1024)
	var downloads atomic.Int64
	mux, _ := newIndexedMux(t, "followers", "followers-1.0.0.whl", content, func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		time.Sleep(80 * time.Millisecond)
		_, _ = w.Write(content)
	})

	const callers = 8
	var wg sync.WaitGroup
	sizes := make([]int, callers)
	statuses := make([]int, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/index/followers/followers-1.0.0.whl", nil))
			resp := w.Result()
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			statuses[i] = resp.StatusCode
			sizes[i] = len(body)
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), downloads.Load(), "only the leader may hit upstream")
	for i := range callers {
		assert.Equal(t, http.StatusOK, statuses[i], "caller %d", i)
		assert.Equal(t, len(content), sizes[i], "caller %d must get the full body", i)
	}
}

// TestHandler_RangeOnCachedFile: a cached file served through http.ServeContent
// honours Range requests, which is the point of serving by path.
func TestHandler_RangeOnCachedFile(t *testing.T) {
	content := testBytes(4096)
	mux, _ := newIndexedMux(t, "ranged", "ranged-1.0.0.whl", content, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	})

	// Populate the cache.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/simple/ranged/ranged-1.0.0.whl", nil))
	require.Equal(t, http.StatusOK, w.Code)

	req := httptest.NewRequest(http.MethodGet, "/simple/ranged/ranged-1.0.0.whl", nil)
	req.Header.Set("Range", "bytes=100-199")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, "bytes 100-199/4096", w.Header().Get("Content-Range"))
	assert.Equal(t, "application/zip", w.Header().Get("Content-Type"))
	assert.Equal(t, content[100:200], w.Body.Bytes())
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func testBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// zeroCopyStorage is a fakeStorage that can also name a real file on disk, so
// it satisfies storage.ZeroCopyCapable and the handler must serve it by path
// rather than pulling the bytes through Get.
type zeroCopyStorage struct {
	*fakeStorage
	dir string
}

func (z *zeroCopyStorage) GetFilePath(_ context.Context, key string) (string, error) {
	path := filepath.Join(z.dir, filepath.Base(key))
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%w: %s", storage.ErrNotFound, key)
	}
	return path, nil
}

// TestServeFromStorage_HeadersPrecedeBody pins the ordering contract:
// every response header must be set before the first body byte, because net/http
// commits the header block on the first Write and silently discards whatever
// is set afterwards. httptest.ResponseRecorder snapshots the headers at that
// same moment, so Result().Header sees exactly what the client would.
func TestServeFromStorage_HeadersPrecedeBody(t *testing.T) {
	const key = "packages/numpy/numpy-1.26.0.tar.gz"
	payload := []byte(strings.Repeat("x", 4096))

	serve := func(t *testing.T, st storage.Storage) *http.Response {
		t.Helper()
		w := httptest.NewRecorder()
		s := &Service{storage: st}
		s.serveFromStorage(w, httptest.NewRequest(http.MethodGet, "/"+key, nil), ServePlan{StorageKey: key, FileName: "numpy-1.26.0.tar.gz"})
		return w.Result()
	}

	t.Run("streaming backend", func(t *testing.T) {
		st := newFakeStorage()
		if _, err := st.Put(context.Background(), key, strings.NewReader(string(payload)), int64(len(payload)), ""); err != nil {
			t.Fatalf("seed storage: %v", err)
		}

		resp := serve(t, st)
		defer func() { _ = resp.Body.Close() }()

		if got, want := resp.Header.Get("Content-Length"), strconv.Itoa(len(payload)); got != want {
			t.Errorf("Content-Length written after the body: got %q, want %q", got, want)
		}
		if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("Content-Type written after the body: got %q", got)
		}
		if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "numpy-1.26.0.tar.gz") {
			t.Errorf("Content-Disposition written after the body: got %q", got)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(body) != string(payload) {
			t.Errorf("body truncated: got %d bytes, want %d", len(body), len(payload))
		}
	})

	t.Run("zero-copy backend is served by path", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "numpy-1.26.0.tar.gz"), payload, 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		// The object is deliberately absent from the in-memory map: if the
		// server fell back to streaming it would 404 instead of serving.
		st := &zeroCopyStorage{fakeStorage: newFakeStorage(), dir: dir}

		resp := serve(t, st)
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("zero-copy path not taken: status %d", resp.StatusCode)
		}
		if got, want := resp.Header.Get("Content-Length"), strconv.Itoa(len(payload)); got != want {
			t.Errorf("Content-Length: got %q, want %q", got, want)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(body) != string(payload) {
			t.Errorf("body mismatch: got %d bytes, want %d", len(body), len(payload))
		}
	})

	t.Run("missing object is a 404, not a 500", func(t *testing.T) {
		resp := serve(t, newFakeStorage())
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 for a missing object, got %d", resp.StatusCode)
		}
	})
}
