package cache

import (
	"sync"
	"time"
)

// ResponseCache stores pre-marshaled JSON responses
type ResponseCache struct {
	mu      sync.RWMutex
	entries map[string]*ResponseEntry
	maxSize int
	lru     []string // Simple LRU tracking
}

type ResponseEntry struct {
	Data      []byte    // Pre-marshaled JSON
	ExpiresAt time.Time
	Size      int
}

func NewResponseCache(maxSize int) *ResponseCache {
	return &ResponseCache{
		entries: make(map[string]*ResponseEntry),
		maxSize: maxSize,
		lru:     make([]string, 0, 1000),
	}
}

func (c *ResponseCache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	entry, exists := c.entries[key]
	c.mu.RUnlock()
	
	if !exists {
		return nil, false
	}
	
	if time.Now().After(entry.ExpiresAt) {
		// Expired, remove it
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return nil, false
	}
	
	// Update LRU
	c.updateLRU(key)
	
	return entry.Data, true
}

func (c *ResponseCache) Set(key string, data []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	// Check if we need to evict entries
	currentSize := 0
	for _, entry := range c.entries {
		currentSize += entry.Size
	}
	
	newSize := len(data)
	
	// Evict old entries if needed
	for currentSize+newSize > c.maxSize && len(c.lru) > 0 {
		// Remove oldest entry
		oldKey := c.lru[0]
		c.lru = c.lru[1:]
		if oldEntry, exists := c.entries[oldKey]; exists {
			currentSize -= oldEntry.Size
			delete(c.entries, oldKey)
		}
	}
	
	c.entries[key] = &ResponseEntry{
		Data:      data,
		ExpiresAt: time.Now().Add(ttl),
		Size:      newSize,
	}
	
	// Add to LRU
	c.lru = append(c.lru, key)
}

func (c *ResponseCache) updateLRU(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	// Find and remove from current position
	for i, k := range c.lru {
		if k == key {
			c.lru = append(c.lru[:i], c.lru[i+1:]...)
			break
		}
	}
	
	// Add to end (most recently used)
	c.lru = append(c.lru, key)
}

func (c *ResponseCache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	delete(c.entries, key)
	
	// Remove from LRU
	for i, k := range c.lru {
		if k == key {
			c.lru = append(c.lru[:i], c.lru[i+1:]...)
			break
		}
	}
}