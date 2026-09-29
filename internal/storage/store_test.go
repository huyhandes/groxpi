package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
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

	"github.com/huyhandes/groxpi/internal/download"
)

// fakeFetcher serves one body per call. With a gate, the first half is
// readable at once and the rest only after the gate closes.
type fakeFetcher struct {
	body  []byte
	sha   string // declared digest; defaults to the body's
	size  int64  // declared size; 0 means the body's length, -1 unknown
	gate  chan struct{}
	err   error
	calls atomic.Int64
}

func (f *fakeFetcher) Fetch(context.Context, string, string) (*download.File, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	sha := f.sha
	if sha == "" {
		sum := sha256.Sum256(f.body)
		sha = hex.EncodeToString(sum[:])
	}
	var body io.Reader = bytes.NewReader(f.body)
	if f.gate != nil {
		half := len(f.body) / 2
		body = io.MultiReader(bytes.NewReader(f.body[:half]), gated{f.gate}, bytes.NewReader(f.body[half:]))
	}
	n, length := int64(len(f.body)), int64(len(f.body))
	switch {
	case f.size < 0:
		n, length = -1, -1
	case f.size > 0:
		n, length = f.size, f.size
	}
	if n == 0 {
		n = -1 // like production: the index never declares a zero size
	}
	return &download.File{Target: download.Target{SHA256: sha, Size: n}, Body: io.NopCloser(body), Length: length}, nil
}

// gated blocks until its channel closes, then reads as empty.
type gated struct{ c chan struct{} }

func (g gated) Read([]byte) (int, error) { <-g.c; return 0, io.EOF }

func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return err
	}))
	return out
}

func waitInFlight(t *testing.T, s *Store, requests int) {
	t.Helper()
	require.Eventually(t, func() bool {
		p := s.InFlight()
		return len(p) == 1 && p[0].Requests == requests && p[0].Bytes > 0
	}, 5*time.Second, time.Millisecond)
}

// openAll opens n readers of one file concurrently, while the fetch is gated.
func openAll(t *testing.T, s *Store, n int) []*Object {
	t.Helper()
	objs := make([]*Object, n)
	var wg sync.WaitGroup
	for i := range objs {
		wg.Go(func() {
			obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", true)
			assert.NoError(t, err)
			objs[i] = obj
		})
	}
	wg.Wait()
	waitInFlight(t, s, n)
	return objs
}

func readAll(objs []*Object) ([][]byte, []error) {
	bodies, errs := make([][]byte, len(objs)), make([]error, len(objs))
	var wg sync.WaitGroup
	for i, o := range objs {
		wg.Go(func() {
			bodies[i], errs[i] = io.ReadAll(o)
			_ = o.Close()
		})
	}
	wg.Wait()
	return bodies, errs
}

func TestStore_CoalescedReadersGetIdenticalBytes(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: bytes.Repeat([]byte("0123456789"), 20000), gate: make(chan struct{})}
	s := NewStore(l, nil, f, "")

	objs := openAll(t, s, 5)
	for _, o := range objs {
		assert.True(t, o.InFlight)
		assert.Equal(t, int64(len(f.body)), o.Size)
	}
	close(f.gate)
	bodies, errs := readAll(objs)
	for i := range objs {
		require.NoError(t, errs[i])
		assert.Equal(t, f.body, bodies[i])
	}
	assert.Equal(t, int64(1), f.calls.Load(), "one fetch serves every reader")

	// The next open is a cached hit, and the flight is gone.
	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", true)
	require.NoError(t, err)
	assert.False(t, obj.InFlight)
	_ = obj.Close()
	assert.Empty(t, s.InFlight())
	require.NoError(t, s.Close())
}

func TestStore_ShaMismatchFailsAllReadersLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocalStorage(dir, 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: bytes.Repeat([]byte("x"), 100000), sha: "00", gate: make(chan struct{})}
	s := NewStore(l, nil, f, "")

	objs := openAll(t, s, 3)
	close(f.gate)
	bodies, errs := readAll(objs)
	for i := range objs {
		require.Error(t, errs[i], "every reader fails")
		assert.Less(t, len(bodies[i]), len(f.body), "no reader gets the final byte")
	}
	require.NoError(t, s.Close())
	assert.Empty(t, files(t, dir), "neither the file nor its spool is left behind")
}

func TestStore_DisconnectAllStillCaches(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: bytes.Repeat([]byte("y"), 100000), gate: make(chan struct{})}
	s := NewStore(l, nil, f, "")

	ctx, cancel := context.WithCancel(t.Context())
	obj, err := s.Open(ctx, "pkg", "pkg-1.0.tar.gz", true)
	require.NoError(t, err)
	_, err = obj.Read(make([]byte, 10))
	require.NoError(t, err)
	_ = obj.Close()
	cancel()

	close(f.gate)
	require.NoError(t, s.Close(), "Close waits for the detached download")
	ok, err := exists(l, Key("pkg", "pkg-1.0.tar.gz"))
	require.NoError(t, err)
	assert.True(t, ok, "the download finished and cached with no client left")
}

func TestStore_HybridPromotion(t *testing.T) {
	key := Key("pkg", "pkg-1.0.tar.gz")
	data := []byte("remote bytes")
	l1, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	remote, bucket := newFakeS3(t)
	bucket.seed(key, data)
	f := &fakeFetcher{err: errors.New("upstream must not be called")}
	s := NewStore(l1, remote, f, "")

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", false)
	require.NoError(t, err, "an L2 hit is served even with fetching off")
	assert.True(t, obj.InFlight, "an L2 hit streams through the spool")
	assert.Equal(t, int64(len(data)), obj.Size)
	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	_ = obj.Close()
	assert.Equal(t, data, got)
	assert.Zero(t, f.calls.Load())

	require.NoError(t, s.Close())
	rc, _, err := l1.Get(context.Background(), key)
	require.NoError(t, err, "the L2 hit was promoted into L1")
	promoted, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	assert.Equal(t, data, promoted)
	assert.Zero(t, bucket.puts.Load(), "a promotion is not uploaded back to L2")
}

func TestStore_HybridUploadsEveryDownload(t *testing.T) {
	l1, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	remote, bucket := newFakeS3(t)
	f := &fakeFetcher{body: bytes.Repeat([]byte("h"), 100000), gate: make(chan struct{})}
	s := NewStore(l1, remote, f, "")

	objs := openAll(t, s, 3)
	close(f.gate)
	bodies, errs := readAll(objs)
	for i := range objs {
		require.NoError(t, errs[i])
		assert.Equal(t, f.body, bodies[i])
	}
	require.NoError(t, s.Close(), "Close waits for the upload")
	ok, err := exists(l1, Key("pkg", "pkg-1.0.tar.gz"))
	require.NoError(t, err)
	assert.True(t, ok, "the download is in the cache")
	assert.Equal(t, f.body, bucket.object(Key("pkg", "pkg-1.0.tar.gz")), "and in the durable store")
	assert.Equal(t, int64(1), bucket.puts.Load(), "uploaded once, however many readers")
}

func TestStore_HybridCancelledPromotionStillCommits(t *testing.T) {
	key := Key("pkg", "pkg-1.0.tar.gz")
	data := bytes.Repeat([]byte("p"), 200000)
	l1, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	remote, bucket := newFakeS3(t)
	bucket.seed(key, data)
	bucket.gate = make(chan struct{})
	s := NewStore(l1, remote, &fakeFetcher{err: errors.New("upstream must not be called")}, "")

	ctx, cancel := context.WithCancel(t.Context())
	obj, err := s.Open(ctx, "pkg", "pkg-1.0.tar.gz", false)
	require.NoError(t, err)
	_, err = obj.Read(make([]byte, 10))
	require.NoError(t, err)
	other, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", false)
	require.NoError(t, err)
	_ = obj.Close()
	cancel()

	close(bucket.gate)
	got, err := io.ReadAll(other)
	require.NoError(t, err)
	_ = other.Close()
	assert.Equal(t, data, got, "a remaining reader gets every byte")
	require.NoError(t, s.Close())
	rc, _, err := l1.Get(context.Background(), key)
	require.NoError(t, err, "the promotion finished with its first client gone")
	promoted, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, data, promoted)
}

func TestStore_S3GetFailureIsAMiss(t *testing.T) {
	key := Key("pkg", "pkg-1.0.tar.gz")
	remote, bucket := newFakeS3(t)
	bucket.seed(key, []byte("stored"))
	bucket.failGet = true // HEAD would still say the object is there
	f := &fakeFetcher{body: []byte("upstream")}
	s := NewStore(nil, remote, f, t.TempDir())

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", true)
	require.NoError(t, err, "a failed GET is a miss, not a 200 that dies after its headers")
	assert.True(t, obj.InFlight, "served by a fresh download")
	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	_ = obj.Close()
	assert.Equal(t, f.body, got)
	require.NoError(t, s.Close())
	assert.Equal(t, int64(1), f.calls.Load())
}

func TestStore_HybridDeletePrefixEvictsBothTiers(t *testing.T) {
	l1, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	s := NewStore(l1, nil, &fakeFetcher{body: []byte("cached")}, "")
	obj, err := s.Open(t.Context(), "numpy", "numpy-1.0.tar.gz", true)
	require.NoError(t, err)
	_, _ = io.ReadAll(obj)
	_ = obj.Close()
	require.NoError(t, s.Close())

	bucket := newFakeBucket(t, 10, "packages/numpy/numpy-1.0.tar.gz", "packages/pandas/pandas-1.0.tar.gz")
	remote, err := NewS3Storage(&S3Config{Endpoint: bucket.URL, AccessKeyID: "test", SecretAccessKey: "test",
		Bucket: "b", ForcePathStyle: true})
	require.NoError(t, err)
	s = NewStore(l1, remote, &fakeFetcher{}, "")

	deleted, err := s.DeletePrefix(context.Background(), Key("numpy", ""))
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	ok, err := exists(l1, Key("numpy", "numpy-1.0.tar.gz"))
	require.NoError(t, err)
	assert.False(t, ok, "gone from the cache")
	assert.Equal(t, []string{"packages/pandas/pandas-1.0.tar.gz"}, bucket.remaining(), "and from the durable store")
	require.NoError(t, s.Close())
}

func TestStore_S3UploadsSpoolAndRemovesIt(t *testing.T) {
	spool := t.TempDir()
	remote, bucket := newFakeS3(t)
	f := &fakeFetcher{body: []byte("s3 bytes")}
	s := NewStore(nil, remote, f, spool)

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", true)
	require.NoError(t, err)
	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	_ = obj.Close()
	assert.Equal(t, f.body, got)
	require.NoError(t, s.Close())

	assert.Equal(t, f.body, bucket.object(Key("pkg", "pkg-1.0.tar.gz")))
	assert.Empty(t, files(t, spool), "the spool is removed after upload")
}

func TestStore_FetchErrorPropagates(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	boom := errors.New("boom")
	s := NewStore(l, nil, &fakeFetcher{err: boom}, "")

	_, err = s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", true)
	require.ErrorIs(t, err, boom)
	assert.Empty(t, s.InFlight())
	require.NoError(t, s.Close())
}

func TestStore_NoFetchMissIsNotFound(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: []byte("z")}
	s := NewStore(l, nil, f, "")

	_, err = s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", false)
	require.ErrorIs(t, err, ErrNotFound)
	assert.Zero(t, f.calls.Load())
	require.NoError(t, s.Close())
}

// TestStore_OversizeBodyWithBadShaFailsEveryReader pins H1: upstream sending
// more than the declared size must not hand any reader the declared length
// before verification fails.
func TestStore_OversizeBodyWithBadShaFailsEveryReader(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocalStorage(dir, 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: bytes.Repeat([]byte("x"), 100001), size: 100000, sha: "00", gate: make(chan struct{})}
	s := NewStore(l, nil, f, "")

	objs := openAll(t, s, 3)
	close(f.gate)
	bodies, errs := readAll(objs)
	for i := range objs {
		require.Error(t, errs[i], "every reader fails")
		assert.Less(t, len(bodies[i]), int(f.size), "no reader gets the declared length")
	}
	require.NoError(t, s.Close())
	assert.Empty(t, files(t, dir))
}

// TestStore_OversizeBodyWithGoodShaFails: the client was promised the declared
// Content-Length, so a longer body fails even when its hash matches.
func TestStore_OversizeBodyWithGoodShaFails(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: bytes.Repeat([]byte("x"), 1001), size: 1000}
	s := NewStore(l, nil, f, "")

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", true)
	require.NoError(t, err)
	got, err := io.ReadAll(obj)
	_ = obj.Close()
	require.Error(t, err)
	assert.Less(t, len(got), 1000)
	require.NoError(t, s.Close())
}

func TestStore_UnknownSizeBadShaFailsEveryReader(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocalStorage(dir, 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: bytes.Repeat([]byte("x"), 100000), size: -1, sha: "00", gate: make(chan struct{})}
	s := NewStore(l, nil, f, "")

	objs := openAll(t, s, 3)
	for _, o := range objs {
		assert.Equal(t, int64(-1), o.Size, "no declared size")
	}
	close(f.gate)
	bodies, errs := readAll(objs)
	for i := range objs {
		require.Error(t, errs[i], "every reader fails")
		assert.Less(t, len(bodies[i]), len(f.body))
	}
	require.NoError(t, s.Close())
	assert.Empty(t, files(t, dir))
}

// TestFlight_FailedAfterStartOpensThenFails pins that a verification failure
// never surfaces from open (which the server turns into a redirect to the bad
// bytes), even when the flight has already failed by the time open looks at it.
func TestFlight_FailedAfterStartOpensThenFails(t *testing.T) {
	sp, err := os.CreateTemp(t.TempDir(), "spool")
	require.NoError(t, err)
	_, err = sp.Write([]byte("abcd"))
	require.NoError(t, err)

	f := &flight{file: "pkg-1.0.tar.gz", size: -1, ready: make(chan struct{}), refs: 1}
	f.cond = sync.NewCond(&f.mu)
	f.start(sp, -1, 4, "")
	f.advance(4)
	fail := errors.New("x")
	f.finish(fail)

	f.ref()
	obj, err := f.open(t.Context())
	require.NoError(t, err, "a body started, so open succeeds")
	_, err = obj.Read(make([]byte, 8))
	require.ErrorIs(t, err, fail, "the read reports the failure")

	require.NoError(t, obj.Close())
	assert.Equal(t, 1, f.refs, "Close drops the reader's ref")
	f.release()
	assert.ErrorIs(t, sp.Close(), os.ErrClosed, "the last release closes the spool")
}

// TestStore_UndersizeBodyFails: a body shorter than the declared size fails,
// good hash or bad, and no reader gets the declared length.
func TestStore_UndersizeBodyFails(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 999)
	for name, sha := range map[string]string{"good_sha": "", "bad_sha": "00"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			l, err := NewLocalStorage(dir, 0, 0)
			require.NoError(t, err)
			s := NewStore(l, nil, &fakeFetcher{body: body, size: 1000, sha: sha}, "")

			obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", true)
			require.NoError(t, err)
			got, err := io.ReadAll(obj)
			_ = obj.Close()
			require.Error(t, err)
			assert.Less(t, len(got), 1000)
			require.NoError(t, s.Close())
			assert.Empty(t, files(t, dir))
		})
	}
}

func TestStore_ZeroLengthBody(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	s := NewStore(l, nil, &fakeFetcher{body: []byte{}}, "")

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", true)
	require.NoError(t, err)
	got, err := io.ReadAll(obj)
	_ = obj.Close()
	require.NoError(t, err)
	assert.Empty(t, got)
	require.NoError(t, s.Close())
	ok, err := exists(l, Key("pkg", "pkg-1.0.tar.gz"))
	require.NoError(t, err)
	assert.True(t, ok, "an empty verified file is cached")
}

// TestStore_UnknownSizeHoldsLastByteUntilDone: with no declared length the last
// byte written is held back until the download verifies, then released.
func TestStore_UnknownSizeHoldsLastByteUntilDone(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: bytes.Repeat([]byte("0123456789"), 1000), size: -1, gate: make(chan struct{})}
	s := NewStore(l, nil, f, "")

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz", true)
	require.NoError(t, err)
	waitInFlight(t, s, 1)
	half := len(f.body) / 2
	head := make([]byte, half-1)
	_, err = io.ReadFull(obj, head)
	require.NoError(t, err, "everything but the last written byte is readable")

	rest := make(chan []byte)
	go func() { b, _ := io.ReadAll(obj); rest <- b }()
	select {
	case <-rest:
		t.Fatal("the last written byte was released before the download finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(f.gate)
	assert.Equal(t, f.body, append(head, <-rest...))
	_ = obj.Close()
	require.NoError(t, s.Close())
	ok, err := exists(l, Key("pkg", "pkg-1.0.tar.gz"))
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestStore_S3ShaMismatchFailsAllReadersUploadsNothing(t *testing.T) {
	spool := t.TempDir()
	remote, bucket := newFakeS3(t)
	f := &fakeFetcher{body: bytes.Repeat([]byte("x"), 100000), sha: "00", gate: make(chan struct{})}
	s := NewStore(nil, remote, f, spool)

	objs := openAll(t, s, 3)
	close(f.gate)
	bodies, errs := readAll(objs)
	for i := range objs {
		require.Error(t, errs[i], "every reader fails")
		assert.Less(t, len(bodies[i]), len(f.body))
	}
	require.NoError(t, s.Close())
	assert.Zero(t, bucket.puts.Load(), "nothing is uploaded")
	assert.Empty(t, files(t, spool), "the spool is removed")
}

func TestStore_ReapsStaleSpoolFiles(t *testing.T) {
	spool := t.TempDir()
	stale, fresh := filepath.Join(spool, tmpPrefix+"stale"), filepath.Join(spool, tmpPrefix+"fresh")
	require.NoError(t, os.WriteFile(stale, []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(fresh, []byte("x"), 0o600))
	old := time.Now().Add(-2 * touchInterval)
	require.NoError(t, os.Chtimes(stale, old, old))

	remote, _ := newFakeS3(t)
	s := NewStore(nil, remote, &fakeFetcher{}, spool)
	assert.NoFileExists(t, stale)
	assert.FileExists(t, fresh, "a spool in progress stays")
	require.NoError(t, s.Close())
}

// fakeS3 is an in-process bucket: PUT stores, HEAD and GET of a missing object
// answer 404, and anything else (the SDK's bucket check) answers 200.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte // by URL path
	puts    atomic.Int64
	failGet bool          // object GETs answer 403 (set before use)
	gate    chan struct{} // object GETs send half, then wait for it to close (set before use)
}

// exists reports whether key is a file in the cache.
func exists(l *LocalStorage, key string) (bool, error) {
	_, err := os.Stat(l.buildPath(key))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func newFakeS3(t *testing.T) (*S3Storage, *fakeS3) {
	t.Helper()
	b := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			b.objects[r.URL.Path] = body
			b.puts.Add(1)
		case http.MethodHead, http.MethodGet:
			if strings.Count(r.URL.Path, "/") < 2 {
				return // bucket check
			}
			body, ok := b.objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodGet && b.failGet {
				w.WriteHeader(http.StatusForbidden) // not retried by the SDK, so the test stays fast
				return
			}
			if r.Method == http.MethodGet && b.gate != nil {
				b.mu.Unlock()
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = w.Write(body[:len(body)/2])
				w.(http.Flusher).Flush()
				<-b.gate
				_, _ = w.Write(body[len(body)/2:])
				b.mu.Lock()
				return
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		}
	}))
	t.Cleanup(srv.Close)
	remote, err := NewS3Storage(&S3Config{Endpoint: srv.URL, AccessKeyID: "test", SecretAccessKey: "test",
		Bucket: "b", ForcePathStyle: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	return remote, b
}

// seed stores an object without counting it as an upload.
func (b *fakeS3) seed(key string, body []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects["/b/"+key] = body
}

func (b *fakeS3) object(key string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	for path, body := range b.objects {
		if strings.HasSuffix(path, "/"+key) {
			return body
		}
	}
	return nil
}

// countingWriter counts the body bytes the fake S3 endpoint managed to send.
type countingWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n.Add(int64(n))
	return n, err
}

func TestStore_S3HitTransfersOnlyTheRange(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 3<<20) // 30 MiB
	mod := time.Now().UTC().Truncate(time.Second)
	var (
		mu     sync.Mutex
		gets   []string // Range header of every object GET
		served atomic.Int64
	)
	done := make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".whl") {
			return // HeadBucket
		}
		if r.Method == http.MethodGet {
			mu.Lock()
			gets = append(gets, r.Header.Get("Range"))
			mu.Unlock()
			defer func() { done <- struct{}{} }()
		}
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Content-Type", "application/zip")
		http.ServeContent(countingWriter{w, &served}, r, "", mod, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	remote, err := NewS3Storage(&S3Config{Endpoint: srv.URL, AccessKeyID: "test", SecretAccessKey: "test",
		Bucket: "b", ForcePathStyle: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	s := NewStore(nil, remote, &fakeFetcher{err: errors.New("upstream must not be called")}, t.TempDir())
	t.Cleanup(func() { _ = s.Close() })

	serve := func(rng string) *httptest.ResponseRecorder {
		obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.whl", false)
		require.NoError(t, err)
		assert.False(t, obj.InFlight)
		assert.Equal(t, int64(len(body)), obj.Size)
		assert.Equal(t, mod, obj.ModTime.UTC())
		assert.Empty(t, obj.ETag, "S3 hits send no ETag, like local hits")
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Range", rng)
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", obj.ContentType) // as the handler does; no sniffing read
		http.ServeContent(rec, req, obj.Name, obj.ModTime, obj)
		_ = obj.Close()
		return rec
	}
	wait := func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("fake S3 GET never finished")
		}
	}

	// The lookup's whole-object GET is the existence check; a range elsewhere
	// abandons it for one bounded window, and the size probe fetches nothing.
	rec := serve("bytes=100-109")
	wait()
	wait()
	assert.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, body[100:110], rec.Body.Bytes())
	assert.Equal(t, []string{"", "bytes=100-1048675"}, gets, "one bounded window after the lookup GET")
	t.Logf("served %d bytes for a 10-byte range", served.Load())
	assert.Less(t, served.Load(), int64(3<<20), "a window and socket buffers at most, never the 30 MiB object")

	// Multi-range: the first part reads on from the lookup body at 0; the
	// second opens a window at its own offset.
	rec = serve("bytes=0-4,20-24")
	wait()
	wait()
	assert.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Contains(t, rec.Body.String(), "01234")
	assert.Equal(t, []string{"", "bytes=100-1048675", "", "bytes=20-1048595"}, gets)
	assert.Less(t, served.Load(), int64(len(body)/2))
}

func TestRangedReader_WindowsGrowAndReturnExactBytes(t *testing.T) {
	data := make([]byte, 5<<20+123)
	for i := range data {
		data[i] = byte(i * 7)
	}
	var ranges [][2]int64
	r := &rangedReader{size: int64(len(data)), open: func(off, end int64) (io.ReadCloser, error) {
		ranges = append(ranges, [2]int64{off, end})
		return io.NopCloser(bytes.NewReader(data[off : end+1])), nil
	}}
	_, err := r.Seek(1, io.SeekStart)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, data[1:], got)
	assert.Equal(t, [][2]int64{{1, 1 << 20}, {1<<20 + 1, 3 << 20}, {3<<20 + 1, int64(len(data)) - 1}}, ranges,
		"windows of 1, 2, then up to 4 MiB")
}

func TestRangedReader_EndAndPastEndIssueNoRequest(t *testing.T) {
	r := &rangedReader{size: 10, open: func(int64, int64) (io.ReadCloser, error) {
		t.Fatal("no request expected")
		return nil, nil
	}}
	end, err := r.Seek(0, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(10), end)
	n, err := r.Read(make([]byte, 4))
	assert.Zero(t, n)
	assert.Equal(t, io.EOF, err)
}
