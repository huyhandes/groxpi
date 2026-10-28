package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// fakeFetcher resolves every file to one target and serves one body per Fetch.
// With a gate, the first half is readable at once and the rest only after the
// gate closes.
type fakeFetcher struct {
	body       []byte
	sha        string // declared digest; defaults to the body's
	size       int64  // declared size; 0 means the body's length, -1 unknown
	url        string // the resolved target's URL
	gate       chan struct{}
	fetchGate  chan struct{} // when set, Fetch waits for it to close
	resolveErr error
	err        error // returned by Fetch
	resolves   atomic.Int64
	calls      atomic.Int64 // Fetch calls
}

func (f *fakeFetcher) Resolve(context.Context, string, string) (download.Target, error) {
	f.resolves.Add(1)
	if f.resolveErr != nil {
		return download.Target{}, f.resolveErr
	}
	sha := f.sha
	if sha == "" {
		sum := sha256.Sum256(f.body)
		sha = hex.EncodeToString(sum[:])
	}
	n := int64(len(f.body))
	switch {
	case f.size < 0:
		n = -1
	case f.size > 0:
		n = f.size
	}
	if n == 0 {
		n = -1 // like production: the index never declares a zero size
	}
	return download.Target{URL: f.url, SHA256: sha, Size: n}, nil
}

func (f *fakeFetcher) Fetch(_ context.Context, t download.Target) (*download.File, error) {
	f.calls.Add(1)
	if f.fetchGate != nil {
		<-f.fetchGate
	}
	if f.err != nil {
		return nil, f.err
	}
	var body io.Reader = bytes.NewReader(f.body)
	if f.gate != nil {
		half := len(f.body) / 2
		body = io.MultiReader(bytes.NewReader(f.body[:half]), gated{f.gate}, bytes.NewReader(f.body[half:]))
	}
	length := t.Size
	if f.size == 0 {
		length = int64(len(f.body))
	}
	return &download.File{Target: t, Body: io.NopCloser(body), Length: length}, nil
}

// credentialedURL is a file URL resolved against an index with a password.
const credentialedURL = "https://user:s3cret@files.example/pkg-1.0.tar.gz"

// requireRedirect asserts err is a RedirectError with reason and the
// credential-free target URL.
func requireRedirect(t *testing.T, err error, reason string) {
	t.Helper()
	var re *RedirectError
	require.ErrorAs(t, err, &re)
	assert.Equal(t, reason, re.Reason)
	assert.Equal(t, "https://files.example/pkg-1.0.tar.gz", re.URL)
	assert.True(t, re.Stripped, "the userinfo was stripped")
	assert.NotContains(t, err.Error(), "s3cret")
}

// gated blocks until its channel closes, then reads as empty.
type gated struct{ c chan struct{} }

func (g gated) Read([]byte) (int, error) { <-g.c; return 0, io.EOF }

func mustKey(t *testing.T, pkg, file string) string {
	t.Helper()
	key, err := Key(pkg, file)
	require.NoError(t, err)
	return key
}

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
			obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
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
	s := NewStore(l, nil, f, "", false)

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
	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
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
	s := NewStore(l, nil, f, "", false)

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
	s := NewStore(l, nil, f, "", false)

	ctx, cancel := context.WithCancel(t.Context())
	obj, err := s.Open(ctx, "pkg", "pkg-1.0.tar.gz")
	require.NoError(t, err)
	_, err = obj.Read(make([]byte, 10))
	require.NoError(t, err)
	_ = obj.Close()
	cancel()

	close(f.gate)
	require.NoError(t, s.Close(), "Close waits for the detached download")
	ok, err := exists(l, mustKey(t, "pkg", "pkg-1.0.tar.gz"))
	require.NoError(t, err)
	assert.True(t, ok, "the download finished and cached with no client left")
}

func TestStore_HybridPromotion(t *testing.T) {
	key := mustKey(t, "pkg", "pkg-1.0.tar.gz")
	data := []byte("remote bytes")
	l1, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	remote, bucket := newFakeS3(t)
	bucket.seed(key, data)
	f := &fakeFetcher{err: errors.New("upstream must not be called")}
	s := NewStore(l1, remote, f, "", true)

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
	require.NoError(t, err, "an L2 hit is served even in redirect mode")
	assert.True(t, obj.InFlight, "an L2 hit streams through the spool")
	assert.Equal(t, int64(len(data)), obj.Size)
	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	_ = obj.Close()
	assert.Equal(t, data, got)
	assert.Zero(t, f.calls.Load())
	assert.Zero(t, f.resolves.Load(), "a durable hit resolves nothing")

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
	s := NewStore(l1, remote, f, "", false)

	objs := openAll(t, s, 3)
	close(f.gate)
	bodies, errs := readAll(objs)
	for i := range objs {
		require.NoError(t, errs[i])
		assert.Equal(t, f.body, bodies[i])
	}
	require.NoError(t, s.Close(), "Close waits for the upload")
	ok, err := exists(l1, mustKey(t, "pkg", "pkg-1.0.tar.gz"))
	require.NoError(t, err)
	assert.True(t, ok, "the download is in the cache")
	assert.Equal(t, f.body, bucket.object(mustKey(t, "pkg", "pkg-1.0.tar.gz")), "and in the durable store")
	assert.Equal(t, int64(1), bucket.puts.Load(), "uploaded once, however many readers")
}

func TestStore_HybridCancelledPromotionStillCommits(t *testing.T) {
	key := mustKey(t, "pkg", "pkg-1.0.tar.gz")
	data := bytes.Repeat([]byte("p"), 200000)
	l1, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	remote, bucket := newFakeS3(t)
	bucket.seed(key, data)
	bucket.gate = make(chan struct{})
	s := NewStore(l1, remote, &fakeFetcher{err: errors.New("upstream must not be called")}, "", true)

	ctx, cancel := context.WithCancel(t.Context())
	obj, err := s.Open(ctx, "pkg", "pkg-1.0.tar.gz")
	require.NoError(t, err)
	_, err = obj.Read(make([]byte, 10))
	require.NoError(t, err)
	other, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
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
	key := mustKey(t, "pkg", "pkg-1.0.tar.gz")
	remote, bucket := newFakeS3(t)
	bucket.seed(key, []byte("stored"))
	bucket.failGet = true // HEAD would still say the object is there
	f := &fakeFetcher{body: []byte("upstream")}
	s := NewStore(nil, remote, f, t.TempDir(), false)

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
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
	put(t, l1, mustKey(t, "numpy", "numpy-1.0.tar.gz"), []byte("cached"))

	bucket := newFakeBucket(t, 10, "packages/numpy/numpy-1.0.tar.gz", "packages/pandas/pandas-1.0.tar.gz")
	remote, err := NewS3Storage(&S3Config{Endpoint: bucket.URL, AccessKeyID: "test", SecretAccessKey: "test",
		Bucket: "b", ForcePathStyle: true})
	require.NoError(t, err)
	s := NewStore(l1, remote, &fakeFetcher{}, "", false)

	deleted, err := s.DeletePrefix(context.Background(), mustKey(t, "numpy", ""))
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	ok, err := exists(l1, mustKey(t, "numpy", "numpy-1.0.tar.gz"))
	require.NoError(t, err)
	assert.False(t, ok, "gone from the cache")
	assert.Equal(t, []string{"packages/pandas/pandas-1.0.tar.gz"}, bucket.remaining(), "and from the durable store")
	require.NoError(t, s.Close())
}

func TestStore_S3UploadsSpoolAndRemovesIt(t *testing.T) {
	spool := t.TempDir()
	remote, bucket := newFakeS3(t)
	f := &fakeFetcher{body: []byte("s3 bytes")}
	s := NewStore(nil, remote, f, spool, false)

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
	require.NoError(t, err)
	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	_ = obj.Close()
	assert.Equal(t, f.body, got)
	require.NoError(t, s.Close())

	assert.Equal(t, f.body, bucket.object(mustKey(t, "pkg", "pkg-1.0.tar.gz")))
	assert.Empty(t, files(t, spool), "the spool is removed after upload")
}

// TestStore_CommitBetweenLookupAndLeadServesTheCommit: a request whose lookup
// missed just before another flight for the key committed serves the committed
// file rather than fetching and uploading it again.
func TestStore_CommitBetweenLookupAndLeadServesTheCommit(t *testing.T) {
	var (
		mu      sync.Mutex
		objects = map[string][]byte{}
		puts    atomic.Int64
		misses  atomic.Int64
	)
	held, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Count(r.URL.Path, "/") < 2 {
			return // bucket check
		}
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			objects[r.URL.Path] = body
			mu.Unlock()
			puts.Add(1)
			return
		}
		mu.Lock()
		body, ok := objects[r.URL.Path]
		mu.Unlock()
		if !ok {
			if misses.Add(1) == 1 { // the first miss is held until the other flight commits
				close(held)
				<-release
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	remote, err := NewS3Storage(&S3Config{Endpoint: srv.URL, AccessKeyID: "test", SecretAccessKey: "test",
		Bucket: "b", ForcePathStyle: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	f := &fakeFetcher{body: []byte("committed bytes")}
	s := NewStore(nil, remote, f, t.TempDir(), false)

	late := make(chan []byte)
	go func() {
		obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
		if !assert.NoError(t, err) {
			late <- nil
			return
		}
		got, err := io.ReadAll(obj)
		assert.NoError(t, err)
		_ = obj.Close()
		late <- got
	}()
	<-held // the late request's lookup has missed but not yet led

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
	require.NoError(t, err)
	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	_ = obj.Close()
	assert.Equal(t, f.body, got)
	require.Eventually(t, func() bool { return len(s.InFlight()) == 0 }, 5*time.Second, time.Millisecond,
		"the first flight commits and unregisters")

	close(release)
	assert.Equal(t, f.body, <-late)
	require.NoError(t, s.Close())
	assert.Equal(t, int64(1), f.calls.Load(), "fetched once")
	assert.Equal(t, int64(1), puts.Load(), "uploaded once")
}

// pkgBySlot finds a package named base plus a number whose sdist key shares
// the commit slot of (pkgA, fileA) when same is set, or does not otherwise.
// The slot hash is seeded per Store, so the pair is chosen at test time.
func pkgBySlot(t *testing.T, s *Store, base, pkgA, fileA string, same bool) (pkg, file string) {
	t.Helper()
	keyA, err := Key(pkgA, fileA)
	require.NoError(t, err)
	for i := range 100 * commitSlots {
		pkg = fmt.Sprintf("%s%d", base, i)
		file = pkg + "-1.0.tar.gz"
		key, err := Key(pkg, file)
		require.NoError(t, err)
		if (s.commitSlot(key) == s.commitSlot(keyA)) == same {
			return pkg, file
		}
	}
	t.Fatalf("no %s package with same=%v slot as %s", base, same, keyA)
	return "", ""
}

// TestStore_CommitOfAnotherKeyDoesNotRepeatTheLookup: a commit of an unrelated
// key between a miss and its lead does not send the miss back to the store.
func TestStore_CommitOfAnotherKeyDoesNotRepeatTheLookup(t *testing.T) {
	var lookupsA atomic.Int64
	held, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Count(r.URL.Path, "/") < 2 || r.Method == http.MethodPut {
			return // bucket check or upload
		}
		if strings.Contains(r.URL.Path, "/pkg-a/") && lookupsA.Add(1) == 1 {
			close(held) // the first miss of A is held until B commits
			<-release
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	remote, err := NewS3Storage(&S3Config{Endpoint: srv.URL, AccessKeyID: "test", SecretAccessKey: "test",
		Bucket: "b", ForcePathStyle: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	s := NewStore(nil, remote, &fakeFetcher{body: []byte("bytes")}, t.TempDir(), false)
	pkgB, fileB := pkgBySlot(t, s, "pkg-b", "pkg-a", "a-1.0.tar.gz", false)

	done := make(chan struct{})
	go func() {
		defer close(done)
		obj, err := s.Open(t.Context(), "pkg-a", "a-1.0.tar.gz")
		if assert.NoError(t, err) {
			_, _ = io.Copy(io.Discard, obj)
			_ = obj.Close()
		}
	}()
	<-held

	obj, err := s.Open(t.Context(), pkgB, fileB)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, obj)
	_ = obj.Close()
	require.Eventually(t, func() bool { return len(s.InFlight()) == 0 }, 5*time.Second, time.Millisecond,
		"B commits and unregisters")

	close(release)
	<-done
	require.NoError(t, s.Close())
	assert.Equal(t, int64(1), lookupsA.Load(), "A looked up once")
}

// TestStore_FetchErrorRedirects: a fetch that fails before its body starts is a
// fetch_failed redirect to the target the leader resolved.
func TestStore_FetchErrorRedirects(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	boom := errors.New("boom")
	f := &fakeFetcher{err: boom, url: credentialedURL}
	s := NewStore(l, nil, f, "", false)

	_, err = s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
	requireRedirect(t, err, telemetry.RedirectFetchFailed)
	require.ErrorIs(t, err, boom)
	assert.Empty(t, s.InFlight())
	assert.Equal(t, int64(1), f.resolves.Load())
	require.NoError(t, s.Close())
}

// TestStore_JoinersShareTheLeadersRedirect: clients coalesced onto a fetch that
// fails all get the leader's redirect, and the index is resolved once.
func TestStore_JoinersShareTheLeadersRedirect(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{err: errors.New("boom"), url: credentialedURL, fetchGate: make(chan struct{})}
	s := NewStore(l, nil, f, "", false)

	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
			if obj != nil {
				_ = obj.Close()
			}
			errs[i] = err
		})
	}
	require.Eventually(t, func() bool {
		p := s.InFlight()
		return len(p) == 1 && p[0].Requests == n
	}, 5*time.Second, time.Millisecond)
	close(f.fetchGate)
	wg.Wait()
	for _, err := range errs {
		requireRedirect(t, err, telemetry.RedirectFetchFailed)
	}
	assert.Equal(t, int64(1), f.resolves.Load(), "joiners do not resolve again")
	assert.Equal(t, int64(1), f.calls.Load())
	require.NoError(t, s.Close())
}

// TestStore_SpoolCreateFailureRedirects: with nowhere to spool, the miss is a
// fetch_failed redirect rather than an error the client cannot act on.
func TestStore_SpoolCreateFailureRedirects(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	remote, _ := newFakeS3(t)
	f := &fakeFetcher{body: []byte("z"), url: credentialedURL}
	s := NewStore(nil, remote, f, filepath.Join(blocker, "spool"), false)

	_, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
	requireRedirect(t, err, telemetry.RedirectFetchFailed)
	assert.Empty(t, s.InFlight())
	assert.Equal(t, int64(1), f.resolves.Load())
	require.NoError(t, s.Close())
}

// TestStore_HybridPromotionSpoolFailureIsAStorageError: a durable hit that
// cannot be spooled into the cache is a storage failure, not a miss: the file
// exists, so a client must not be told it does not.
func TestStore_HybridPromotionSpoolFailureIsAStorageError(t *testing.T) {
	dir := t.TempDir()
	l1, err := NewLocalStorage(dir, 0, 0)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "packages"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "packages", "pkg"), nil, 0o600)) // the spool dir cannot be made
	remote, bucket := newFakeS3(t)
	bucket.seed(mustKey(t, "pkg", "pkg-1.0.tar.gz"), []byte("remote bytes"))
	f := &fakeFetcher{err: errors.New("upstream must not be called")}
	s := NewStore(l1, remote, f, "", false)

	_, err = s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
	require.ErrorIs(t, err, ErrStorage)
	var re *RedirectError
	assert.NotErrorAs(t, err, &re, "the durable store has it: no upstream redirect")
	assert.Zero(t, f.resolves.Load())
	assert.Empty(t, s.InFlight())
	require.NoError(t, s.Close())
}

// TestStore_NotListedIsNotARedirect: a file the index does not list is
// download.ErrNotListed in either mode, and nothing is fetched.
func TestStore_NotListedIsNotARedirect(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{resolveErr: fmt.Errorf("%w: pkg/pkg-1.0.tar.gz", download.ErrNotListed)}
	for _, redirect := range []bool{false, true} {
		s := NewStore(l, nil, f, "", redirect)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		_, err = s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
		require.ErrorIs(t, err, download.ErrNotListed, "redirect=%v", redirect)
		var re *RedirectError
		assert.NotErrorAs(t, err, &re, "redirect=%v", redirect)
		assert.Empty(t, s.InFlight())
	}
	assert.Equal(t, int64(2), f.resolves.Load())
	assert.Zero(t, f.calls.Load())
}

// TestStore_RedirectMissStartsNoFlight: a store built in redirect mode resolves
// a miss once and answers caching_disabled without fetching.
func TestStore_RedirectMissStartsNoFlight(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: []byte("z"), url: credentialedURL}
	s := NewStore(l, nil, f, "", true)
	assert.True(t, s.Redirects())
	assert.False(t, NewStore(l, nil, f, "", false).Redirects())

	_, err = s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
	requireRedirect(t, err, telemetry.RedirectCachingDisabled)
	assert.Empty(t, s.InFlight())
	assert.Equal(t, int64(1), f.resolves.Load())
	assert.Zero(t, f.calls.Load())
	require.NoError(t, s.Close())
}

// TestStore_OpenRejectsInvalidKey pins A1: a name that could leave the package
// directory is refused before the cache, the durable store or the fetcher is
// touched, so a file planted beside packages/ is never served.
func TestStore_OpenRejectsInvalidKey(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocalStorage(dir, 0, 0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret"), []byte("planted"), 0o600))
	f := &fakeFetcher{body: []byte("z")}
	s := NewStore(l, nil, f, "", false)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	redirect := NewStore(l, nil, f, "", true)

	for _, tc := range []struct{ pkg, file string }{
		{"pkg", "../../secret"},
		{"pkg", "../secret"},
		{"pkg", "a/b"},
		{"pkg", `a\b`},
		{"pkg", "a\x00b"},
		{"pkg", ".hidden"},
		{"pkg", ".."},
		{"pkg", ""},
		{"", "pkg-1.0.tar.gz"},
		{"../..", "secret"}, // a bare ".." normalises to the inert "-"
		{"a/b", "pkg-1.0.tar.gz"},
	} {
		for _, st := range []*Store{s, redirect} {
			obj, err := st.Open(t.Context(), tc.pkg, tc.file)
			if obj != nil {
				_ = obj.Close()
			}
			require.ErrorIs(t, err, ErrInvalidKey, "pkg=%q file=%q", tc.pkg, tc.file)
			assert.NotErrorIs(t, err, ErrNotFound, "pkg=%q file=%q", tc.pkg, tc.file)
		}
	}
	assert.Zero(t, f.calls.Load(), "an invalid key never reaches upstream")

	for _, file := range []string{
		"pkg-1.0-py3-none-any.whl",
		"pkg-1.0.tar.gz",
		"pkg-1.0-py3-none-any.whl.metadata",
		"pkg-1.0+local.1-cp312-cp312-manylinux_2_17_x86_64.whl",
	} {
		obj, err := s.Open(t.Context(), "pkg", file)
		require.NoError(t, err, file)
		_, err = io.ReadAll(obj)
		require.NoError(t, err, file)
		require.NoError(t, obj.Close())
	}
	assert.Equal(t, int64(4), f.calls.Load(), "real names reach Fetch")
}

// TestStore_OversizeBodyWithBadShaFailsEveryReader pins H1: upstream sending
// more than the declared size must not hand any reader the declared length
// before verification fails.
func TestStore_OversizeBodyWithBadShaFailsEveryReader(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocalStorage(dir, 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: bytes.Repeat([]byte("x"), 100001), size: 100000, sha: "00", gate: make(chan struct{})}
	s := NewStore(l, nil, f, "", false)

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
	s := NewStore(l, nil, f, "", false)

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
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
	s := NewStore(l, nil, f, "", false)

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
	f.start(sp, -1, 4)
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
			s := NewStore(l, nil, &fakeFetcher{body: body, size: 1000, sha: sha}, "", false)

			obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
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
	s := NewStore(l, nil, &fakeFetcher{body: []byte{}}, "", false)

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
	require.NoError(t, err)
	got, err := io.ReadAll(obj)
	_ = obj.Close()
	require.NoError(t, err)
	assert.Empty(t, got)
	require.NoError(t, s.Close())
	ok, err := exists(l, mustKey(t, "pkg", "pkg-1.0.tar.gz"))
	require.NoError(t, err)
	assert.True(t, ok, "an empty verified file is cached")
}

// TestStore_UnknownSizeHoldsLastByteUntilDone: with no declared length the last
// byte written is held back until the download verifies, then released.
func TestStore_UnknownSizeHoldsLastByteUntilDone(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	f := &fakeFetcher{body: bytes.Repeat([]byte("0123456789"), 1000), size: -1, gate: make(chan struct{})}
	s := NewStore(l, nil, f, "", false)

	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
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
	ok, err := exists(l, mustKey(t, "pkg", "pkg-1.0.tar.gz"))
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestStore_S3ShaMismatchFailsAllReadersUploadsNothing(t *testing.T) {
	spool := t.TempDir()
	remote, bucket := newFakeS3(t)
	f := &fakeFetcher{body: bytes.Repeat([]byte("x"), 100000), sha: "00", gate: make(chan struct{})}
	s := NewStore(nil, remote, f, spool, false)

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
	s := NewStore(nil, remote, &fakeFetcher{}, spool, false)
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
	putOnce sync.Once
	failGet bool          // object GETs answer 403 (set before use)
	gate    chan struct{} // object GETs send half, then wait for it to close (set before use)
	putGate chan struct{} // PUTs signal putSeen, then wait for it to close (set before use)
	putSeen chan struct{} // closed by the first gated PUT
	getHold chan struct{} // the first object GET reads the bucket, signals getHeld, then waits for it to close (set before use)
	getHeld chan struct{}
	getOnce sync.Once
	delHold chan struct{} // DeleteObjects signal delHeld, then wait for it to close (set before use)
	delHeld chan struct{}
	delOnce sync.Once
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
		switch q := r.URL.Query(); {
		case q.Get("list-type") == "2":
			b.list(w, q.Get("prefix"))
			return
		case r.Method == http.MethodPost && q.Has("delete"):
			b.delete(w, r)
			return
		}
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			if b.putGate != nil {
				b.mu.Unlock()
				b.putOnce.Do(func() { close(b.putSeen) })
				<-b.putGate
				b.mu.Lock()
			}
			b.objects[r.URL.Path] = body
			b.puts.Add(1)
		case http.MethodHead, http.MethodGet:
			if strings.Count(r.URL.Path, "/") < 2 {
				return // bucket check
			}
			body, ok := b.objects[r.URL.Path]
			if r.Method == http.MethodGet && b.getHold != nil {
				first := false
				b.getOnce.Do(func() { first = true })
				if first {
					b.mu.Unlock()
					close(b.getHeld)
					<-b.getHold
					b.mu.Lock()
				}
			}
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
	s := NewStore(nil, remote, &fakeFetcher{err: errors.New("upstream must not be called")}, t.TempDir(), true)
	t.Cleanup(func() { _ = s.Close() })

	serve := func(rng string) *httptest.ResponseRecorder {
		obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.whl")
		require.NoError(t, err)
		assert.False(t, obj.InFlight)
		assert.Equal(t, int64(len(body)), obj.Size)
		assert.Equal(t, mod, obj.ModTime.UTC())
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

// TestKey_NormalisesThePackage pins c8: the key is built from the normalised
// package name, which is what the denylist then checks; the file stays raw.
func TestKey_NormalisesThePackage(t *testing.T) {
	assert.Equal(t, mustKey(t, "foo-bar", "Foo_Bar-1.0.tar.gz"), mustKey(t, "Foo_Bar", "Foo_Bar-1.0.tar.gz"))
	assert.Equal(t, "packages/foo-bar/Foo_Bar-1.0.tar.gz", mustKey(t, "Foo.BAR", "Foo_Bar-1.0.tar.gz"))
	assert.Equal(t, "packages/foo-bar/", mustKey(t, "Foo__Bar", ""))

	// ".." and ".x" normalise to "-" and "-x", which are inert names; what
	// would still escape the directory is refused.
	for _, pkg := range []string{"", "a/b", `a\b`, "a\x00b", "Foo/Bar", "A/../B"} {
		_, err := Key(pkg, "f.tar.gz")
		require.ErrorIs(t, err, ErrInvalidKey, "pkg=%q", pkg)
	}
	for _, file := range []string{"../secret", ".hidden", "a/b"} {
		_, err := Key("Foo_Bar", file)
		require.ErrorIs(t, err, ErrInvalidKey, "file=%q", file)
	}
}

// TestStore_OpenNormalisesThePackage pins c8: a mixed-case open joins the
// normalised key and resolves upstream under the normalised name.
func TestStore_OpenNormalisesThePackage(t *testing.T) {
	l, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	f := &namedFetcher{fakeFetcher: fakeFetcher{body: []byte("z")}}
	s := NewStore(l, nil, f, "", false)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	for _, pkg := range []string{"Foo_Bar", "foo-bar"} {
		obj, err := s.Open(t.Context(), pkg, "Foo_Bar-1.0.tar.gz")
		require.NoError(t, err, pkg)
		_, err = io.ReadAll(obj)
		require.NoError(t, err)
		require.NoError(t, obj.Close())
	}
	assert.Equal(t, int64(1), f.calls.Load(), "both spellings are one key")
	assert.Equal(t, []string{"foo-bar"}, f.names(), "upstream is resolved under the normalised name")
}

// namedFetcher records the package names it is asked to resolve.
type namedFetcher struct {
	fakeFetcher
	mu   sync.Mutex
	pkgs []string
}

func (f *namedFetcher) Resolve(ctx context.Context, pkg, file string) (download.Target, error) {
	f.mu.Lock()
	f.pkgs = append(f.pkgs, pkg)
	f.mu.Unlock()
	return f.fakeFetcher.Resolve(ctx, pkg, file)
}

func (f *namedFetcher) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pkgs...)
}

// TestStore_EvictDuringPromotionLookupIsNotUndone: a DeletePrefix between a
// hybrid Open's durable GET and its flight registration marks nothing, so the
// Open must notice the eviction and look again instead of promoting the
// deleted bytes back into the cache.
func TestStore_EvictDuringPromotionLookupIsNotUndone(t *testing.T) {
	l1, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	remote, bucket := newFakeS3(t)
	key := mustKey(t, "pkg", "pkg-1.0.tar.gz")
	bucket.seed(key, []byte("evicted bytes"))
	bucket.getHold, bucket.getHeld = make(chan struct{}), make(chan struct{})
	f := &fakeFetcher{body: []byte("upstream bytes")}
	s := NewStore(l1, remote, f, "", false)

	got := make(chan []byte, 1)
	go func() {
		obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
		if err != nil {
			got <- []byte(err.Error())
			return
		}
		body, _ := io.ReadAll(obj)
		_ = obj.Close()
		got <- body
	}()
	<-bucket.getHeld // the GET is held before the fake writes anything; Open has not locked yet
	_, err = s.DeletePrefix(t.Context(), mustKey(t, "pkg", ""))
	require.NoError(t, err)
	close(bucket.getHold)

	assert.Equal(t, "upstream bytes", string(<-got), "the deleted bytes are not served")
	require.NoError(t, s.Close())
	assert.Equal(t, int64(1), f.resolves.Load(), "the Open fetched upstream")
	cached, err := os.ReadFile(l1.buildPath(key))
	require.NoError(t, err)
	assert.Equal(t, "upstream bytes", string(cached), "the evicted bytes were not promoted back")
}

// TestStore_StalledS3LookupFallsBack pins that a lookup GET whose headers do not
// arrive within the S3 header deadline is a miss: the file is fetched upstream
// instead of the request waiting on S3.
func TestStore_StalledS3LookupFallsBack(t *testing.T) {
	for _, mode := range []string{"s3", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			remote, bucket := newFakeS3(t)
			remote.getHeaderTimeout = 200 * time.Millisecond
			bucket.seed(mustKey(t, "pkg", "pkg-1.0.tar.gz"), []byte("stale bytes"))
			bucket.getHold, bucket.getHeld = make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { close(bucket.getHold) }) // before the server closes
			var cache *LocalStorage
			if mode == "hybrid" {
				var err error
				cache, err = NewLocalStorage(t.TempDir(), 0, 0)
				require.NoError(t, err)
			}
			f := &fakeFetcher{body: []byte("upstream bytes")}
			s := NewStore(cache, remote, f, t.TempDir(), false)

			start := time.Now()
			obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
			require.NoError(t, err)
			got, err := io.ReadAll(obj)
			require.NoError(t, err)
			_ = obj.Close()
			assert.Less(t, time.Since(start), 3*time.Second, "the lookup gave up at its deadline")
			assert.Equal(t, "upstream bytes", string(got))
			assert.Equal(t, int64(1), f.calls.Load(), "the stalled lookup fell back to upstream")
			require.NoError(t, s.Close())
		})
	}
}

// TestStore_ClientLeavingDuringS3LookupStopsTheWait pins that a lookup GET held
// by S3 ends when its client leaves, and that nothing is fetched for it.
func TestStore_ClientLeavingDuringS3LookupStopsTheWait(t *testing.T) {
	remote, bucket := newFakeS3(t)
	bucket.getHold, bucket.getHeld = make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(bucket.getHold) })
	f := &fakeFetcher{body: []byte("upstream bytes")}
	s := NewStore(nil, remote, f, t.TempDir(), false)

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := s.Open(ctx, "pkg", "pkg-1.0.tar.gz")
		errc <- err
	}()
	<-bucket.getHeld
	cancel()
	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("Open still waiting on S3 after its client left")
	}
	require.NoError(t, s.Close())
	assert.Zero(t, f.resolves.Load(), "nothing is fetched for a client that left")
}

// dropOnceS3 serves one object whose first GET sends Content-Length and half the
// body, then drops the connection; every later GET is served whole (ranges
// honoured).
func dropOnceS3(t *testing.T, body []byte) *S3Storage {
	t.Helper()
	var dropped atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Count(r.URL.Path, "/") < 2 || r.Method == http.MethodPut {
			return // bucket check or upload
		}
		if r.Method == http.MethodGet && dropped.CompareAndSwap(false, true) {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body[:len(body)/2])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	remote, err := NewS3Storage(&S3Config{Endpoint: srv.URL, AccessKeyID: "test", SecretAccessKey: "test",
		Bucket: "b", ForcePathStyle: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	return remote
}

// TestStore_S3BodyDropIsResumed pins that one connection drop mid-body does not
// fail an S3 hit or a promotion: the read resumes at its offset.
func TestStore_S3BodyDropIsResumed(t *testing.T) {
	body := make([]byte, 3<<20+17)
	for i := range body {
		body[i] = byte(i * 13)
	}
	sum := sha256.Sum256(body)
	for _, mode := range []string{"s3", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			remote := dropOnceS3(t, body)
			var cache *LocalStorage
			if mode == "hybrid" {
				var err error
				cache, err = NewLocalStorage(t.TempDir(), 0, 0)
				require.NoError(t, err)
			}
			f := &fakeFetcher{err: errors.New("upstream must not be called")}
			s := NewStore(cache, remote, f, t.TempDir(), false)

			obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.whl")
			require.NoError(t, err)
			got, err := io.ReadAll(obj)
			require.NoError(t, err)
			_ = obj.Close()
			require.NoError(t, s.Close())
			assert.Equal(t, sum, sha256.Sum256(got), "the full, correct bytes")
			if cache != nil {
				cached, err := os.ReadFile(cache.buildPath(mustKey(t, "pkg", "pkg-1.0.whl")))
				require.NoError(t, err)
				assert.Equal(t, sum, sha256.Sum256(cached), "the promotion cached the full bytes")
			}
			assert.Zero(t, f.calls.Load())
		})
	}
}

// failingReader returns its bytes, then err.
type failingReader struct {
	r   io.Reader
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, f.err
	}
	return n, err
}

func TestRangedReader_SecondFailureInARowIsReturned(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	boom := errors.New("connection reset")
	opens := 0
	r := &rangedReader{size: int64(len(data)),
		body: io.NopCloser(&failingReader{r: bytes.NewReader(data[:10]), err: boom}),
		open: func(int64, int64) (io.ReadCloser, error) {
			opens++
			return io.NopCloser(&failingReader{r: strings.NewReader(""), err: boom}), nil
		}}
	got, err := io.ReadAll(r)
	require.ErrorIs(t, err, boom)
	assert.Len(t, got, 10)
	assert.Equal(t, 1, opens, "one reopen at the failed offset, then the error")
}
