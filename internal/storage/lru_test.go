package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// onDiskSize sums the sizes of all regular files under dir.
func onDiskSize(t *testing.T, dir string) int64 {
	t.Helper()

	var total int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	require.NoError(t, err)

	return total
}

// onDiskCount counts regular files under dir.
func onDiskCount(t *testing.T, dir string) int {
	t.Helper()

	count := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	require.NoError(t, err)

	return count
}

// putBlob writes a blob of n bytes under key.
func putBlob(t *testing.T, s *LRULocalStorage, key string, n int) {
	t.Helper()

	data := bytes.Repeat([]byte("x"), n)
	info, err := s.Put(context.Background(), key, bytes.NewReader(data), int64(n), "application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, int64(n), info.Size)
}

// trackedSize returns the LRU cache's own view of how many bytes it holds.
func trackedSize(s *LRULocalStorage) int64 {
	return s.GetStats()["current_size_bytes"].(int64)
}

// waitForSize waits until the LRU has evicted down to at most maxSize.
func waitForSize(t *testing.T, s *LRULocalStorage, maxSize int64) {
	t.Helper()

	require.Eventually(t, func() bool {
		return trackedSize(s) <= maxSize
	}, 3*time.Second, 5*time.Millisecond, "LRU never evicted down to %d bytes (still %d)", maxSize, trackedSize(s))
}

// TestLRULocalStorage_HotFileSurvivesEviction is the headline test: a file read
// repeatedly through *any* read path must be treated as recently used and must
// not be evicted while a colder file is still resident.
//
// It fails against an embedded LocalStorage, because GetFilePath / Stat fall
// through to the embedded type and never reach RecordAccess.
func TestLRULocalStorage_HotFileSurvivesEviction(t *testing.T) {
	ctx := context.Background()

	const (
		blobSize = 250
		maxSize  = int64(1000)
	)

	readPaths := map[string]func(t *testing.T, s *LRULocalStorage, key string){
		"GetFilePath": func(t *testing.T, s *LRULocalStorage, key string) {
			path, err := s.GetFilePath(ctx, key)
			require.NoError(t, err)
			require.NotEmpty(t, path)
		},
		"Stat": func(t *testing.T, s *LRULocalStorage, key string) {
			info, err := s.Stat(ctx, key)
			require.NoError(t, err)
			require.Equal(t, int64(blobSize), info.Size)
		},
		"Get": func(t *testing.T, s *LRULocalStorage, key string) {
			rc, _, err := s.Get(ctx, key)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, rc)
			require.NoError(t, err)
			require.NoError(t, rc.Close())
		},
	}

	for name, read := range readPaths {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			s, err := NewLRULocalStorage(dir, maxSize, 0)
			require.NoError(t, err)
			defer func() { _ = s.Close() }()

			// hot.bin is written first, so it starts out as the *least* recently
			// used entry. Only repeated reads can save it.
			putBlob(t, s, "hot.bin", blobSize)
			putBlob(t, s, "cold.bin", blobSize)

			for range 5 {
				read(t, s, "hot.bin")
			}

			// Push the cache over its limit.
			putBlob(t, s, "filler-1.bin", blobSize)
			putBlob(t, s, "filler-2.bin", blobSize)
			putBlob(t, s, "filler-3.bin", blobSize)

			waitForSize(t, s, maxSize)

			hotExists, err := s.Exists(ctx, "hot.bin")
			require.NoError(t, err)
			assert.True(t, hotExists, "frequently read file was evicted while a colder file survived")

			coldExists, err := s.Exists(ctx, "cold.bin")
			require.NoError(t, err)
			assert.False(t, coldExists, "expected the cold file to be the eviction victim")
		})
	}
}

// TestLRULocalStorage_ForwardsCapabilities pins the wrapper's capability
// contract: it must expose every capability its inner storage genuinely has and
// none that it does not. A wrapper that silently drops zero-copy would push the
// hottest read path onto a byte-by-byte copy; one that advertised presigning
// would hand callers a URL nothing can serve.
func TestLRULocalStorage_ForwardsCapabilities(t *testing.T) {
	dir := t.TempDir()

	s, err := NewLRULocalStorage(dir, 0, 0)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	var backend Storage = s

	zc, ok := backend.(ZeroCopyCapable)
	require.True(t, ok, "wrapper dropped the inner storage's zero-copy capability")

	_, isPresignable := backend.(Presignable)
	assert.False(t, isPresignable, "wrapper advertised a capability local storage does not have")

	putBlob(t, s, "served.bin", 64)

	path, err := zc.GetFilePath(context.Background(), "served.bin")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "served.bin"), path)

	// Forwarding must not bypass the wrapper's bookkeeping.
	assert.Equal(t, int64(64), trackedSize(s))
}

// TestLRULocalStorage_NotFoundIsSentinel pins that the wrapper forwards the
// sentinel rather than rewriting it into some wrapper-specific error.
func TestLRULocalStorage_NotFoundIsSentinel(t *testing.T) {
	s, err := NewLRULocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	ctx := context.Background()

	_, _, err = s.Get(ctx, "absent.bin")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.Stat(ctx, "absent.bin")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.GetFilePath(ctx, "absent.bin")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestLRULocalStorage_SizeAccountingMatchesDisk verifies the tracked size never
// drifts from what is actually on disk across a series of puts and evictions.
func TestLRULocalStorage_SizeAccountingMatchesDisk(t *testing.T) {
	dir := t.TempDir()
	maxSize := int64(1000)

	s, err := NewLRULocalStorage(dir, maxSize, 0)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	for i := range 10 {
		putBlob(t, s, filepath.Join("pkg", string(rune('a'+i))+".bin"), 250)
	}

	waitForSize(t, s, maxSize)

	assert.Equal(t, onDiskSize(t, dir), trackedSize(s),
		"tracked size drifted from the bytes actually on disk")
	assert.Equal(t, onDiskCount(t, dir), s.GetStats()["entry_count"].(int),
		"tracked entry count drifted from the files actually on disk")
	assert.LessOrEqual(t, trackedSize(s), maxSize)
}

// TestLRULocalStorage_EvictionRemovesFilesFromDisk checks eviction actually
// unlinks the victim rather than only dropping it from the bookkeeping.
func TestLRULocalStorage_EvictionRemovesFilesFromDisk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	maxSize := int64(500)

	s, err := NewLRULocalStorage(dir, maxSize, 0)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	putBlob(t, s, "victim.bin", 250)
	putBlob(t, s, "keep-1.bin", 250)
	putBlob(t, s, "keep-2.bin", 250)

	waitForSize(t, s, maxSize)

	exists, err := s.Exists(ctx, "victim.bin")
	require.NoError(t, err)
	assert.False(t, exists, "evicted entry left its file behind on disk")

	_, err = os.Stat(filepath.Join(dir, "victim.bin"))
	assert.True(t, os.IsNotExist(err), "expected victim file to be unlinked, got err=%v", err)
}

// spyDeleter records the keys eviction asks the backend to remove.
type spyDeleter struct {
	inner *LocalStorage
	mu    sync.Mutex
	keys  []string
}

func (s *spyDeleter) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.keys = append(s.keys, key)
	s.mu.Unlock()

	return s.inner.Delete(ctx, key)
}

func (s *spyDeleter) deleted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.keys...)
}

// TestLRUCache_EvictsThroughDeleter verifies eviction is routed through the
// storage backend's Delete rather than unlinking files behind its back.
func TestLRUCache_EvictsThroughDeleter(t *testing.T) {
	dir := t.TempDir()

	local, err := NewLocalStorage(dir)
	require.NoError(t, err)
	spy := &spyDeleter{inner: local}

	cache := newLRUCache(dir, 500, 0, spy)
	defer func() { _ = cache.Close() }()

	for _, key := range []string{"a.bin", "b.bin", "c.bin"} {
		_, err := local.Put(context.Background(), key, bytes.NewReader(bytes.Repeat([]byte("x"), 250)), 250, "")
		require.NoError(t, err)
		require.NoError(t, cache.RecordWrite(key, 250))
	}

	require.Eventually(t, func() bool {
		return len(spy.deleted()) > 0
	}, 3*time.Second, 5*time.Millisecond, "eviction never called the backend's Delete")

	assert.Equal(t, []string{"a.bin"}, spy.deleted(), "expected the least recently used key to be deleted")

	_, err = os.Stat(filepath.Join(dir, "a.bin"))
	assert.True(t, os.IsNotExist(err))
}

// TestLRULocalStorage_OverwriteKeepsSizeAccurate verifies that rewriting a key
// with a different payload size updates the accounting instead of leaving the
// stale size behind.
func TestLRULocalStorage_OverwriteKeepsSizeAccurate(t *testing.T) {
	dir := t.TempDir()

	s, err := NewLRULocalStorage(dir, 0, 0) // unlimited: no eviction interference
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	putBlob(t, s, "resized.bin", 100)
	assert.Equal(t, int64(100), trackedSize(s))

	putBlob(t, s, "resized.bin", 400)
	assert.Equal(t, int64(400), trackedSize(s), "overwrite left a stale size in the accounting")
	assert.Equal(t, onDiskSize(t, dir), trackedSize(s))
}

// TestLRULocalStorage_ConcurrentReadsUnderEvictionPressure exercises every read
// path concurrently while eviction runs, for the -race detector.
func TestLRULocalStorage_ConcurrentReadsUnderEvictionPressure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	s, err := NewLRULocalStorage(dir, 2000, 0)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	putBlob(t, s, "shared.bin", 250)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for range 20 {
				_, _ = s.GetFilePath(ctx, "shared.bin")
				_, _ = s.Stat(ctx, "shared.bin")
				if rc, _, err := s.Get(ctx, "shared.bin"); err == nil {
					_, _ = io.Copy(io.Discard, rc)
					_ = rc.Close()
				}
				putBlob(t, s, filepath.Join("churn", string(rune('a'+n))+".bin"), 250)
			}
		}(i)
	}
	wg.Wait()

	assert.LessOrEqual(t, trackedSize(s), int64(2000)+250*8)
}
