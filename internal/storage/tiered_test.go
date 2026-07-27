package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
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

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data

	return &ObjectInfo{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeTier) Stat(_ context.Context, key string) (*ObjectInfo, error) {
	data, err := f.load(key)
	if err != nil {
		return nil, err
	}
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

var _ l1Storage = (*fakeTier)(nil)

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

// TestTieredStorage_BasicOperations tests basic tiered storage operations
func TestTieredStorage_BasicOperations(t *testing.T) {
	// Create temporary directories
	localDir := t.TempDir()

	// Create tiered storage (without real S3, just test structure)
	// Note: This is a basic structure test. Full integration tests would use MinIO
	t.Run("creation", func(t *testing.T) {
		// Just verify local cache creation works
		lruCache, err := NewLRULocalStorage(localDir, 1024*1024*10, 0)
		if err != nil {
			t.Fatalf("Failed to create LRU local storage: %v", err)
		}
		defer func() { _ = lruCache.Close() }()

		if lruCache == nil {
			t.Fatal("Expected non-nil LRU cache")
		}
	})
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
