package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLocalStorage(t *testing.T) {
	t.Run("creates_base_directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		baseDir := filepath.Join(tmpDir, "test-storage")

		localStorage, err := NewLocalStorage(baseDir, 0, 0)
		if err != nil {
			t.Fatalf("NewLocalStorage failed: %v", err)
		}

		// Verify directory was created
		if _, err := os.Stat(baseDir); os.IsNotExist(err) {
			t.Error("Base directory was not created")
		}

		// Files land under it.
		putN(t, localStorage, "packages/p/f", 1)
		assert.FileExists(t, filepath.Join(baseDir, "packages", "p", "f"))
	})

	t.Run("handles_existing_directory", func(t *testing.T) {
		tmpDir := t.TempDir() // This already exists

		storage, err := NewLocalStorage(tmpDir, 0, 0)
		if err != nil {
			t.Fatalf("NewLocalStorage failed with existing directory: %v", err)
		}

		putN(t, storage, "packages/p/f", 1)
		assert.FileExists(t, filepath.Join(tmpDir, "packages", "p", "f"))
	})

	t.Run("fails_with_permission_error", func(t *testing.T) {
		// Try to create in a location that should fail (root directory with invalid permissions)
		invalidDir := "/root/invalid-test-dir"

		_, err := NewLocalStorage(invalidDir, 0, 0)
		if err == nil {
			t.Error("Expected error when creating storage in invalid location")
		}
	})
}

func TestLocalStorage_Get(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir(), 0, 0)
	ctx := context.Background()

	// Setup: Create a test file
	key := "test-get.txt"
	originalContent := "test content for get"
	put(t, storage, key, []byte(originalContent))

	t.Run("retrieves_file_successfully", func(t *testing.T) {
		reader, info, err := storage.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		defer func() { _ = reader.Close() }()

		if info.Size != int64(len(originalContent)) {
			t.Errorf("Expected size %d, got %d", len(originalContent), info.Size)
		}

		// Read content and verify
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("Failed to read content: %v", err)
		}

		if string(content) != originalContent {
			t.Errorf("Expected content %q, got %q", originalContent, string(content))
		}
	})

	t.Run("returns_error_for_non_existent_file", func(t *testing.T) {
		_, _, err := storage.Get(ctx, "non-existent.txt")
		if err == nil {
			t.Error("Expected error for non-existent file")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("Expected 'not found' error, got: %v", err)
		}
	})
}

func TestLocalStorage_NotFoundIsSentinel(t *testing.T) {
	s, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	ctx := context.Background()

	const missing = "no/such/object.txt"

	t.Run("Get", func(t *testing.T) {
		_, _, err := s.Get(ctx, missing)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestLocalStorage_Close(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir(), 0, 0)

	// Close should not return an error
	err := storage.Close()
	if err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

// put writes data under key the way the store does: a temp file in key's
// directory, committed by rename.
func put(t testing.TB, s *LocalStorage, key string, data []byte) {
	t.Helper()
	dir := filepath.Dir(s.buildPath(key))
	require.NoError(t, os.MkdirAll(dir, 0o750))
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	require.NoError(t, err)
	_, err = tmp.Write(data)
	require.NoError(t, errors.Join(err, tmp.Close()))
	require.NoError(t, s.commit(context.Background(), tmp.Name(), key, int64(len(data))))
}

// putN writes n bytes under key.
func putN(t *testing.T, s *LocalStorage, key string, n int) {
	t.Helper()
	put(t, s, key, bytes.Repeat([]byte("x"), n))
}

// setAtime sets key's atime to ago in the past, leaving mtime alone.
func setAtime(t *testing.T, s *LocalStorage, key string, ago time.Duration) {
	t.Helper()
	path := s.buildPath(key)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Chtimes(path, time.Now().Add(-ago), info.ModTime()))
}

func keys(entries []CacheEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Key
	}
	return out
}

func total(entries []CacheEntry) int64 {
	var n int64
	for _, e := range entries {
		n += e.Size
	}
	return n
}

// assertAccounted pins the size accounting through eviction, the only thing
// it drives: filling the cache to exactly maxSize evicts nothing, and one byte
// more evicts down to the low watermark. A cache that undercounts what is on
// disk never crosses. The probe files are the newest, so they outlive the
// eviction; the test's own files may not, so this ends a scenario.
func assertAccounted(t *testing.T, s *LocalStorage, maxSize int64) {
	t.Helper()
	before := s.Snapshot()
	held := keys(before)
	if fill := maxSize - total(before); fill > 0 {
		putN(t, s, "packages/zz-probe/fill", int(fill))
		held = append(held, "packages/zz-probe/fill")
	}
	assert.ElementsMatch(t, held, keys(s.Snapshot()), "a cache at exactly maxSize must evict nothing")

	putN(t, s, "packages/zz-probe/over", 1)
	after := s.Snapshot()
	assert.LessOrEqual(t, total(after), maxSize*lowWatermarkPercent/100, "crossing maxSize must evict to the low watermark")
	assert.Contains(t, keys(after), "packages/zz-probe/over", "eviction takes the oldest-accessed files first")
}

func TestLocalStorage_EvictsOldestByAtime(t *testing.T) {
	s, err := NewLocalStorage(t.TempDir(), 1000, 0)
	require.NoError(t, err)

	for _, k := range []string{"packages/a", "packages/b", "packages/c", "packages/d"} {
		putN(t, s, k, 250)
	}
	setAtime(t, s, "packages/c", 3*time.Hour)
	setAtime(t, s, "packages/a", 2*time.Hour)
	setAtime(t, s, "packages/b", time.Hour)

	putN(t, s, "packages/e", 250) // 1250 > 1000: evict oldest down to 900

	assert.ElementsMatch(t, []string{"packages/b", "packages/d", "packages/e"}, keys(s.Snapshot()))
	assertAccounted(t, s, 1000) // what is left on disk is what is counted
}

func TestLocalStorage_RestartPreservesRecency(t *testing.T) {
	dir := t.TempDir()
	s, err := NewLocalStorage(dir, 1000, 0)
	require.NoError(t, err)

	for _, k := range []string{"packages/a", "packages/b", "packages/c"} {
		putN(t, s, k, 250)
	}
	setAtime(t, s, "packages/a", 3*time.Hour)
	setAtime(t, s, "packages/b", 2*time.Hour)
	setAtime(t, s, "packages/c", time.Hour)
	err = getClose(s, "packages/a") // a hit makes a the most recent
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s, err = NewLocalStorage(dir, 1000, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"packages/a", "packages/c", "packages/b"}, keys(s.Snapshot()))

	// Over only if the startup walk seeded the size with the 750 on disk.
	putN(t, s, "packages/d", 250)
	putN(t, s, "packages/e", 250) // over: b then c go, a survives
	assert.ElementsMatch(t, []string{"packages/a", "packages/d", "packages/e"}, keys(s.Snapshot()))
}

// setMtime sets key's mtime to ago in the past and its atime to now: an old
// write that was just read.
func setMtime(t *testing.T, s *LocalStorage, key string, ago time.Duration) {
	t.Helper()
	require.NoError(t, os.Chtimes(s.buildPath(key), time.Now(), time.Now().Add(-ago)))
}

// TestLocalStorage_TTLExpiresIdleCacheByWriteTime pins GROXPI_LOCAL_CACHE_TTL:
// age is measured from the write (mtime), not the last read, and the
// background sweep expires files with no Put to trigger a walk.
func TestLocalStorage_TTLExpiresIdleCacheByWriteTime(t *testing.T) {
	s, err := NewLocalStorage(t.TempDir(), 0, 200*time.Millisecond)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	putN(t, s, "packages/old-but-read", 10)
	putN(t, s, "packages/fresh", 10)
	setMtime(t, s, "packages/old-but-read", time.Hour)

	assert.Eventually(t, func() bool {
		ks := keys(s.Snapshot())
		return len(ks) == 1 && ks[0] == "packages/fresh"
	}, 2*time.Second, 20*time.Millisecond, "a recently read file written past the TTL must expire with no writes")

	assert.Eventually(t, func() bool { return len(s.Snapshot()) == 0 }, 2*time.Second, 20*time.Millisecond,
		"an idle file must expire once its write is older than the TTL")
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "Close must be idempotent")
}

// TestLocalStorage_RemovesStaleTempFiles pins crash-leftover cleanup: a temp
// file untouched for longer than touchInterval goes, a fresh one (a write in
// progress) stays, and neither is listed.
func TestLocalStorage_RemovesStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, packagesDir, "p")
	require.NoError(t, os.MkdirAll(pkg, 0o750))
	stale, fresh := filepath.Join(pkg, tmpPrefix+"stale"), filepath.Join(pkg, tmpPrefix+"fresh")
	require.NoError(t, os.WriteFile(stale, []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(fresh, []byte("x"), 0o600))
	old := time.Now().Add(-2 * touchInterval)
	require.NoError(t, os.Chtimes(stale, old, old))

	s, err := NewLocalStorage(dir, 100, 0)
	require.NoError(t, err)

	assert.NoFileExists(t, stale)
	assert.FileExists(t, fresh)
	assert.Empty(t, s.Snapshot())
	assertAccounted(t, s, 100) // temp files do not count
}

// TestLocalStorage_SizeAccounting pins the accounting across an overwrite and
// a prefix delete: eviction must see the bytes on disk.
func TestLocalStorage_SizeAccounting(t *testing.T) {
	const maxSize = 1000
	fill := func(t *testing.T) *LocalStorage {
		t.Helper()
		s, err := NewLocalStorage(t.TempDir(), maxSize, 0)
		require.NoError(t, err)
		putN(t, s, "packages/a/1", 100)
		putN(t, s, "packages/a/1", 40) // overwrite replaces, not adds
		putN(t, s, "packages/a/2", 10)
		putN(t, s, "packages/b/1", 7)
		return s
	}

	t.Run("overwrite", func(t *testing.T) {
		s := fill(t)
		assert.Equal(t, int64(57), total(s.Snapshot()))
		assertAccounted(t, s, maxSize)
	})

	t.Run("delete_prefix", func(t *testing.T) {
		s := fill(t)
		n, err := s.DeletePrefix(context.Background(), "packages/a/")
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		assert.Equal(t, []string{"packages/b/1"}, keys(s.Snapshot()))
		assertAccounted(t, s, maxSize)
	})
}

func TestLocalStorage_HitSetsAtimeExplicitly(t *testing.T) {
	s, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)
	putN(t, s, "k", 10)

	path := s.buildPath("k")
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, old, old))

	err = getClose(s, "k")
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), atime(info), time.Minute, "a hit must bump atime without relying on the mount")
	assert.True(t, info.ModTime().Equal(old), "mtime is Last-Modified and must not move")

	recent := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, recent, old))
	err = getClose(s, "k")
	require.NoError(t, err)
	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.True(t, atime(info).Equal(recent), "a hit within the throttle window must not touch the file")
}

// getClose is one cache hit: open and close.
func getClose(s *LocalStorage, key string) error {
	rc, _, err := s.Get(context.Background(), key)
	if err == nil {
		err = rc.Close()
	}
	return err
}

// TestLocalStorage_OwnsOnlyPackagesDir pins the cache's walk scope: over a
// shared directory (the default is os.TempDir()), foreign files and stale temp
// files outside packages/ are never listed, counted, evicted or reaped.
func TestLocalStorage_OwnsOnlyPackagesDir(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * touchInterval)
	foreign := []string{
		filepath.Join(dir, "other.txt"),
		filepath.Join(dir, tmpPrefix+"x"),
		filepath.Join(dir, "sub", "y"),
		filepath.Join(dir, "sub", tmpPrefix+"z"),
	}
	for _, p := range foreign {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte("foreign"), 0o600))
		require.NoError(t, os.Chtimes(p, old, old))
	}
	cached := filepath.Join(dir, "packages", "p", "f")
	require.NoError(t, os.MkdirAll(filepath.Dir(cached), 0o750))
	require.NoError(t, os.WriteFile(cached, []byte("cached"), 0o600))

	s, err := NewLocalStorage(dir, 100, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"packages/p/f"}, keys(s.Snapshot()))

	putN(t, s, "packages/p/g", 200) // over budget: evicts only under packages/
	for _, p := range foreign {
		assert.FileExists(t, p)
	}
	assert.Empty(t, s.Snapshot())
	assertAccounted(t, s, 100) // foreign files do not count toward the budget
}
