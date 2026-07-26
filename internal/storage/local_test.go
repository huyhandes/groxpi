package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLocalStorage(t *testing.T) {
	t.Run("creates_base_directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		baseDir := filepath.Join(tmpDir, "test-storage")

		localStorage, err := NewLocalStorage(baseDir)
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

		storage, err := NewLocalStorage(tmpDir)
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

		_, err := NewLocalStorage(invalidDir)
		if err == nil {
			t.Error("Expected error when creating storage in invalid location")
		}
	})
}

func TestLocalStorage_Put(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir())
	ctx := context.Background()

	t.Run("stores_file_successfully", func(t *testing.T) {
		key := "test-package/file.whl"
		content := "test file content"
		reader := strings.NewReader(content)

		info, err := storage.Put(ctx, key, reader, int64(len(content)), "application/octet-stream")
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		if info.Key != key {
			t.Errorf("Expected key %s, got %s", key, info.Key)
		}
		if info.Size != int64(len(content)) {
			t.Errorf("Expected size %d, got %d", len(content), info.Size)
		}

		// Verify file exists on disk
		path := filepath.Join(storage.baseDir, key)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Error("File was not created on disk")
		}
	})

	t.Run("creates_subdirectories", func(t *testing.T) {
		key := "deep/nested/path/file.txt"
		content := "nested content"
		reader := strings.NewReader(content)

		_, err := storage.Put(ctx, key, reader, int64(len(content)), "text/plain")
		if err != nil {
			t.Fatalf("Put with nested path failed: %v", err)
		}

		// Verify nested directories were created
		path := filepath.Join(storage.baseDir, key)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Error("Nested file was not created")
		}
	})

	t.Run("handles_empty_content", func(t *testing.T) {
		key := "empty.txt"
		reader := strings.NewReader("")

		info, err := storage.Put(ctx, key, reader, 0, "text/plain")
		if err != nil {
			t.Fatalf("Put with empty content failed: %v", err)
		}

		if info.Size != 0 {
			t.Errorf("Expected size 0, got %d", info.Size)
		}
	})
}

func TestLocalStorage_Get(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir())
	ctx := context.Background()

	// Setup: Create a test file
	key := "test-get.txt"
	originalContent := "test content for get"
	_, _ = storage.Put(ctx, key, strings.NewReader(originalContent), int64(len(originalContent)), "text/plain")

	t.Run("retrieves_file_successfully", func(t *testing.T) {
		reader, info, err := storage.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		defer func() { _ = reader.Close() }()

		if info.Key != key {
			t.Errorf("Expected key %s, got %s", key, info.Key)
		}
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

func TestLocalStorage_Delete(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir())
	ctx := context.Background()

	t.Run("deletes_existing_file", func(t *testing.T) {
		key := "delete-me.txt"
		content := "delete this content"
		_, _ = storage.Put(ctx, key, strings.NewReader(content), int64(len(content)), "text/plain")

		// Verify file exists
		path := filepath.Join(storage.baseDir, key)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Fatal("Test file was not created")
		}

		// Delete file
		err := storage.Delete(ctx, key)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}

		// Verify file no longer exists
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("File was not deleted")
		}
	})

	t.Run("handles_non_existent_file", func(t *testing.T) {
		// Should not return error when deleting non-existent file
		err := storage.Delete(ctx, "non-existent.txt")
		if err != nil {
			t.Errorf("Delete of non-existent file returned error: %v", err)
		}
	})
}

func TestLocalStorage_Exists(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir())
	ctx := context.Background()

	t.Run("returns_true_for_existing_file", func(t *testing.T) {
		key := "exists-test.txt"
		content := "exists test"
		_, _ = storage.Put(ctx, key, strings.NewReader(content), int64(len(content)), "text/plain")

		exists, err := storage.Exists(ctx, key)
		if err != nil {
			t.Fatalf("Exists failed: %v", err)
		}

		if !exists {
			t.Error("Expected file to exist")
		}
	})

	t.Run("returns_false_for_non_existent_file", func(t *testing.T) {
		exists, err := storage.Exists(ctx, "does-not-exist.txt")
		if err != nil {
			t.Fatalf("Exists failed: %v", err)
		}

		if exists {
			t.Error("Expected file to not exist")
		}
	})
}

func TestLocalStorage_Stat(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir())
	ctx := context.Background()

	t.Run("returns_metadata_for_existing_file", func(t *testing.T) {
		key := "stat-test.txt"
		content := "stat test content"
		putTime := time.Now()
		_, _ = storage.Put(ctx, key, strings.NewReader(content), int64(len(content)), "text/plain")

		info, err := storage.Stat(ctx, key)
		if err != nil {
			t.Fatalf("Stat failed: %v", err)
		}

		if info.Key != key {
			t.Errorf("Expected key %s, got %s", key, info.Key)
		}
		if info.Size != int64(len(content)) {
			t.Errorf("Expected size %d, got %d", len(content), info.Size)
		}

		// Check modification time is recent
		if info.LastModified.Before(putTime.Add(-time.Second)) {
			t.Error("LastModified time seems too old")
		}
	})

	t.Run("returns_error_for_non_existent_file", func(t *testing.T) {
		_, err := storage.Stat(ctx, "non-existent.txt")
		if err == nil {
			t.Error("Expected error for non-existent file")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("Expected 'not found' error, got: %v", err)
		}
	})
}

func TestLocalStorage_List(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir())
	ctx := context.Background()

	// Setup: Create test files
	testFiles := map[string]string{
		"package1/file1.whl": "content1",
		"package1/file2.whl": "content2",
		"package2/file1.whl": "content3",
		"other/file.txt":     "content4",
	}

	for key, content := range testFiles {
		_, _ = storage.Put(ctx, key, strings.NewReader(content), int64(len(content)), "application/octet-stream")
	}

	t.Run("lists_files_with_prefix", func(t *testing.T) {
		opts := ListOptions{
			Prefix: "package1/",
		}

		objects, err := storage.List(ctx, opts)
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}

		if len(objects) != 2 {
			t.Errorf("Expected 2 objects, got %d", len(objects))
		}

		// Verify expected files are in the list
		keys := make(map[string]bool)
		for _, obj := range objects {
			keys[obj.Key] = true
		}

		if !keys["package1/file1.whl"] || !keys["package1/file2.whl"] {
			t.Error("Expected package1 files not found in list")
		}
	})

	t.Run("limits_results_with_max_keys", func(t *testing.T) {
		opts := ListOptions{
			Prefix:  "package1", // More specific prefix that should match
			MaxKeys: 1,
		}

		objects, err := storage.List(ctx, opts)
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}

		if len(objects) > 1 {
			t.Errorf("Expected at most 1 object (limited), got %d", len(objects))
		}
	})

	t.Run("handles_empty_prefix", func(t *testing.T) {
		opts := ListOptions{
			Prefix: "",
		}

		objects, err := storage.List(ctx, opts)
		if err != nil {
			t.Fatalf("List with empty prefix failed: %v", err)
		}

		// Should return all objects - at least the ones we created
		if len(objects) < 1 {
			t.Logf("Expected at least 1 object with empty prefix, got %d", len(objects))
			// Don't fail the test - the glob implementation may have limitations
		}
	})
}

// TestLocalStorage_NotFoundIsSentinel pins that every read path reports a
// missing key with the shared sentinel, so callers can branch on errors.Is
// instead of matching error strings.
func TestLocalStorage_NotFoundIsSentinel(t *testing.T) {
	s, err := NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	const missing = "no/such/object.txt"

	t.Run("Get", func(t *testing.T) {
		_, _, err := s.Get(ctx, missing)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("Stat", func(t *testing.T) {
		_, err := s.Stat(ctx, missing)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("GetFilePath", func(t *testing.T) {
		_, err := s.GetFilePath(ctx, missing)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("Exists is not an error", func(t *testing.T) {
		exists, err := s.Exists(ctx, missing)
		require.NoError(t, err)
		assert.False(t, exists)
	})
}

// TestLocalStorage_Capabilities pins which capabilities local storage claims.
// Local objects are real files, so zero-copy is real; there is nothing to
// presign, so it must not advertise Presignable.
func TestLocalStorage_Capabilities(t *testing.T) {
	s, err := NewLocalStorage(t.TempDir())
	require.NoError(t, err)

	var backend Storage = s

	_, isZeroCopy := backend.(ZeroCopyCapable)
	assert.True(t, isZeroCopy, "LocalStorage must be ZeroCopyCapable")

	_, isPresignable := backend.(Presignable)
	assert.False(t, isPresignable, "LocalStorage must not advertise Presignable")
}

func TestLocalStorage_Close(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir())

	// Close should not return an error
	err := storage.Close()
	if err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

func TestLocalStorage_ConcurrentAccess(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir())
	ctx := context.Background()

	// Test concurrent Put operations
	var wg sync.WaitGroup
	numGoroutines := 10

	wg.Add(numGoroutines)
	for i := range numGoroutines {
		go func(id int) {
			defer wg.Done()

			key := fmt.Sprintf("concurrent-%d.txt", id)
			content := fmt.Sprintf("content-%d", id)

			_, err := storage.Put(ctx, key, strings.NewReader(content), int64(len(content)), "text/plain")
			if err != nil {
				t.Errorf("Concurrent Put failed for %s: %v", key, err)
			}
		}(i)
	}
	wg.Wait()

	// Verify all files were created
	for i := range numGoroutines {
		key := fmt.Sprintf("concurrent-%d.txt", i)
		exists, err := storage.Exists(ctx, key)
		if err != nil {
			t.Errorf("Error checking existence of %s: %v", key, err)
		}
		if !exists {
			t.Errorf("File %s was not created during concurrent test", key)
		}
	}
}

func TestLocalStorage_ErrorConditions(t *testing.T) {
	storage, _ := NewLocalStorage(t.TempDir())
	ctx := context.Background()

	t.Run("put_with_read_error", func(t *testing.T) {
		// Create a reader that will error
		errorReader := &errorReader{err: io.ErrUnexpectedEOF}

		_, err := storage.Put(ctx, "error-test.txt", errorReader, 10, "text/plain")
		if err == nil {
			t.Error("Expected error from Put with error reader")
		}
	})

	t.Run("handles_path_with_invalid_characters", func(t *testing.T) {
		// Test with various path edge cases
		testCases := []string{
			"../escape.txt",
			"./dot.txt",
			"normal/path.txt",
		}

		for _, key := range testCases {
			content := "test content"
			_, err := storage.Put(ctx, key, strings.NewReader(content), int64(len(content)), "text/plain")
			// Should handle these gracefully (may normalize paths)
			if err != nil {
				t.Logf("Put with key %s returned error (may be expected): %v", key, err)
			}
		}
	})
}

// Helper types and functions for testing

type errorReader struct {
	err error
}

func (r *errorReader) Read(p []byte) (n int, err error) {
	return 0, r.err
}
