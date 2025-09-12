package cache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewResponseCache(t *testing.T) {
	maxSize := 1024 * 1024 // 1MB
	cache := NewResponseCache(maxSize)

	if cache == nil {
		t.Fatal("NewResponseCache() returned nil")
	}

	if cache.maxSize != maxSize {
		t.Errorf("Expected maxSize to be %d, got %d", maxSize, cache.maxSize)
	}

	if cache.entries == nil {
		t.Error("Cache entries map was not initialized")
	}

	if cache.lru == nil {
		t.Error("LRU slice was not initialized")
	}

	// Test with zero maxSize
	zeroCache := NewResponseCache(0)
	if zeroCache.maxSize != 0 {
		t.Errorf("Expected zero maxSize to be preserved, got %d", zeroCache.maxSize)
	}

	// Test with negative maxSize (should work but not be very useful)
	negativeCache := NewResponseCache(-1)
	if negativeCache.maxSize != -1 {
		t.Errorf("Expected negative maxSize to be preserved, got %d", negativeCache.maxSize)
	}
}

func TestResponseCache_SetAndGet(t *testing.T) {
	cache := NewResponseCache(1024 * 1024) // 1MB

	t.Run("set and get valid entry", func(t *testing.T) {
		key := "test-key"
		data := []byte(`{"test": "data"}`)
		ttl := 5 * time.Second

		cache.Set(key, data, ttl)

		result, exists := cache.Get(key)
		if !exists {
			t.Error("Expected entry to exist")
		}

		if string(result) != string(data) {
			t.Errorf("Expected '%s', got '%s'", string(data), string(result))
		}
	})

	t.Run("get non-existent entry", func(t *testing.T) {
		_, exists := cache.Get("non-existent-key")
		if exists {
			t.Error("Expected entry to not exist")
		}
	})

	t.Run("get expired entry", func(t *testing.T) {
		key := "expired-key"
		data := []byte(`{"expired": "data"}`)
		ttl := 10 * time.Millisecond

		cache.Set(key, data, ttl)

		// Wait for expiration
		time.Sleep(20 * time.Millisecond)

		_, exists := cache.Get(key)
		if exists {
			t.Error("Expected expired entry to not exist")
		}

		// Verify entry was removed from cache
		cache.mu.RLock()
		_, stillInMap := cache.entries[key]
		cache.mu.RUnlock()

		if stillInMap {
			t.Error("Expected expired entry to be removed from entries map")
		}
	})

	t.Run("overwrite existing entry", func(t *testing.T) {
		key := "overwrite-key"
		data1 := []byte(`{"first": "data"}`)
		data2 := []byte(`{"second": "data"}`)
		ttl := 5 * time.Second

		cache.Set(key, data1, ttl)
		cache.Set(key, data2, ttl)

		result, exists := cache.Get(key)
		if !exists {
			t.Error("Expected entry to exist")
		}

		if string(result) != string(data2) {
			t.Errorf("Expected '%s', got '%s'", string(data2), string(result))
		}
	})
}

func TestResponseCache_GetZeroCopy(t *testing.T) {
	cache := NewResponseCache(1024 * 1024) // 1MB

	t.Run("get zero-copy valid entry", func(t *testing.T) {
		key := "zero-copy-key"
		data := []byte(`{"zero": "copy"}`)
		ttl := 5 * time.Second

		cache.Set(key, data, ttl)

		result, release, exists := cache.GetZeroCopy(key)
		if !exists {
			t.Error("Expected entry to exist")
		}

		if release == nil {
			t.Error("Expected release function to be provided")
		}

		if string(result) != string(data) {
			t.Errorf("Expected '%s', got '%s'", string(data), string(result))
		}

		// Verify reference count was incremented
		cache.mu.RLock()
		entry := cache.entries[key]
		refCount := atomic.LoadInt64(&entry.RefCount)
		cache.mu.RUnlock()

		if refCount != 1 {
			t.Errorf("Expected reference count to be 1, got %d", refCount)
		}

		// Call release function
		release()

		// Verify reference count was decremented
		cache.mu.RLock()
		refCountAfter := atomic.LoadInt64(&entry.RefCount)
		cache.mu.RUnlock()

		if refCountAfter != 0 {
			t.Errorf("Expected reference count to be 0 after release, got %d", refCountAfter)
		}
	})

	t.Run("get zero-copy non-existent entry", func(t *testing.T) {
		result, release, exists := cache.GetZeroCopy("non-existent-key")
		if exists {
			t.Error("Expected entry to not exist")
		}

		if result != nil {
			t.Error("Expected result to be nil")
		}

		if release != nil {
			t.Error("Expected release function to be nil")
		}
	})

	t.Run("get zero-copy expired entry", func(t *testing.T) {
		key := "zero-copy-expired-key"
		data := []byte(`{"zero": "copy", "expired": true}`)
		ttl := 10 * time.Millisecond

		cache.Set(key, data, ttl)

		// Wait for expiration
		time.Sleep(20 * time.Millisecond)

		result, release, exists := cache.GetZeroCopy(key)
		if exists {
			t.Error("Expected expired entry to not exist")
		}

		if result != nil {
			t.Error("Expected result to be nil for expired entry")
		}

		if release != nil {
			t.Error("Expected release function to be nil for expired entry")
		}
	})
}

func TestResponseCache_LRUEviction(t *testing.T) {
	maxSize := 1000 // 1000 bytes max
	cache := NewResponseCache(maxSize)

	// Add entries that will exceed max size
	entries := []struct {
		key  string
		data []byte
	}{
		{"key1", []byte(fmt.Sprintf("%0400s", "a"))}, // 400 bytes
		{"key2", []byte(fmt.Sprintf("%0400s", "b"))}, // 400 bytes
		{"key3", []byte(fmt.Sprintf("%0400s", "c"))}, // 400 bytes - should trigger eviction
	}

	ttl := 5 * time.Second

	// Add first two entries
	cache.Set(entries[0].key, entries[0].data, ttl)
	cache.Set(entries[1].key, entries[1].data, ttl)

	// Verify both exist
	_, exists1 := cache.Get(entries[0].key)
	_, exists2 := cache.Get(entries[1].key)
	if !exists1 || !exists2 {
		t.Error("Expected both initial entries to exist")
	}

	// Add third entry, which should evict the first
	cache.Set(entries[2].key, entries[2].data, ttl)

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
}

func TestResponseCache_AccessUpdatesLRU(t *testing.T) {
	maxSize := 1000 // 1000 bytes max
	cache := NewResponseCache(maxSize)

	// Add two entries
	data1 := []byte(fmt.Sprintf("%0400s", "data1"))
	data2 := []byte(fmt.Sprintf("%0400s", "data2"))
	data3 := []byte(fmt.Sprintf("%0400s", "data3"))
	ttl := 5 * time.Second

	cache.Set("key1", data1, ttl)
	cache.Set("key2", data2, ttl)

	// Access first entry to move it to front of LRU
	cache.Get("key1")

	// Add third entry that requires eviction
	cache.Set("key3", data3, ttl)

	// First entry should still exist (was accessed recently)
	_, exists1 := cache.Get("key1")
	if !exists1 {
		t.Error("Expected recently accessed entry to not be evicted")
	}

	// Second entry should be evicted (was least recently used)
	_, exists2 := cache.Get("key2")
	if exists2 {
		t.Error("Expected least recently used entry to be evicted")
	}

	// Third entry should exist
	_, exists3 := cache.Get("key3")
	if !exists3 {
		t.Error("Expected newly added entry to exist")
	}
}

func TestResponseCache_Invalidate(t *testing.T) {
	cache := NewResponseCache(1024 * 1024) // 1MB

	key := "invalidate-key"
	data := []byte(`{"invalidate": "test"}`)
	ttl := 5 * time.Second

	// Set entry
	cache.Set(key, data, ttl)

	// Verify it exists
	_, exists := cache.Get(key)
	if !exists {
		t.Error("Expected entry to exist before invalidation")
	}

	// Invalidate
	cache.Invalidate(key)

	// Verify it's gone
	_, exists = cache.Get(key)
	if exists {
		t.Error("Expected entry to be invalidated")
	}

	// Verify it's also removed from LRU
	cache.mu.RLock()
	lruContainsKey := false
	for _, k := range cache.lru {
		if k == key {
			lruContainsKey = true
			break
		}
	}
	cache.mu.RUnlock()

	if lruContainsKey {
		t.Error("Expected key to be removed from LRU")
	}
}

func TestResponseCache_ConcurrentAccess(t *testing.T) {
	cache := NewResponseCache(10 * 1024 * 1024) // 10MB

	const numGoroutines = 100
	const numOperations = 10

	var wg sync.WaitGroup

	// Test concurrent reads and writes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for j := 0; j < numOperations; j++ {
				key := fmt.Sprintf("concurrent-key-%d-%d", id, j)
				data := []byte(fmt.Sprintf(`{"id": %d, "operation": %d}`, id, j))

				// Write
				cache.Set(key, data, 1*time.Second)

				// Read
				result, exists := cache.Get(key)
				if !exists {
					t.Errorf("Expected key %s to exist", key)
					return
				}

				if string(result) != string(data) {
					t.Errorf("Expected '%s', got '%s'", string(data), string(result))
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestResponseCache_ConcurrentZeroCopyAccess(t *testing.T) {
	cache := NewResponseCache(10 * 1024 * 1024) // 10MB

	key := "zero-copy-concurrent"
	data := []byte(`{"concurrent": "zero-copy"}`)
	ttl := 5 * time.Second

	cache.Set(key, data, ttl)

	const numGoroutines = 50
	var wg sync.WaitGroup
	var successCount int64

	// Test concurrent zero-copy access
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			result, release, exists := cache.GetZeroCopy(key)
			if !exists {
				t.Error("Expected entry to exist")
				return
			}

			if string(result) != string(data) {
				t.Errorf("Expected '%s', got '%s'", string(data), string(result))
				return
			}

			// Hold reference for a short time
			time.Sleep(1 * time.Millisecond)

			// Release reference
			release()

			atomic.AddInt64(&successCount, 1)
		}()
	}

	wg.Wait()

	if atomic.LoadInt64(&successCount) != numGoroutines {
		t.Errorf("Expected %d successful operations, got %d", numGoroutines, atomic.LoadInt64(&successCount))
	}

	// Verify final reference count is 0
	cache.mu.RLock()
	entry := cache.entries[key]
	finalRefCount := atomic.LoadInt64(&entry.RefCount)
	cache.mu.RUnlock()

	if finalRefCount != 0 {
		t.Errorf("Expected final reference count to be 0, got %d", finalRefCount)
	}
}

func TestResponseCache_EdgeCases(t *testing.T) {
	cache := NewResponseCache(100) // Very small cache

	t.Run("empty data", func(t *testing.T) {
		key := "empty-data"
		data := []byte("")
		ttl := 5 * time.Second

		cache.Set(key, data, ttl)

		result, exists := cache.Get(key)
		if !exists {
			t.Error("Expected empty data entry to exist")
		}

		if len(result) != 0 {
			t.Errorf("Expected empty result, got %d bytes", len(result))
		}
	})

	t.Run("nil data", func(t *testing.T) {
		key := "nil-data"
		var data []byte = nil
		ttl := 5 * time.Second

		cache.Set(key, data, ttl)

		result, exists := cache.Get(key)
		if !exists {
			t.Error("Expected nil data entry to exist")
		}

		if result != nil {
			t.Errorf("Expected nil result, got %v", result)
		}
	})

	t.Run("zero TTL", func(t *testing.T) {
		key := "zero-ttl"
		data := []byte(`{"ttl": 0}`)

		cache.Set(key, data, 0)

		// Should be expired immediately
		_, exists := cache.Get(key)
		if exists {
			t.Error("Expected entry with zero TTL to be expired immediately")
		}
	})

	t.Run("negative TTL", func(t *testing.T) {
		key := "negative-ttl"
		data := []byte(`{"ttl": -1}`)

		cache.Set(key, data, -1*time.Second)

		// Should be expired immediately
		_, exists := cache.Get(key)
		if exists {
			t.Error("Expected entry with negative TTL to be expired immediately")
		}
	})

	t.Run("data larger than cache", func(t *testing.T) {
		key := "too-large"
		data := []byte(fmt.Sprintf("%0200s", "large"))
		ttl := 5 * time.Second

		cache.Set(key, data, ttl)

		// Current implementation allows storing items larger than max cache
		// It will just evict everything else and store this large item
		_, exists := cache.Get(key)
		if !exists {
			t.Error("Expected large entry to be stored (implementation allows this)")
		}

		// Cache should have evicted everything else
		cache.mu.RLock()
		entryCount := len(cache.entries)
		cache.mu.RUnlock()

		if entryCount != 1 {
			t.Errorf("Expected only 1 entry after storing large item, got %d", entryCount)
		}
	})
}

func TestResponseCache_LRUUpdateLogic(t *testing.T) {
	cache := NewResponseCache(1024 * 1024) // 1MB

	// Add several entries
	keys := []string{"key1", "key2", "key3", "key4"}
	for i, key := range keys {
		data := []byte(fmt.Sprintf(`{"index": %d}`, i))
		cache.Set(key, data, 5*time.Second)
	}

	// Access key2 to move it to the end of LRU
	cache.Get("key2")

	// Verify LRU order
	cache.mu.RLock()
	lruOrder := make([]string, len(cache.lru))
	copy(lruOrder, cache.lru)
	cache.mu.RUnlock()

	// key2 should be at the end (most recently used)
	if lruOrder[len(lruOrder)-1] != "key2" {
		t.Errorf("Expected key2 to be most recent in LRU, got order: %v", lruOrder)
	}

	// Access key1 to move it to the end
	cache.Get("key1")

	cache.mu.RLock()
	newLruOrder := make([]string, len(cache.lru))
	copy(newLruOrder, cache.lru)
	cache.mu.RUnlock()

	// key1 should now be at the end
	if newLruOrder[len(newLruOrder)-1] != "key1" {
		t.Errorf("Expected key1 to be most recent in LRU, got order: %v", newLruOrder)
	}
}

// Benchmark tests for ResponseCache
func BenchmarkResponseCache_Set(b *testing.B) {
	cache := NewResponseCache(100 * 1024 * 1024) // 100MB
	data := []byte(`{"benchmark": "set"}`)
	ttl := 1 * time.Hour

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-set-%d", i)
		cache.Set(key, data, ttl)
	}
}

func BenchmarkResponseCache_Get(b *testing.B) {
	cache := NewResponseCache(100 * 1024 * 1024) // 100MB
	data := []byte(`{"benchmark": "get"}`)
	ttl := 1 * time.Hour

	// Pre-populate cache
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("bench-get-%d", i)
		cache.Set(key, data, ttl)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-get-%d", i%1000)
		cache.Get(key)
	}
}

func BenchmarkResponseCache_GetZeroCopy(b *testing.B) {
	cache := NewResponseCache(100 * 1024 * 1024) // 100MB
	data := []byte(`{"benchmark": "zero-copy"}`)
	ttl := 1 * time.Hour

	// Pre-populate cache
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("bench-zero-copy-%d", i)
		cache.Set(key, data, ttl)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-zero-copy-%d", i%1000)
		_, release, exists := cache.GetZeroCopy(key)
		if exists && release != nil {
			release()
		}
	}
}

func BenchmarkResponseCache_ConcurrentAccess(b *testing.B) {
	cache := NewResponseCache(100 * 1024 * 1024) // 100MB
	data := []byte(`{"benchmark": "concurrent"}`)
	ttl := 1 * time.Hour

	// Pre-populate cache
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("bench-concurrent-%d", i)
		cache.Set(key, data, ttl)
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			key := fmt.Sprintf("bench-concurrent-%d", b.N%100)
			cache.Get(key)
		}
	})
}
