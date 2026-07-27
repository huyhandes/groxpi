package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTier is an in-memory Storage double usable as either tier. It satisfies
// both capability interfaces so it can be dropped into the L1 or the L2 slot,
// and it can be told to fail so tests can tell a real backend error apart from
// a miss.
type fakeTier struct {
	mu       sync.Mutex
	objects  map[string][]byte
	failWith error // returned by every read path instead of consulting objects
	putErr   error // returned by Put instead of storing
	putGate  chan struct{}

	gets atomic.Int64
	puts atomic.Int64
}

func newFakeTier(objects map[string][]byte) *fakeTier {
	if objects == nil {
		objects = map[string][]byte{}
	}
	return &fakeTier{objects: objects}
}

func (f *fakeTier) load(key string) ([]byte, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	data, ok := f.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return data, nil
}

func (f *fakeTier) Get(_ context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	f.gets.Add(1)

	data, err := f.load(key)
	if err != nil {
		return nil, nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), &ObjectInfo{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeTier) Put(_ context.Context, key string, reader io.Reader, _ int64, _ string) (*ObjectInfo, error) {
	f.puts.Add(1)

	if f.putGate != nil {
		<-f.putGate
	}
	if f.putErr != nil {
		return nil, f.putErr
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data

	return &ObjectInfo{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeTier) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakeTier) Exists(_ context.Context, key string) (bool, error) {
	if f.failWith != nil {
		return false, f.failWith
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok, nil
}

func (f *fakeTier) Close() error { return nil }

func (f *fakeTier) GetFilePath(_ context.Context, key string) (string, error) {
	if _, err := f.load(key); err != nil {
		return "", err
	}
	return "/fake/" + key, nil
}

func (f *fakeTier) DeletePrefix(_ context.Context, prefix string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	deleted := 0
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			delete(f.objects, key)
			deleted++
		}
	}
	return deleted, nil
}

func (f *fakeTier) Snapshot() []LRUEntry {
	f.mu.Lock()
	defer f.mu.Unlock()

	rows := make([]LRUEntry, 0, len(f.objects))
	for key, data := range f.objects {
		rows = append(rows, LRUEntry{Key: key, Size: int64(len(data))})
	}
	return rows
}

var _ l1Storage = (*fakeTier)(nil)

// TestTieredStorage_DeletePrefixForwardsToL1 pins the hybrid-mode gap where
// evicting a package cleared the index entry and left every file on disk. L2 is
// deliberately untouched: discovering what matches there would need the storage
// listing operation this design does without.
func TestTieredStorage_DeletePrefixForwardsToL1(t *testing.T) {
	l1 := newFakeTier(map[string][]byte{
		"packages/evictme/evictme-1.0.0.tar.gz": []byte("a"),
		"packages/evictme/evictme-1.0.0.whl":    []byte("b"),
		"packages/keepme/keepme-1.0.0.tar.gz":   []byte("c"),
	})
	l2 := newFakeTier(map[string][]byte{"packages/evictme/evictme-1.0.0.tar.gz": []byte("a")})

	ts := newTieredStorage(l1, l2, 1, 1)
	t.Cleanup(func() { _ = ts.Close() })

	deleted, err := ts.DeletePrefix(context.Background(), "packages/evictme/")
	require.NoError(t, err)
	assert.Equal(t, 2, deleted)

	exists, err := l1.Exists(context.Background(), "packages/evictme/evictme-1.0.0.tar.gz")
	require.NoError(t, err)
	assert.False(t, exists, "L1 must no longer hold the evicted package")

	exists, err = l1.Exists(context.Background(), "packages/keepme/keepme-1.0.0.tar.gz")
	require.NoError(t, err)
	assert.True(t, exists, "an unrelated package must survive")

	exists, err = l2.Exists(context.Background(), "packages/evictme/evictme-1.0.0.tar.gz")
	require.NoError(t, err)
	assert.True(t, exists, "L2 is best-effort and deliberately left alone")
}

// TestTieredStorage_SnapshotForwardsToL1 pins the hybrid-mode gap where the
// cached-package listing rendered zero rows: the admin page reads its rows from
// a Snapshot the top-level backend has to provide, and in hybrid mode that
// backend is the tiered one, not the local cache underneath it.
func TestTieredStorage_SnapshotForwardsToL1(t *testing.T) {
	const key = "packages/requests/requests-2.31.0.tar.gz"
	payload := []byte("wheel bytes")

	l1, err := NewLRULocalStorage(t.TempDir(), 10*1024*1024, 0)
	require.NoError(t, err)

	ts := newTieredStorage(l1, newFakeTier(nil), 4, 1)
	defer func() { _ = ts.Close() }()

	_, err = ts.Put(context.Background(), key, bytes.NewReader(payload), int64(len(payload)), "application/octet-stream")
	require.NoError(t, err)

	rows := ts.Snapshot()
	require.Len(t, rows, 1, "a file written through the tiered backend must show up in its snapshot")
	assert.Equal(t, key, rows[0].Key)
	assert.Equal(t, int64(len(payload)), rows[0].Size)
}

// TestTieredStorage_L1BackfillLands covers the bug where the back-fill job was
// submitted with a context the submitting goroutine cancelled on its way out:
// the worker then bailed and L1 was never populated from L2.
func TestTieredStorage_L1BackfillLands(t *testing.T) {
	const key = "packages/numpy/numpy-1.26.0.tar.gz"
	payload := []byte("wheel bytes")

	l1, err := NewLRULocalStorage(t.TempDir(), 10*1024*1024, 0)
	require.NoError(t, err)

	l2 := newFakeTier(map[string][]byte{key: payload})

	ts := newTieredStorage(l1, l2, 8, 2)
	defer func() { _ = ts.Close() }()

	ctx := context.Background()

	reader, info, err := ts.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, int64(len(payload)), info.Size)

	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, payload, body)

	// The back-fill runs asynchronously, but it must actually run.
	require.Eventually(t, func() bool {
		exists, err := l1.Exists(ctx, key)
		return err == nil && exists
	}, 5*time.Second, 10*time.Millisecond, "L1 was never populated from L2")

	// And the copy that landed must be the real payload.
	cached, _, err := l1.Get(ctx, key)
	require.NoError(t, err)
	defer func() { _ = cached.Close() }()

	cachedBody, err := io.ReadAll(cached)
	require.NoError(t, err)
	assert.Equal(t, payload, cachedBody)
}

// TestTieredStorage_L1BackfillSurvivesRequestCancellation pins that the job
// outlives the request that triggered it: cancelling the caller's context must
// not abort a population already queued.
func TestTieredStorage_L1BackfillSurvivesRequestCancellation(t *testing.T) {
	const key = "packages/flask/flask-3.0.0.tar.gz"
	payload := []byte("more wheel bytes")

	l1, err := NewLRULocalStorage(t.TempDir(), 10*1024*1024, 0)
	require.NoError(t, err)

	l2 := newFakeTier(map[string][]byte{key: payload})

	ts := newTieredStorage(l1, l2, 8, 2)
	defer func() { _ = ts.Close() }()

	reqCtx, cancel := context.WithCancel(context.Background())

	reader, _, err := ts.Get(reqCtx, key)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	// Client goes away immediately after the body is handed over.
	cancel()

	require.Eventually(t, func() bool {
		exists, err := l1.Exists(context.Background(), key)
		return err == nil && exists
	}, 5*time.Second, 10*time.Millisecond, "back-fill died with the request context")
}

// TestTieredStorage_PropagatesRealL1Error covers the bug where any L1 error was
// treated as a cache miss, so a broken local disk silently became an L2 read
// and the real failure was never surfaced.
func TestTieredStorage_PropagatesRealL1Error(t *testing.T) {
	const key = "packages/numpy/numpy-1.26.0.tar.gz"

	diskFailure := errors.New("input/output error")

	l1 := newFakeTier(nil)
	l1.failWith = diskFailure

	l2 := newFakeTier(map[string][]byte{key: []byte("wheel bytes")})

	ts := newTieredStorage(l1, l2, 4, 1)
	defer func() { _ = ts.Close() }()

	ctx := context.Background()

	t.Run("Get", func(t *testing.T) {
		_, _, err := ts.Get(ctx, key)
		require.ErrorIs(t, err, diskFailure)
		assert.NotErrorIs(t, err, ErrNotFound)
		assert.Zero(t, l2.gets.Load(), "L2 must not be consulted after a real L1 failure")
	})

	t.Run("Exists", func(t *testing.T) {
		_, err := ts.Exists(ctx, key)
		require.ErrorIs(t, err, diskFailure)
	})
}

// TestTieredStorage_PutIsLocalFirst pins the ordering: the write returns as
// soon as the local file is complete, and the upload to the object store
// happens afterwards in the background.
func TestTieredStorage_PutIsLocalFirst(t *testing.T) {
	const key = "packages/numpy/numpy-1.26.0.tar.gz"
	payload := []byte("wheel bytes")

	l1, err := NewLRULocalStorage(t.TempDir(), 10*1024*1024, 0)
	require.NoError(t, err)

	l2 := newFakeTier(nil)
	l2.putGate = make(chan struct{}) // no upload may finish until released

	ts := newTieredStorage(l1, l2, 4, 1)

	ctx := context.Background()

	info, err := ts.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), "application/gzip")
	require.NoError(t, err, "Put must not wait on the object store")
	assert.Equal(t, int64(len(payload)), info.Size)

	// The client can be served right now: the local copy is complete while the
	// upload is still parked.
	exists, err := l1.Exists(ctx, key)
	require.NoError(t, err)
	assert.True(t, exists, "local write must be complete when Put returns")

	uploaded, err := l2.Exists(ctx, key)
	require.NoError(t, err)
	assert.False(t, uploaded, "Put must not have waited for the object store")

	// Releasing the object store lets the background upload land.
	close(l2.putGate)
	require.Eventually(t, func() bool {
		ok, err := l2.Exists(ctx, key)
		return err == nil && ok
	}, 5*time.Second, 10*time.Millisecond, "background upload never reached the object store")

	require.NoError(t, ts.Close())
}

// TestTieredStorage_PutSurvivesObjectStoreFailure pins that the object store is
// best-effort: an upload error is logged and dropped, the client's write still
// succeeds, and the file is present locally.
func TestTieredStorage_PutSurvivesObjectStoreFailure(t *testing.T) {
	const key = "packages/flask/flask-3.0.0.tar.gz"
	payload := []byte("more wheel bytes")

	l1, err := NewLRULocalStorage(t.TempDir(), 10*1024*1024, 0)
	require.NoError(t, err)

	l2 := newFakeTier(nil)
	l2.putErr = errors.New("s3 is having a day")

	ts := newTieredStorage(l1, l2, 4, 1)
	defer func() { _ = ts.Close() }()

	ctx := context.Background()

	_, err = ts.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), "application/gzip")
	require.NoError(t, err, "an object store failure must not fail the client's write")

	reader, _, err := l1.Get(ctx, key)
	require.NoError(t, err, "the file must be present locally")
	defer func() { _ = reader.Close() }()

	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, payload, body)

	// The upload was attempted and its failure went nowhere near the caller.
	require.Eventually(t, func() bool { return l2.puts.Load() > 0 }, 5*time.Second, 10*time.Millisecond)
}

// TestTieredStorage_CloseDrainsPendingUploads pins that a shutdown does not
// silently abandon files that were cached moments earlier.
func TestTieredStorage_CloseDrainsPendingUploads(t *testing.T) {
	l1, err := NewLRULocalStorage(t.TempDir(), 10*1024*1024, 0)
	require.NoError(t, err)

	l2 := newFakeTier(nil)

	// One worker and a queue deep enough for every key, so the uploads are
	// genuinely still pending when Close is called.
	ts := newTieredStorage(l1, l2, 8, 1)

	ctx := context.Background()
	keys := []string{"packages/a/a-1.0.tar.gz", "packages/b/b-1.0.tar.gz", "packages/c/c-1.0.tar.gz"}
	for _, key := range keys {
		_, err := ts.Put(ctx, key, bytes.NewReader([]byte(key)), int64(len(key)), "application/gzip")
		require.NoError(t, err)
	}

	require.NoError(t, ts.Close())

	for _, key := range keys {
		exists, err := l2.Exists(ctx, key)
		require.NoError(t, err)
		assert.True(t, exists, "%s was abandoned instead of drained on shutdown", key)
	}
}

// TestTieredStorage_PutRejectsTruncatedSource pins that a source that fails
// part-way through commits to neither tier: the local write must fail the call
// rather than leaving a short object behind and queueing it for upload.
func TestTieredStorage_PutRejectsTruncatedSource(t *testing.T) {
	const key = "packages/numpy/numpy-1.26.0.tar.gz"

	l1 := newFakeTier(nil)
	l2 := newFakeTier(nil)
	ts := newTieredStorage(l1, l2, 4, 1)
	defer func() { _ = ts.Close() }()

	truncated := io.MultiReader(
		bytes.NewReader([]byte("first half")),
		iotest.ErrReader(errors.New("connection reset")),
	)

	_, err := ts.Put(context.Background(), key, truncated, 100, "application/gzip")
	require.Error(t, err)

	for name, tier := range map[string]*fakeTier{"L1": l1, "L2": l2} {
		exists, err := tier.Exists(context.Background(), key)
		require.NoError(t, err)
		assert.False(t, exists, "%s must not commit a truncated object", name)
	}
}

// TestTieredStorage_MissIsSentinel pins that a genuine miss in both tiers is
// reported with the shared sentinel.
func TestTieredStorage_MissIsSentinel(t *testing.T) {
	ts := newTieredStorage(newFakeTier(nil), newFakeTier(nil), 4, 1)
	defer func() { _ = ts.Close() }()

	ctx := context.Background()

	_, _, err := ts.Get(ctx, "packages/absent/absent-1.0.0.tar.gz")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestTieredStorage_Capabilities pins that the tiered backend exposes zero-copy
// from L1, the tier that genuinely has it (real local files).
func TestTieredStorage_Capabilities(t *testing.T) {
	const key = "packages/numpy/numpy-1.26.0.tar.gz"

	l1 := newFakeTier(map[string][]byte{key: []byte("wheel bytes")})
	l2 := newFakeTier(map[string][]byte{key: []byte("wheel bytes")})

	ts := newTieredStorage(l1, l2, 4, 1)
	defer func() { _ = ts.Close() }()

	var backend Storage = ts

	zc, ok := backend.(ZeroCopyCapable)
	require.True(t, ok, "TieredStorage must be ZeroCopyCapable via L1")

	path, err := zc.GetFilePath(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, "/fake/"+key, path, "zero-copy must resolve against L1")
}

// TestLRUCache tests LRU eviction logic
func TestLRUCache(t *testing.T) {
	baseDir := t.TempDir()
	maxSize := int64(1024) // 1KB max

	local, err := NewLocalStorage(baseDir)
	if err != nil {
		t.Fatalf("Failed to create local storage: %v", err)
	}

	lru := newLRUCache(baseDir, maxSize, 0, local)
	defer func() { _ = lru.Close() }()

	t.Run("records access", func(t *testing.T) {
		lru.RecordAccess("test-key-1", 512)

		if got := cacheCount(lru); got != 1 {
			t.Errorf("Expected 1 entry, got %d", got)
		}
		if got := cacheSize(lru); got != 512 {
			t.Errorf("Expected 512 bytes, got %d", got)
		}
	})

	t.Run("triggers eviction when over size", func(t *testing.T) {
		// Add entries that exceed max size
		lru.RecordAccess("test-key-2", 400)
		lru.RecordAccess("test-key-3", 400)

		// Wait a bit for eviction worker to run
		time.Sleep(100 * time.Millisecond)

		// Should have evicted to get under maxSize
		if currentSize := cacheSize(lru); currentSize > maxSize {
			t.Errorf("Expected size <= %d after eviction, got %d", maxSize, currentSize)
		}
	})

	t.Run("deletes entry", func(t *testing.T) {
		initialCount := cacheCount(lru)

		// Add a new entry
		lru.RecordAccess("test-key-delete", 100)

		// Delete it
		lru.RecordDelete("test-key-delete")

		if finalCount := cacheCount(lru); finalCount != initialCount {
			t.Errorf("Expected count to return to %d after delete, got %d", initialCount, finalCount)
		}
	})
}

// TestLRULocalStorage tests LRU local storage wrapper
func TestLRULocalStorage(t *testing.T) {
	baseDir := t.TempDir()
	maxSize := int64(1024 * 1024) // 1MB

	storage, err := NewLRULocalStorage(baseDir, maxSize, 0)
	if err != nil {
		t.Fatalf("Failed to create LRU local storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	ctx := context.Background()

	t.Run("put and get with LRU tracking", func(t *testing.T) {
		testData := []byte("test data for LRU tracking")
		reader := bytes.NewReader(testData)

		// Put file
		info, err := storage.Put(ctx, "test/file.txt", reader, int64(len(testData)), "text/plain")
		if err != nil {
			t.Fatalf("Failed to put file: %v", err)
		}

		if info.Size != int64(len(testData)) {
			t.Errorf("Expected size %d, got %d", len(testData), info.Size)
		}

		// Get file (should update LRU)
		readCloser, _, err := storage.Get(ctx, "test/file.txt")
		if err != nil {
			t.Fatalf("Failed to get file: %v", err)
		}
		defer func() { _ = readCloser.Close() }()

		readData, err := io.ReadAll(readCloser)
		if err != nil {
			t.Fatalf("Failed to read data: %v", err)
		}

		if !bytes.Equal(testData, readData) {
			t.Errorf("Data mismatch: expected %s, got %s", testData, readData)
		}

		// Check LRU tracking
		if trackedCount(storage) < 1 {
			t.Error("Expected at least 1 entry in LRU cache")
		}
	})

	t.Run("delete with LRU tracking", func(t *testing.T) {
		testData := []byte("data to delete")
		reader := bytes.NewReader(testData)

		// Put file
		_, err := storage.Put(ctx, "test/delete-me.txt", reader, int64(len(testData)), "text/plain")
		if err != nil {
			t.Fatalf("Failed to put file: %v", err)
		}

		// Delete file
		err = storage.Delete(ctx, "test/delete-me.txt")
		if err != nil {
			t.Fatalf("Failed to delete file: %v", err)
		}

		// Verify file is deleted
		exists, err := storage.Exists(ctx, "test/delete-me.txt")
		if err != nil {
			t.Fatalf("Failed to check existence: %v", err)
		}
		if exists {
			t.Error("File should not exist after deletion")
		}
	})

	t.Run("rebuild from existing files", func(t *testing.T) {
		// Create some files directly in the directory
		testFile := filepath.Join(baseDir, "rebuild-test.txt")
		testData := []byte("rebuild test data")
		err := os.WriteFile(testFile, testData, 0644)
		if err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}

		// Create new storage that should rebuild cache
		newStorage, err := NewLRULocalStorage(baseDir, maxSize, 0)
		if err != nil {
			t.Fatalf("Failed to create new LRU storage: %v", err)
		}
		defer func() { _ = newStorage.Close() }()

		// Check that the file was discovered
		if trackedCount(newStorage) < 1 {
			t.Error("Expected cache to be rebuilt from existing files")
		}
	})
}

// TestTieredStorage_ConcurrentAccess tests concurrent operations
func TestTieredStorage_ConcurrentAccess(t *testing.T) {
	baseDir := t.TempDir()
	maxSize := int64(10 * 1024 * 1024) // 10MB

	storage, err := NewLRULocalStorage(baseDir, maxSize, 0)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	ctx := context.Background()

	// Perform concurrent puts and gets
	done := make(chan bool)
	for i := range 5 {
		go func(n int) {
			key := filepath.Join("concurrent", "file", string(rune('a'+n))+".txt")
			data := bytes.Repeat([]byte{byte(n)}, 100)

			// Put
			_, err := storage.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "application/octet-stream")
			if err != nil {
				t.Errorf("Concurrent put failed: %v", err)
			}

			// Get
			reader, _, err := storage.Get(ctx, key)
			if err != nil {
				t.Errorf("Concurrent get failed: %v", err)
			} else {
				_ = reader.Close()
			}

			done <- true
		}(i)
	}

	// Wait for all goroutines
	for range 5 {
		<-done
	}
}
