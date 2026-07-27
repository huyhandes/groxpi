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

// cacheSize returns the cache's own view of how many bytes it holds.
func cacheSize(c *LRUCache) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.currentSize
}

// cacheCount returns how many entries the cache is tracking.
func cacheCount(c *LRUCache) int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.lruList.Len()
}

// trackedSize returns the LRU cache's own view of how many bytes it holds.
func trackedSize(s *LRULocalStorage) int64 {
	return cacheSize(s.lruCache)
}

// trackedCount returns how many entries the wrapper's LRU cache is tracking.
func trackedCount(s *LRULocalStorage) int {
	return cacheCount(s.lruCache)
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

// TestLRULocalStorage_ExpiresEntriesUnderQuota pins that TTL expiry is
// independent of the size limit: an entry past its TTL must be evicted even
// though the cache is nowhere near maxSize. Size-driven eviction on its own
// leaves stale content resident for as long as the cache stays under quota,
// which makes a configured TTL a no-op on any cache that never fills up.
func TestLRULocalStorage_ExpiresEntriesUnderQuota(t *testing.T) {
	dir := t.TempDir()

	// maxSize is orders of magnitude larger than what is written, so nothing
	// here can trigger size-driven eviction.
	s, err := NewLRULocalStorage(dir, 1<<20, 100*time.Millisecond)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	putBlob(t, s, "stale.bin", 128)
	require.Equal(t, int64(128), trackedSize(s))

	require.Eventually(t, func() bool {
		return trackedSize(s) == 0
	}, 3*time.Second, 10*time.Millisecond,
		"entry past its TTL was never expired while the cache was under quota")

	_, err = os.Stat(filepath.Join(dir, "stale.bin"))
	assert.True(t, os.IsNotExist(err), "expired entry left its file behind on disk")
}

// TestLRULocalStorage_ForwardsCapabilities pins the wrapper's capability
// contract: it must expose every capability its inner storage genuinely has. A
// wrapper that silently drops zero-copy would push the hottest read path onto a
// byte-by-byte copy.
func TestLRULocalStorage_ForwardsCapabilities(t *testing.T) {
	dir := t.TempDir()

	s, err := NewLRULocalStorage(dir, 0, 0)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	var backend Storage = s

	zc, ok := backend.(ZeroCopyCapable)
	require.True(t, ok, "wrapper dropped the inner storage's zero-copy capability")

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
	assert.Equal(t, onDiskCount(t, dir), trackedCount(s),
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

// Delete records the key only once the backend has actually removed it, so a
// caller that observes the key can also rely on the file being gone.
func (s *spyDeleter) Delete(ctx context.Context, key string) error {
	if err := s.inner.Delete(ctx, key); err != nil {
		return err
	}

	s.mu.Lock()
	s.keys = append(s.keys, key)
	s.mu.Unlock()

	return nil
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
		cache.RecordWrite(key, 250)
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

// TestLRULocalStorage_SnapshotReportsServingStats pins the columns the
// administrative listing needs: size, age, hit count and last-served time, with
// the hit count rising each time the same object is served.
func TestLRULocalStorage_SnapshotReportsServingStats(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	s, err := NewLRULocalStorage(dir, 0, 0)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	before := time.Now()
	putBlob(t, s, "packages/numpy/numpy-2.0.0.tar.gz", 128)

	rows := s.Snapshot()
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, "packages/numpy/numpy-2.0.0.tar.gz", row.Key)
	assert.Equal(t, int64(128), row.Size)
	assert.False(t, row.CreatedAt.Before(before), "creation time must be usable as an age")
	assert.Zero(t, row.Hits, "a write is not a serve")

	for i := range 3 {
		rc, _, err := s.Get(ctx, "packages/numpy/numpy-2.0.0.tar.gz")
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())

		rows = s.Snapshot()
		require.Len(t, rows, 1)
		assert.Equal(t, int64(i+1), rows[0].Hits, "hit count must rise on every serve")
		assert.False(t, rows[0].LastServed.Before(before), "last-served time was not recorded")
	}

	// The snapshot is a copy: mutating it cannot corrupt the cache.
	rows[0].Size = -1
	assert.Equal(t, int64(128), s.Snapshot()[0].Size)
}

// TestLRULocalStorage_DeletePrefixKeepsAccountingConsistent pins that a
// prefix delete removes exactly the matching objects, through the cache's own
// deletion path, leaving size accounting and recency ordering intact.
func TestLRULocalStorage_DeletePrefixKeepsAccountingConsistent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	s, err := NewLRULocalStorage(dir, 0, 0)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	putBlob(t, s, "packages/numpy/numpy-2.0.0.tar.gz", 100)
	putBlob(t, s, "packages/numpy/numpy-2.0.0-py3-none-any.whl", 200)
	// A package whose name merely starts with the evicted one must survive.
	putBlob(t, s, "packages/numpy-stubs/numpy-stubs-1.0.0.tar.gz", 300)

	deleted, err := s.DeletePrefix(ctx, "packages/numpy/")
	require.NoError(t, err)
	assert.Equal(t, 2, deleted)

	for _, key := range []string{"packages/numpy/numpy-2.0.0.tar.gz", "packages/numpy/numpy-2.0.0-py3-none-any.whl"} {
		exists, err := s.Exists(ctx, key)
		require.NoError(t, err)
		assert.False(t, exists, "%s survived the prefix delete", key)
	}

	exists, err := s.Exists(ctx, "packages/numpy-stubs/numpy-stubs-1.0.0.tar.gz")
	require.NoError(t, err)
	assert.True(t, exists, "prefix delete matched a different package")

	assert.Equal(t, int64(300), trackedSize(s))
	assert.Equal(t, onDiskSize(t, dir), trackedSize(s))
	assert.Equal(t, 1, trackedCount(s))
	require.Len(t, s.Snapshot(), 1)

	// Deleting a prefix nothing matches is not an error.
	deleted, err = s.DeletePrefix(ctx, "packages/absent/")
	require.NoError(t, err)
	assert.Equal(t, 0, deleted)
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
