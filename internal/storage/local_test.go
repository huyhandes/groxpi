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

		// Now we can test internal fields since we're in the same package
		if localStorage.baseDir != baseDir {
			t.Errorf("Expected baseDir %s, got %s", baseDir, localStorage.baseDir)
		}

		// Verify directory was created
		if _, err := os.Stat(baseDir); os.IsNotExist(err) {
			t.Error("Base directory was not created")
		}
	})

	t.Run("handles_existing_directory", func(t *testing.T) {
		tmpDir := t.TempDir() // This already exists

		storage, err := NewLocalStorage(tmpDir, 0, 0)
		if err != nil {
			t.Fatalf("NewLocalStorage failed with existing directory: %v", err)
		}

		if storage.baseDir != tmpDir {
			t.Errorf("Expected baseDir %s, got %s", tmpDir, storage.baseDir)
		}
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

func TestLocalStorage_EvictsOldestByAtime(t *testing.T) {
	s, err := NewLocalStorage(t.TempDir(), 1000, 0)
	require.NoError(t, err)

	for _, k := range []string{"a", "b", "c", "d"} {
		putN(t, s, k, 250)
	}
	setAtime(t, s, "c", 3*time.Hour)
	setAtime(t, s, "a", 2*time.Hour)
	setAtime(t, s, "b", time.Hour)

	putN(t, s, "e", 250) // 1250 > 1000: evict oldest down to 900

	assert.ElementsMatch(t, []string{"b", "d", "e"}, keys(s.Snapshot()))
	assert.Equal(t, int64(750), s.size.Load(), "the counter must match what is left on disk")
}

func TestLocalStorage_RestartPreservesRecency(t *testing.T) {
	dir := t.TempDir()
	s, err := NewLocalStorage(dir, 1000, 0)
	require.NoError(t, err)

	for _, k := range []string{"a", "b", "c"} {
		putN(t, s, k, 250)
	}
	setAtime(t, s, "a", 3*time.Hour)
	setAtime(t, s, "b", 2*time.Hour)
	setAtime(t, s, "c", time.Hour)
	err = getClose(s, "a") // a hit makes a the most recent
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s, err = NewLocalStorage(dir, 1000, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(750), s.size.Load(), "the startup walk must seed the size counter")
	assert.Equal(t, []string{"a", "c", "b"}, keys(s.Snapshot()))

	putN(t, s, "d", 250)
	putN(t, s, "e", 250) // over: b then c go, a survives
	assert.ElementsMatch(t, []string{"a", "d", "e"}, keys(s.Snapshot()))
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

	putN(t, s, "old-but-read", 10)
	putN(t, s, "fresh", 10)
	setMtime(t, s, "old-but-read", time.Hour)

	assert.Eventually(t, func() bool {
		ks := keys(s.Snapshot())
		return len(ks) == 1 && ks[0] == "fresh"
	}, 2*time.Second, 20*time.Millisecond, "a recently read file written past the TTL must expire with no writes")
	assert.Equal(t, int64(10), s.size.Load())

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
	stale, fresh := filepath.Join(dir, tmpPrefix+"stale"), filepath.Join(dir, tmpPrefix+"fresh")
	require.NoError(t, os.WriteFile(stale, []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(fresh, []byte("x"), 0o600))
	old := time.Now().Add(-2 * touchInterval)
	require.NoError(t, os.Chtimes(stale, old, old))

	s, err := NewLocalStorage(dir, 0, 0)
	require.NoError(t, err)

	assert.NoFileExists(t, stale)
	assert.FileExists(t, fresh)
	assert.Empty(t, s.Snapshot())
	assert.Zero(t, s.size.Load())
}

// TestLocalStorage_SizeAccounting pins the counter across an overwrite and a
// prefix delete: it must equal the bytes on disk.
func TestLocalStorage_SizeAccounting(t *testing.T) {
	ctx := context.Background()
	s, err := NewLocalStorage(t.TempDir(), 0, 0)
	require.NoError(t, err)

	putN(t, s, "packages/a/1", 100)
	putN(t, s, "packages/a/1", 40) // overwrite replaces, not adds
	putN(t, s, "packages/a/2", 10)
	putN(t, s, "packages/b/1", 7)
	assert.Equal(t, int64(57), s.size.Load())

	n, err := s.DeletePrefix(ctx, "packages/a/")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, int64(7), s.size.Load())
	assert.Equal(t, []string{"packages/b/1"}, keys(s.Snapshot()))
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
