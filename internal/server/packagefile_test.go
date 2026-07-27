package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/cache"
	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/pypi"
	"github.com/huyhandes/groxpi/internal/storage"
	"github.com/huyhandes/groxpi/internal/streaming"
)

// --- test doubles -----------------------------------------------------------

// fakeStorage is a minimal in-memory storage.Storage for decision-tree tests.
// It implements the core interface and nothing else: no GetFilePath, so it must
// not be picked up as ZeroCopyCapable and the server has to take the plain
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

// fakeIndex is a packageIndex double recording how often upstream was consulted.
type fakeIndex struct {
	files    map[string][]pypi.FileInfo
	packages []string
	err      error
	calls    atomic.Int64
}

func (f *fakeIndex) GetPackageList() ([]string, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.packages, nil
}

func (f *fakeIndex) GetPackageFiles(packageName string) ([]pypi.FileInfo, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	files, ok := f.files[packageName]
	if !ok {
		return nil, fmt.Errorf("package not found: %s", packageName)
	}
	return files, nil
}

// fakeDownloader is a streaming.StreamingDownloader double.
type fakeDownloader struct {
	body      []byte
	err       error
	nilResult bool // report success with no StreamResult
	delay     time.Duration
	calls     atomic.Int64
	storage   *fakeStorage
}

func (f *fakeDownloader) DownloadAndStream(ctx context.Context, _, storageKey string, w io.Writer, _ streaming.Expectation) (*streaming.StreamResult, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.nilResult {
		return nil, nil
	}
	n, err := w.Write(f.body)
	if err != nil {
		return nil, err
	}
	if f.storage != nil {
		if _, err := f.storage.Put(ctx, storageKey, bytes.NewReader(f.body), int64(len(f.body)), "application/octet-stream"); err != nil {
			return nil, err
		}
	}
	return &streaming.StreamResult{Size: int64(n), ContentType: "application/octet-stream", ETag: `"deadbeef"`}, nil
}

// newTestService wires a PackageFileService from doubles, no Gin involved.
func newTestService(t *testing.T, st storage.Storage, index *fakeIndex, dl streaming.StreamingDownloader, downloadTimeout time.Duration) *PackageFileService {
	t.Helper()
	cfg := &config.Config{
		CacheDir:        t.TempDir(),
		CacheSize:       1024 * 1024,
		IndexTTL:        5 * time.Minute,
		DownloadTimeout: downloadTimeout,
	}
	return newPackageFileService(cfg, st, cache.NewIndexCache(), index, dl)
}

func indexWith(pkg string, files ...pypi.FileInfo) *fakeIndex {
	return &fakeIndex{files: map[string][]pypi.FileInfo{pkg: files}}
}

// --- Plan decision tree ------------------------------------------------------

func TestPackageFileService_Plan_DecisionTree(t *testing.T) {
	const pkg = "numpy"
	const file = "numpy-1.26.0.tar.gz"
	const key = "packages/numpy/numpy-1.26.0.tar.gz"

	upstream := pypi.FileInfo{
		Name:   file,
		URL:    "https://files.example.org/numpy-1.26.0.tar.gz",
		Size:   4096,
		Hashes: map[string]string{"sha256": "abc123"},
	}

	tests := []struct {
		name            string
		storage         *fakeStorage
		index           *fakeIndex
		downloadTimeout time.Duration
		seedIndexCache  []pypi.FileInfo
		wantAction      ServeAction
		wantErr         bool
		wantIndexCalls  int64
		check           func(t *testing.T, plan ServePlan)
	}{
		{
			name:            "storage hit serves the cached object",
			storage:         newFakeStorage(key),
			index:           indexWith(pkg, upstream),
			downloadTimeout: time.Minute,
			wantAction:      ActionFromStorage,
			wantIndexCalls:  0,
			check: func(t *testing.T, plan ServePlan) {
				assert.Equal(t, key, plan.StorageKey)
			},
		},
		{
			name:            "index cache hit avoids the upstream index fetch",
			storage:         newFakeStorage(),
			index:           indexWith(pkg, upstream),
			downloadTimeout: time.Minute,
			seedIndexCache:  []pypi.FileInfo{upstream},
			wantAction:      ActionStreamAndCache,
			wantIndexCalls:  0,
			check: func(t *testing.T, plan ServePlan) {
				assert.Equal(t, upstream.URL, plan.URL)
			},
		},
		{
			name:            "index cache miss fetches upstream",
			storage:         newFakeStorage(),
			index:           indexWith(pkg, upstream),
			downloadTimeout: time.Minute,
			wantAction:      ActionStreamAndCache,
			wantIndexCalls:  1,
			check: func(t *testing.T, plan ServePlan) {
				assert.Equal(t, int64(4096), plan.Size)
				assert.Equal(t, "application/gzip", plan.ContentType)
				assert.Equal(t, `"abc123"`, plan.ETag)
				assert.Positive(t, plan.Timeout)
			},
		},
		{
			name:            "file missing from the index is not found",
			storage:         newFakeStorage(),
			index:           indexWith(pkg, pypi.FileInfo{Name: "other-1.0.0.tar.gz", URL: "https://example.org/other"}),
			downloadTimeout: time.Minute,
			wantAction:      ActionNotFound,
			wantIndexCalls:  1,
		},
		{
			name:            "unknown package surfaces an error",
			storage:         newFakeStorage(),
			index:           &fakeIndex{files: map[string][]pypi.FileInfo{}},
			downloadTimeout: time.Minute,
			wantAction:      ActionNotFound,
			wantErr:         true,
			wantIndexCalls:  1,
		},
		{
			name:            "zero download timeout redirects upstream",
			storage:         newFakeStorage(),
			index:           indexWith(pkg, upstream),
			downloadTimeout: 0,
			wantAction:      ActionRedirect,
			wantIndexCalls:  1,
			check: func(t *testing.T, plan ServePlan) {
				assert.Equal(t, upstream.URL, plan.URL)
				assert.Zero(t, plan.Timeout)
			},
		},
		{
			name:            "unknown upstream size falls back to the configured timeout",
			storage:         newFakeStorage(),
			index:           indexWith(pkg, pypi.FileInfo{Name: file, URL: upstream.URL}),
			downloadTimeout: 3 * time.Second,
			wantAction:      ActionStreamAndCache,
			wantIndexCalls:  1,
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
			index:           indexWith(pkg, upstream),
			downloadTimeout: time.Minute,
			wantAction:      ActionStreamAndCache,
			wantIndexCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(t, tt.storage, tt.index, &fakeDownloader{}, tt.downloadTimeout)
			if tt.seedIndexCache != nil {
				svc.indexCache.SetPackage(pkg, tt.seedIndexCache, time.Minute)
			}

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
			assert.Equal(t, tt.wantIndexCalls, tt.index.calls.Load(), "upstream index calls")
			if tt.check != nil {
				tt.check(t, plan)
			}
		})
	}
}

func TestPackageFileService_Plan_CachesUpstreamIndex(t *testing.T) {
	const pkg = "requests"
	const file = "requests-2.31.0-py3-none-any.whl"

	index := indexWith(pkg, pypi.FileInfo{Name: file, URL: "https://example.org/" + file, Size: 10})
	svc := newTestService(t, newFakeStorage(), index, &fakeDownloader{}, time.Minute)

	for range 3 {
		plan, err := svc.Plan(context.Background(), pkg, file)
		require.NoError(t, err)
		require.Equal(t, ActionStreamAndCache, plan.Action)
		assert.Equal(t, "application/zip", plan.ContentType)
	}

	assert.Equal(t, int64(1), index.calls.Load(), "the index should be fetched once and cached")
}

// --- Fetch deduplication -----------------------------------------------------

func TestPackageFileService_Fetch_SingleCallerStreams(t *testing.T) {
	st := newFakeStorage()
	dl := &fakeDownloader{body: []byte("payload"), storage: st}
	svc := newTestService(t, st, indexWith("p"), dl, time.Minute)

	plan := ServePlan{Action: ActionStreamAndCache, StorageKey: "packages/p/f", URL: "http://x/f", Timeout: time.Minute}

	var buf bytes.Buffer
	result, led, err := svc.Fetch(context.Background(), plan, &buf)

	require.NoError(t, err)
	assert.True(t, led, "the only caller must lead the fetch")
	require.NotNil(t, result)
	assert.Equal(t, int64(len("payload")), result.Size)
	assert.Equal(t, "payload", buf.String())
	assert.Equal(t, int64(1), dl.calls.Load())
}

// TestPackageFileService_Fetch_ConcurrentDedup is the behaviour contract the
// deleted hand-rolled download coordinator used to carry: concurrent requests for the same
// uncached file must not each hit upstream.
func TestPackageFileService_Fetch_ConcurrentDedup(t *testing.T) {
	st := newFakeStorage()
	dl := &fakeDownloader{body: []byte("payload"), storage: st, delay: 50 * time.Millisecond}
	svc := newTestService(t, st, indexWith("p"), dl, time.Minute)

	plan := ServePlan{Action: ActionStreamAndCache, StorageKey: "packages/p/f", URL: "http://x/f", Timeout: time.Minute}

	const callers = 10
	var (
		wg      sync.WaitGroup
		leaders atomic.Int64
		bodies  atomic.Int64
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
			bodies.Add(int64(buf.Len()))
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), dl.calls.Load(), "exactly one upstream download for %d concurrent callers", callers)
	assert.Equal(t, int64(1), leaders.Load(), "exactly one caller leads")

	// The leader populated storage, so followers can now be re-planned onto it.
	exists, err := st.Exists(context.Background(), plan.StorageKey)
	require.NoError(t, err)
	assert.True(t, exists, "the leader must have populated storage for the followers")
}

func TestPackageFileService_Fetch_LeaderFailurePropagates(t *testing.T) {
	st := newFakeStorage()
	dl := &fakeDownloader{err: errors.New("upstream exploded"), delay: 20 * time.Millisecond}
	svc := newTestService(t, st, indexWith("p"), dl, time.Minute)

	plan := ServePlan{Action: ActionStreamAndCache, StorageKey: "packages/p/f", URL: "http://x/f", Timeout: time.Minute}

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			_, _, err := svc.Fetch(context.Background(), plan, &buf)
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		assert.Error(t, err, "caller %d should see the shared failure", i)
	}
	assert.Equal(t, int64(1), dl.calls.Load())
}

// TestPackageFileService_Fetch_SurvivesClientCancellation pins the rule that the
// cache-population write is not cancelled just because the triggering client
// disconnected mid-download.
func TestPackageFileService_Fetch_SurvivesClientCancellation(t *testing.T) {
	st := newFakeStorage()
	dl := &fakeDownloader{body: []byte("payload"), storage: st, delay: 30 * time.Millisecond}
	svc := newTestService(t, st, indexWith("p"), dl, time.Minute)

	plan := ServePlan{Action: ActionStreamAndCache, StorageKey: "packages/p/f", URL: "http://x/f", Timeout: time.Minute}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client already gone

	var buf bytes.Buffer
	result, led, err := svc.Fetch(ctx, plan, &buf)

	require.NoError(t, err)
	assert.True(t, led)
	require.NotNil(t, result)

	exists, err := st.Exists(context.Background(), plan.StorageKey)
	require.NoError(t, err)
	assert.True(t, exists, "cache population must complete despite client cancellation")
}

// TestPackageFileService_Fetch_NoStaleDedupState is the port of the old
// hand-rolled download coordinator cleanup test: dedup bookkeeping must not outlive the
// download it coordinated (the old map needed a 30s cleanup goroutine per
// download; singleflight releases the key when the leader returns).
func TestPackageFileService_Fetch_NoStaleDedupState(t *testing.T) {
	st := newFakeStorage()
	dl := &fakeDownloader{body: []byte("payload"), storage: st}
	svc := newTestService(t, st, indexWith("p"), dl, time.Minute)

	plan := ServePlan{Action: ActionStreamAndCache, StorageKey: "packages/p/f", URL: "http://x/f", Timeout: time.Minute}

	var buf bytes.Buffer
	_, led, err := svc.Fetch(context.Background(), plan, &buf)
	require.NoError(t, err)
	require.True(t, led)

	// A later caller for the same key leads again: no entry was left behind.
	var second bytes.Buffer
	_, led, err = svc.Fetch(context.Background(), plan, &second)
	require.NoError(t, err)
	assert.True(t, led, "dedup state must be released once the download completes")
	assert.Equal(t, int64(2), dl.calls.Load())
}

// TestPackageFileService_Fetch_NilResultIsAnError pins the nil-deref bug: the
// leader's singleflight value used to be type-asserted with the ok discarded, so
// a downloader reporting success without a StreamResult produced a (nil, true,
// nil) return that the caller dereferenced.
func TestPackageFileService_Fetch_NilResultIsAnError(t *testing.T) {
	st := newFakeStorage()
	dl := &fakeDownloader{nilResult: true}
	svc := newTestService(t, st, indexWith("p"), dl, time.Minute)

	plan := ServePlan{Action: ActionStreamAndCache, StorageKey: "packages/p/f", URL: "http://x/f", Timeout: time.Minute}

	result, led, err := svc.Fetch(context.Background(), plan, io.Discard)

	require.Error(t, err, "a leader with no result must report an error, not hand back nil")
	assert.True(t, led, "the caller still led the fetch")
	assert.Nil(t, result)
}

// TestServer_StreamAndCache_NilDownloadResultDoesNotPanic is the transport half
// of the same bug: reading result.Size on a nil result panicked the handler.
func TestServer_StreamAndCache_NilDownloadResultDoesNotPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	svc := newTestService(t, newFakeStorage(), indexWith("p"), &fakeDownloader{nilResult: true}, time.Minute)
	plan := ServePlan{
		Action:      ActionStreamAndCache,
		PackageName: "p",
		FileName:    "f",
		StorageKey:  "packages/p/f",
		URL:         "https://upstream.example.org/f",
		Timeout:     time.Minute,
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/index/p/f", nil)

	srv := &Server{packageFiles: svc}
	srv.streamAndCache(c, plan)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusFound, resp.StatusCode, "the failed fetch must fall back to upstream")
	assert.Equal(t, plan.URL, resp.Header.Get("Location"))
}

// --- ETag quoting ------------------------------------------------------------

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

// TestServer_ServeFromStorage_ETagQuotedOnce pins the double-quoting bug: a local
// backend reports a bare hash while an S3 backend echoes the API's already quoted
// form, and serveFromStorage used to wrap both, emitting `""abc""`.
func TestServer_ServeFromStorage_ETagQuotedOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)

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
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/"+key, nil)

			srv := &Server{storage: st}
			require.NoError(t, srv.serveFromStorage(c, key))

			assert.Equal(t, tt.wantHeader, w.Result().Header.Get("ETag"))
		})
	}
}

// TestPackageFileService_Plan_ETagQuotedOnce is the other half of the same
// contract: the download path must agree with the storage path.
func TestPackageFileService_Plan_ETagQuotedOnce(t *testing.T) {
	const pkg, file = "numpy", "numpy-1.26.0.tar.gz"

	for name, hash := range map[string]string{
		"bare index hash":   "abc123",
		"quoted index hash": `"abc123"`,
	} {
		t.Run(name, func(t *testing.T) {
			index := indexWith(pkg, pypi.FileInfo{
				Name:   file,
				URL:    "https://files.example.org/" + file,
				Size:   4096,
				Hashes: map[string]string{"sha256": hash},
			})
			svc := newTestService(t, newFakeStorage(), index, &fakeDownloader{}, time.Minute)

			plan, err := svc.Plan(context.Background(), pkg, file)
			require.NoError(t, err)
			assert.Equal(t, `"abc123"`, plan.ETag)
		})
	}
}

// --- headers precede the body ------------------------------------------------

// TestServer_DownloadStream_HeadersPrecedeBody covers the bug where
// Content-Type/Content-Length were set after the body had been streamed and were
// therefore silently discarded.
func TestServer_DownloadStream_HeadersPrecedeBody(t *testing.T) {
	const packageName = "headertest"
	const fileName = "headertest-1.0.0.tar.gz"
	fileContent := bytes.Repeat([]byte("x"), 2048)

	var mockPyPI *httptest.Server
	mockPyPI = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+packageName+"/" {
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			response := map[string]any{
				"name": packageName,
				"files": []map[string]any{
					{
						"filename": fileName,
						"url":      fmt.Sprintf("%s/files/%s", mockPyPI.URL, fileName),
						"size":     int64(len(fileContent)),
						"hashes":   map[string]string{"sha256": "cafebabe"},
					},
				},
			}
			jsonData, _ := json.Marshal(response)
			_, _ = w.Write(jsonData)
			return
		}
		if strings.Contains(r.URL.Path, "/files/") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(fileContent)
		}
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 10 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := New(cfg)
	req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, body, len(fileContent))
	assert.Equal(t, "application/gzip", resp.Header.Get("Content-Type"),
		"content type must be derived from the filename and sent before the body")
	assert.Equal(t, strconv.Itoa(len(fileContent)), resp.Header.Get("Content-Length"),
		"content length must come from the index and be sent before the body")
	assert.Equal(t, `"cafebabe"`, resp.Header.Get("ETag"),
		"etag must come from the index hashes and be sent before the body")
}

// TestServer_HandleDownloadFile_ServesFollowersFromStorage asserts the full
// transport-level contract that the download coordinator used to provide.
func TestServer_HandleDownloadFile_ServesFollowersFromStorage(t *testing.T) {
	const packageName = "followers"
	const fileName = "followers-1.0.0.whl"
	fileContent := bytes.Repeat([]byte("y"), 64*1024)

	var downloads atomic.Int64
	var mockPyPI *httptest.Server
	mockPyPI = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+packageName+"/" {
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			response := map[string]any{
				"name": packageName,
				"files": []map[string]any{
					{
						"filename": fileName,
						"url":      fmt.Sprintf("%s/files/%s", mockPyPI.URL, fileName),
						"size":     int64(len(fileContent)),
					},
				},
			}
			jsonData, _ := json.Marshal(response)
			_, _ = w.Write(jsonData)
			return
		}
		if strings.Contains(r.URL.Path, "/files/") {
			downloads.Add(1)
			time.Sleep(80 * time.Millisecond)
			_, _ = w.Write(fileContent)
		}
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 10 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := New(cfg)
	router := srv.Router()

	const callers = 8
	var wg sync.WaitGroup
	sizes := make([]int, callers)
	statuses := make([]int, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
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
		assert.Equal(t, len(fileContent), sizes[i], "caller %d must get the full body", i)
	}
}
