package cache

import (
	"container/list"
	"path/filepath"
	"sync"
)

type FileEntry struct {
	Path string
	Size int64
}

type FileCache struct {
	mu       sync.RWMutex
	cacheDir string
	maxSize  int64
	curSize  int64
	entries  map[string]*list.Element
	lru      *list.List
}

func NewFileCache(cacheDir string, maxSize int64) *FileCache {
	return &FileCache{
		cacheDir: cacheDir,
		maxSize:  maxSize,
		entries:  make(map[string]*list.Element),
		lru:      list.New(),
	}
}

func (c *FileCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, exists := c.entries[key]
	if !exists {
		return "", false
	}

	// Move to front (most recently used)
	c.lru.MoveToFront(elem)
	entry := elem.Value.(*FileEntry)

	return entry.Path, true
}

func (c *FileCache) Set(key string, path string, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Cannot cache anything if max size is 0
	if c.maxSize == 0 {
		return
	}

	// Check if already exists
	if elem, exists := c.entries[key]; exists {
		c.lru.MoveToFront(elem)
		return
	}

	// Evict if necessary
	for c.curSize+size > c.maxSize && c.lru.Len() > 0 {
		c.evict()
	}

	// Don't add if still too big after eviction
	if c.curSize+size > c.maxSize {
		return
	}

	// Add new entry
	entry := &FileEntry{
		Path: path,
		Size: size,
	}
	elem := c.lru.PushFront(entry)
	c.entries[key] = elem
	c.curSize += size
}

func (c *FileCache) evict() {
	elem := c.lru.Back()
	if elem == nil {
		return
	}

	c.lru.Remove(elem)
	entry := elem.Value.(*FileEntry)

	// Find and remove from map
	for key, e := range c.entries {
		if e == elem {
			delete(c.entries, key)
			break
		}
	}

	c.curSize -= entry.Size
}

func (c *FileCache) GetCachePath(packageName, fileName string) string {
	return filepath.Join(c.cacheDir, "groxpi-cache", packageName, fileName)
}
