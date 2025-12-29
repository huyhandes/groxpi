package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

	lru := NewLRUCache(baseDir, maxSize, 0)
	defer func() { _ = lru.Close() }()

	t.Run("records access", func(t *testing.T) {
		err := lru.RecordAccess("test-key-1", 512)
		if err != nil {
			t.Fatalf("Failed to record access: %v", err)
		}

		stats := lru.GetStats()
		if stats["entry_count"].(int) != 1 {
			t.Errorf("Expected 1 entry, got %d", stats["entry_count"].(int))
		}
		if stats["current_size_bytes"].(int64) != 512 {
			t.Errorf("Expected 512 bytes, got %d", stats["current_size_bytes"].(int64))
		}
	})

	t.Run("triggers eviction when over size", func(t *testing.T) {
		// Add entries that exceed max size
		_ = lru.RecordAccess("test-key-2", 400)
		_ = lru.RecordAccess("test-key-3", 400)

		// Wait a bit for eviction worker to run
		time.Sleep(100 * time.Millisecond)

		stats := lru.GetStats()
		currentSize := stats["current_size_bytes"].(int64)

		// Should have evicted to get under maxSize
		if currentSize > maxSize {
			t.Errorf("Expected size <= %d after eviction, got %d", maxSize, currentSize)
		}
	})

	t.Run("deletes entry", func(t *testing.T) {
		initialStats := lru.GetStats()
		initialCount := initialStats["entry_count"].(int)

		// Add a new entry
		_ = lru.RecordAccess("test-key-delete", 100)

		// Delete it
		err := lru.RecordDelete("test-key-delete")
		if err != nil {
			t.Fatalf("Failed to delete entry: %v", err)
		}

		finalStats := lru.GetStats()
		finalCount := finalStats["entry_count"].(int)

		if finalCount != initialCount {
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

		// Check LRU stats
		stats := storage.GetStats()
		if stats["entry_count"].(int) < 1 {
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
		stats := newStorage.GetStats()
		if stats["entry_count"].(int) < 1 {
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
	for i := 0; i < 5; i++ {
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
	for i := 0; i < 5; i++ {
		<-done
	}
}
