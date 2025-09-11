package cache

import (
	"sync"
	"time"
)

type IndexEntry struct {
	Data      interface{}
	ExpiresAt time.Time
}

type IndexCache struct {
	mu      sync.RWMutex
	entries map[string]*IndexEntry
}

func NewIndexCache() *IndexCache {
	return &IndexCache{
		entries: make(map[string]*IndexEntry),
	}
}

func (c *IndexCache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	entry, exists := c.entries[key]
	if !exists {
		return nil, false
	}
	
	if time.Now().After(entry.ExpiresAt) {
		return nil, false
	}
	
	return entry.Data, true
}

func (c *IndexCache) Set(key string, data interface{}, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	c.entries[key] = &IndexEntry{
		Data:      data,
		ExpiresAt: time.Now().Add(ttl),
	}
}

func (c *IndexCache) InvalidateList() {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	delete(c.entries, "package-list")
}

func (c *IndexCache) InvalidatePackage(packageName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	delete(c.entries, "package:"+packageName)
}

func (c *IndexCache) GetPackage(packageName string) (interface{}, bool) {
	return c.Get("package:" + packageName)
}

func (c *IndexCache) SetPackage(packageName string, data interface{}, ttl time.Duration) {
	c.Set("package:"+packageName, data, ttl)
}