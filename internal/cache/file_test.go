package cache

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestNewFileCache(t *testing.T) {
	cacheDir := "/tmp/test-cache"
	maxSize := int64(1024 * 1024) // 1MB
	
	cache := NewFileCache(cacheDir, maxSize)
	
	if cache == nil {
		t.Fatal("NewFileCache() returned nil")
	}
	
	if cache.cacheDir != cacheDir {
		t.Errorf("Expected cacheDir to be %s, got %s", cacheDir, cache.cacheDir)
	}
	
	if cache.maxSize != maxSize {
		t.Errorf("Expected maxSize to be %d, got %d", maxSize, cache.maxSize)
	}
	
	if cache.entries == nil {
		t.Error("Cache entries map was not initialized")
	}
	
	if cache.lru == nil {
		t.Error("LRU list was not initialized")
	}
}

func TestFileCache_SetAndGet(t *testing.T) {
	cacheDir := "/tmp/test-cache"
	maxSize := int64(1024 * 1024) // 1MB
	cache := NewFileCache(cacheDir, maxSize)
	
	t.Run("set and get new entry", func(t *testing.T) {
		key := "test-package/test-file.whl"
		path := "/path/to/test-file.whl"
		size := int64(1024)
		
		cache.Set(key, path, size)
		
		result, exists := cache.Get(key)
		if !exists {
			t.Error("Expected entry to exist")
		}
		
		if result != path {
			t.Errorf("Expected path '%s', got '%s'", path, result)
		}
		
		if cache.curSize != size {
			t.Errorf("Expected current size to be %d, got %d", size, cache.curSize)
		}
	})
	
	t.Run("get non-existent entry", func(t *testing.T) {
		_, exists := cache.Get("non-existent-key")
		if exists {
			t.Error("Expected entry to not exist")
		}
	})
	
	t.Run("set existing entry moves to front", func(t *testing.T) {
		key := "existing-package/existing-file.whl"
		path := "/path/to/existing-file.whl"
		size := int64(512)
		
		// Set entry twice
		cache.Set(key, path, size)
		initialSize := cache.curSize
		cache.Set(key, path, size)
		
		// Size should not change when setting existing entry
		if cache.curSize != initialSize {
			t.Errorf("Expected size to remain %d, got %d", initialSize, cache.curSize)
		}
		
		// Entry should still exist
		result, exists := cache.Get(key)
		if !exists {
			t.Error("Expected existing entry to still exist")
		}
		
		if result != path {
			t.Errorf("Expected path '%s', got '%s'", path, result)
		}
	})
}

func TestFileCache_LRUEviction(t *testing.T) {
	cacheDir := "/tmp/test-cache"
	maxSize := int64(1024) // 1KB max
	cache := NewFileCache(cacheDir, maxSize)
	
	// Add entries that exceed max size
	entries := []struct {
		key  string
		path string
		size int64
	}{
		{"pkg1/file1.whl", "/path/to/file1.whl", 400},
		{"pkg2/file2.whl", "/path/to/file2.whl", 400},
		{"pkg3/file3.whl", "/path/to/file3.whl", 400}, // This should trigger eviction
	}
	
	// Add first two entries
	cache.Set(entries[0].key, entries[0].path, entries[0].size)
	cache.Set(entries[1].key, entries[1].path, entries[1].size)
	
	// Verify both exist
	_, exists1 := cache.Get(entries[0].key)
	_, exists2 := cache.Get(entries[1].key)
	if !exists1 || !exists2 {
		t.Error("Expected both initial entries to exist")
	}
	
	// Add third entry, which should evict the first
	cache.Set(entries[2].key, entries[2].path, entries[2].size)
	
	// First entry should be evicted
	_, exists1 = cache.Get(entries[0].key)
	if exists1 {
		t.Error("Expected first entry to be evicted")
	}
	
	// Second and third entries should exist
	_, exists2 = cache.Get(entries[1].key)
	_, exists3 := cache.Get(entries[2].key)
	if !exists2 || !exists3 {
		t.Error("Expected second and third entries to exist")
	}
	
	// Cache size should not exceed max
	if cache.curSize > maxSize {
		t.Errorf("Cache size %d exceeds max size %d", cache.curSize, maxSize)
	}
}

func TestFileCache_GetCachePath(t *testing.T) {
	cacheDir := "/tmp/test-cache"
	cache := NewFileCache(cacheDir, 1024)
	
	packageName := "test-package"
	fileName := "test-file.whl"
	
	expectedPath := filepath.Join(cacheDir, "groxpi-cache", packageName, fileName)
	actualPath := cache.GetCachePath(packageName, fileName)
	
	if actualPath != expectedPath {
		t.Errorf("Expected cache path '%s', got '%s'", expectedPath, actualPath)
	}
}

func TestFileCache_EmptyCache(t *testing.T) {
	cacheDir := "/tmp/test-cache"
	cache := NewFileCache(cacheDir, 1024)
	
	if cache.curSize != 0 {
		t.Errorf("Expected empty cache to have size 0, got %d", cache.curSize)
	}
	
	if cache.lru.Len() != 0 {
		t.Errorf("Expected empty cache LRU length to be 0, got %d", cache.lru.Len())
	}
	
	if len(cache.entries) != 0 {
		t.Errorf("Expected empty cache entries length to be 0, got %d", len(cache.entries))
	}
}

func TestFileCache_ZeroMaxSize(t *testing.T) {
	cacheDir := "/tmp/test-cache"
	cache := NewFileCache(cacheDir, 0) // Zero max size
	
	key := "test-package/test-file.whl"
	path := "/path/to/test-file.whl"
	size := int64(1024)
	
	// Should not be able to add anything to zero-size cache
	cache.Set(key, path, size)
	
	_, exists := cache.Get(key)
	if exists {
		t.Error("Expected entry to not exist in zero-size cache")
	}
}

func TestFileCache_MultipleEvictions(t *testing.T) {
	cacheDir := "/tmp/test-cache"
	maxSize := int64(1000)
	cache := NewFileCache(cacheDir, maxSize)
	
	// Add multiple small files
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("pkg%d/file%d.whl", i, i)
		path := fmt.Sprintf("/path/to/file%d.whl", i)
		size := int64(300) // Each file is 300 bytes
		
		cache.Set(key, path, size)
	}
	
	// Should have evicted some entries to stay under limit
	if cache.curSize > maxSize {
		t.Errorf("Cache size %d exceeds max size %d", cache.curSize, maxSize)
	}
	
	// Should have 3 entries (3 * 300 = 900 < 1000)
	expectedEntries := 3
	if len(cache.entries) != expectedEntries {
		t.Errorf("Expected %d entries, got %d", expectedEntries, len(cache.entries))
	}
}

func TestFileCache_AccessUpdatesLRU(t *testing.T) {
	cacheDir := "/tmp/test-cache"
	maxSize := int64(1000)
	cache := NewFileCache(cacheDir, maxSize)
	
	// Add two entries
	cache.Set("pkg1/file1.whl", "/path/file1.whl", 400)
	cache.Set("pkg2/file2.whl", "/path/file2.whl", 400)
	
	// Access first entry to move it to front
	cache.Get("pkg1/file1.whl")
	
	// Add third entry that requires eviction
	cache.Set("pkg3/file3.whl", "/path/file3.whl", 400)
	
	// First entry should still exist (was accessed recently)
	_, exists1 := cache.Get("pkg1/file1.whl")
	if !exists1 {
		t.Error("Expected recently accessed entry to not be evicted")
	}
	
	// Second entry should be evicted (was least recently used)
	_, exists2 := cache.Get("pkg2/file2.whl")
	if exists2 {
		t.Error("Expected least recently used entry to be evicted")
	}
	
	// Third entry should exist
	_, exists3 := cache.Get("pkg3/file3.whl")
	if !exists3 {
		t.Error("Expected newly added entry to exist")
	}
}

func TestFileCache_ConcurrentAccess(t *testing.T) {
	cacheDir := "/tmp/test-cache"
	cache := NewFileCache(cacheDir, 10240) // 10KB
	done := make(chan bool)
	
	// Test concurrent reads and writes
	for i := 0; i < 10; i++ {
		go func(id int) {
			defer func() { done <- true }()
			
			key := fmt.Sprintf("pkg%d/file%d.whl", id, id)
			path := fmt.Sprintf("/path/to/file%d.whl", id)
			size := int64(100)
			
			// Write
			cache.Set(key, path, size)
			
			// Read
			result, exists := cache.Get(key)
			if !exists {
				t.Errorf("Expected key %s to exist", key)
				return
			}
			
			if result != path {
				t.Errorf("Expected '%s', got '%s'", path, result)
			}
		}(i)
	}
	
	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}
}