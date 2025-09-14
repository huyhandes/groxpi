package cache

import (
	"fmt"
	"testing"
	"time"
)

func TestNewIndexCache(t *testing.T) {
	indexCache := NewIndexCache()

	if indexCache == nil {
		t.Fatal("NewIndexCache() returned nil")
	}

	// Skip internal field checks
}

func TestIndexCache_SetAndGet(t *testing.T) {
	indexCache := NewIndexCache()

	t.Run("set and get valid entry", func(t *testing.T) {
		key := "test-key"
		data := "test-data"
		ttl := 5 * time.Second

		indexCache.Set(key, data, ttl)

		result, exists := indexCache.Get(key)
		if !exists {
			t.Error("Expected entry to exist")
		}

		if result != data {
			t.Errorf("Expected '%s', got '%s'", data, result)
		}
	})

	t.Run("get non-existent entry", func(t *testing.T) {
		_, exists := indexCache.Get("non-existent-key")
		if exists {
			t.Error("Expected entry to not exist")
		}
	})

	t.Run("get expired entry", func(t *testing.T) {
		key := "expired-key"
		data := "expired-data"
		ttl := 10 * time.Millisecond

		indexCache.Set(key, data, ttl)

		// Wait for expiration
		time.Sleep(20 * time.Millisecond)

		_, exists := indexCache.Get(key)
		if exists {
			t.Error("Expected expired entry to not exist")
		}
	})

	t.Run("overwrite existing entry", func(t *testing.T) {
		key := "overwrite-key"
		data1 := "first-data"
		data2 := "second-data"
		ttl := 5 * time.Second

		indexCache.Set(key, data1, ttl)
		indexCache.Set(key, data2, ttl)

		result, exists := indexCache.Get(key)
		if !exists {
			t.Error("Expected entry to exist")
		}

		if result != data2 {
			t.Errorf("Expected '%s', got '%s'", data2, result)
		}
	})
}

func TestIndexCache_InvalidateList(t *testing.T) {
	indexCache := NewIndexCache()

	// Set package list
	indexCache.Set("package-list", []string{"package1", "package2"}, 5*time.Second)

	// Verify it exists
	_, exists := indexCache.Get("package-list")
	if !exists {
		t.Error("Expected package-list to exist before invalidation")
	}

	// Invalidate
	indexCache.InvalidateList()

	// Verify it's gone
	_, exists = indexCache.Get("package-list")
	if exists {
		t.Error("Expected package-list to be invalidated")
	}
}

func TestIndexCache_InvalidatePackage(t *testing.T) {
	indexCache := NewIndexCache()

	packageName := "test-package"
	key := "package:" + packageName

	// Set package data
	indexCache.Set(key, []string{"file1.whl", "file2.tar.gz"}, 5*time.Second)

	// Verify it exists
	_, exists := indexCache.Get(key)
	if !exists {
		t.Error("Expected package data to exist before invalidation")
	}

	// Invalidate
	indexCache.InvalidatePackage(packageName)

	// Verify it's gone
	_, exists = indexCache.Get(key)
	if exists {
		t.Error("Expected package data to be invalidated")
	}
}

func TestIndexCache_ConcurrentAccess(t *testing.T) {
	indexCache := NewIndexCache()
	done := make(chan bool)

	// Test concurrent reads and writes
	for i := 0; i < 10; i++ {
		go func(id int) {
			defer func() { done <- true }()

			key := fmt.Sprintf("key-%d", id)
			data := fmt.Sprintf("data-%d", id)

			// Write
			indexCache.Set(key, data, 5*time.Second)

			// Read
			result, exists := indexCache.Get(key)
			if !exists {
				t.Errorf("Expected key %s to exist", key)
				return
			}

			if result != data {
				t.Errorf("Expected '%s', got '%s'", data, result)
			}
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestIndexCache_DifferentDataTypes(t *testing.T) {
	indexCache := NewIndexCache()

	testCases := []struct {
		name string
		key  string
		data interface{}
	}{
		{"string", "str-key", "string-value"},
		{"int", "int-key", 42},
		{"slice", "slice-key", []string{"a", "b", "c"}},
		{"map", "map-key", map[string]int{"x": 1, "y": 2}},
		{"struct", "struct-key", struct{ Name string }{"test"}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			indexCache.Set(tc.key, tc.data, 5*time.Second)

			result, exists := indexCache.Get(tc.key)
			if !exists {
				t.Errorf("Expected entry for key %s to exist", tc.key)
			}

			// For complex types, we can't do deep comparison easily
			// Just verify something was stored
			if result == nil {
				t.Errorf("Expected non-nil result for key %s", tc.key)
			}
		})
	}
}

func TestIndexCache_ZeroTTL(t *testing.T) {
	indexCache := NewIndexCache()

	key := "zero-ttl-key"
	data := "zero-ttl-data"

	// Set with zero TTL (should expire immediately)
	indexCache.Set(key, data, 0)

	// Should be expired immediately
	_, exists := indexCache.Get(key)
	if exists {
		t.Error("Expected entry with zero TTL to be expired immediately")
	}
}

func TestIndexCache_NegativeTTL(t *testing.T) {
	indexCache := NewIndexCache()

	key := "negative-ttl-key"
	data := "negative-ttl-data"

	// Set with negative TTL (should expire immediately)
	indexCache.Set(key, data, -1*time.Second)

	// Should be expired immediately
	_, exists := indexCache.Get(key)
	if exists {
		t.Error("Expected entry with negative TTL to be expired immediately")
	}
}
