package storage

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/phuslu/log"
)

// LRUEntry represents an entry in the LRU cache
type LRUEntry struct {
	Key          string
	Size         int64
	LastAccessed time.Time
	FilePath     string
}

// LRUCache implements an LRU eviction policy for local file storage
type LRUCache struct {
	mu           sync.RWMutex
	maxSize      int64                    // Maximum cache size in bytes
	currentSize  int64                    // Current cache size in bytes
	entries      map[string]*list.Element // Key -> list element mapping
	lruList      *list.List               // Doubly-linked list for LRU ordering
	baseDir      string                   // Base directory for cached files
	evictionChan chan struct{}            // Channel to trigger eviction checks
	stopChan     chan struct{}            // Channel to stop background eviction
	wg           sync.WaitGroup
}

// NewLRUCache creates a new LRU cache
func NewLRUCache(baseDir string, maxSize int64) *LRUCache {
	cache := &LRUCache{
		maxSize:      maxSize,
		currentSize:  0,
		entries:      make(map[string]*list.Element),
		lruList:      list.New(),
		baseDir:      baseDir,
		evictionChan: make(chan struct{}, 1),
		stopChan:     make(chan struct{}),
	}

	// Start background eviction goroutine
	cache.wg.Add(1)
	go cache.evictionWorker()

	log.Info().
		Str("base_dir", baseDir).
		Int64("max_size_bytes", maxSize).
		Int64("max_size_mb", maxSize/(1024*1024)).
		Msg("LRU cache initialized")

	return cache
}

// evictionWorker runs in the background and performs evictions when needed
func (lru *LRUCache) evictionWorker() {
	defer lru.wg.Done()

	ticker := time.NewTicker(1 * time.Minute) // Periodic cleanup
	defer ticker.Stop()

	for {
		select {
		case <-lru.stopChan:
			log.Info().Msg("LRU eviction worker stopping")
			return
		case <-lru.evictionChan:
			lru.performEviction()
		case <-ticker.C:
			// Periodic cleanup of stale entries
			lru.cleanupStaleEntries()
		}
	}
}

// performEviction evicts least recently used entries until size is under limit
func (lru *LRUCache) performEviction() {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	if lru.currentSize <= lru.maxSize {
		return
	}

	evictedCount := 0
	evictedSize := int64(0)

	log.Info().
		Int64("current_size_mb", lru.currentSize/(1024*1024)).
		Int64("max_size_mb", lru.maxSize/(1024*1024)).
		Msg("Starting LRU eviction")

	// Evict from the back of the list (least recently used)
	for lru.currentSize > lru.maxSize && lru.lruList.Len() > 0 {
		elem := lru.lruList.Back()
		if elem == nil {
			break
		}

		entry := elem.Value.(*LRUEntry)

		// Delete the file
		if err := os.Remove(entry.FilePath); err != nil {
			if !os.IsNotExist(err) {
				log.Error().
					Err(err).
					Str("key", entry.Key).
					Str("path", entry.FilePath).
					Msg("Failed to delete file during eviction")
			}
		}

		// Remove from tracking
		lru.currentSize -= entry.Size
		evictedSize += entry.Size
		evictedCount++

		delete(lru.entries, entry.Key)
		lru.lruList.Remove(elem)

		log.Debug().
			Str("key", entry.Key).
			Int64("size", entry.Size).
			Msg("Evicted entry from L1 cache")
	}

	log.Info().
		Int("evicted_count", evictedCount).
		Int64("evicted_size_mb", evictedSize/(1024*1024)).
		Int64("new_size_mb", lru.currentSize/(1024*1024)).
		Msg("LRU eviction completed")
}

// cleanupStaleEntries removes entries for files that no longer exist
func (lru *LRUCache) cleanupStaleEntries() {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	staleCount := 0
	staleSize := int64(0)

	for key, elem := range lru.entries {
		entry := elem.Value.(*LRUEntry)

		// Check if file still exists
		if _, err := os.Stat(entry.FilePath); os.IsNotExist(err) {
			// File was deleted externally, remove from tracking
			lru.currentSize -= entry.Size
			staleSize += entry.Size
			staleCount++

			delete(lru.entries, key)
			lru.lruList.Remove(elem)

			log.Debug().
				Str("key", key).
				Str("path", entry.FilePath).
				Msg("Removed stale entry from L1 cache tracking")
		}
	}

	if staleCount > 0 {
		log.Info().
			Int("stale_count", staleCount).
			Int64("stale_size_mb", staleSize/(1024*1024)).
			Msg("Cleaned up stale cache entries")
	}
}

// RecordAccess records an access to a file and updates LRU ordering
func (lru *LRUCache) RecordAccess(key string, size int64) error {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	filePath := filepath.Join(lru.baseDir, key)

	// Check if entry already exists
	if elem, exists := lru.entries[key]; exists {
		// Move to front (most recently used)
		entry := elem.Value.(*LRUEntry)
		entry.LastAccessed = time.Now()
		lru.lruList.MoveToFront(elem)

		log.Debug().Str("key", key).Msg("Updated access time for existing entry")
		return nil
	}

	// New entry
	entry := &LRUEntry{
		Key:          key,
		Size:         size,
		LastAccessed: time.Now(),
		FilePath:     filePath,
	}

	elem := lru.lruList.PushFront(entry)
	lru.entries[key] = elem
	lru.currentSize += size

	log.Debug().
		Str("key", key).
		Int64("size", size).
		Int64("current_size_mb", lru.currentSize/(1024*1024)).
		Msg("Added new entry to L1 cache")

	// Trigger eviction if over size limit
	if lru.currentSize > lru.maxSize {
		select {
		case lru.evictionChan <- struct{}{}:
		default:
			// Eviction already queued
		}
	}

	return nil
}

// RecordWrite records a write operation and adds/updates entry
func (lru *LRUCache) RecordWrite(key string, size int64) error {
	return lru.RecordAccess(key, size)
}

// RecordDelete removes an entry from tracking
func (lru *LRUCache) RecordDelete(key string) error {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	elem, exists := lru.entries[key]
	if !exists {
		return nil
	}

	entry := elem.Value.(*LRUEntry)
	lru.currentSize -= entry.Size

	delete(lru.entries, key)
	lru.lruList.Remove(elem)

	log.Debug().
		Str("key", key).
		Int64("size", entry.Size).
		Msg("Removed entry from L1 cache tracking")

	return nil
}

// GetStats returns current cache statistics
func (lru *LRUCache) GetStats() map[string]interface{} {
	lru.mu.RLock()
	defer lru.mu.RUnlock()

	return map[string]interface{}{
		"max_size_bytes":     lru.maxSize,
		"max_size_mb":        lru.maxSize / (1024 * 1024),
		"current_size_bytes": lru.currentSize,
		"current_size_mb":    lru.currentSize / (1024 * 1024),
		"entry_count":        lru.lruList.Len(),
		"usage_percent":      float64(lru.currentSize) / float64(lru.maxSize) * 100,
	}
}

// Close stops the LRU cache and cleans up resources
func (lru *LRUCache) Close() error {
	close(lru.stopChan)
	lru.wg.Wait()

	log.Info().Msg("LRU cache closed")
	return nil
}

// ScanAndRebuild scans the base directory and rebuilds the LRU cache from existing files
func (lru *LRUCache) ScanAndRebuild(ctx context.Context) error {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	log.Info().Str("base_dir", lru.baseDir).Msg("Scanning directory to rebuild L1 cache")

	scannedCount := 0
	scannedSize := int64(0)

	err := filepath.Walk(lru.baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Get relative path from base directory
		relPath, err := filepath.Rel(lru.baseDir, path)
		if err != nil {
			return err
		}

		// Add to LRU cache
		entry := &LRUEntry{
			Key:          relPath,
			Size:         info.Size(),
			LastAccessed: info.ModTime(),
			FilePath:     path,
		}

		elem := lru.lruList.PushFront(entry)
		lru.entries[relPath] = elem
		lru.currentSize += info.Size()

		scannedCount++
		scannedSize += info.Size()

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to scan directory: %w", err)
	}

	log.Info().
		Int("file_count", scannedCount).
		Int64("total_size_mb", scannedSize/(1024*1024)).
		Int64("current_size_mb", lru.currentSize/(1024*1024)).
		Int64("max_size_mb", lru.maxSize/(1024*1024)).
		Msg("L1 cache rebuild completed")

	// Trigger eviction if needed
	if lru.currentSize > lru.maxSize {
		select {
		case lru.evictionChan <- struct{}{}:
		default:
		}
	}

	return nil
}

// LRULocalStorage wraps LocalStorage with LRU eviction
type LRULocalStorage struct {
	*LocalStorage
	lruCache *LRUCache
}

// NewLRULocalStorage creates a LocalStorage with LRU eviction
func NewLRULocalStorage(baseDir string, maxSize int64) (*LRULocalStorage, error) {
	// Create base local storage
	localStorage, err := NewLocalStorage(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create local storage: %w", err)
	}

	// Create LRU cache
	lruCache := NewLRUCache(baseDir, maxSize)

	storage := &LRULocalStorage{
		LocalStorage: localStorage,
		lruCache:     lruCache,
	}

	// Scan and rebuild cache from existing files
	ctx := context.Background()
	if err := lruCache.ScanAndRebuild(ctx); err != nil {
		log.Warn().Err(err).Msg("Failed to rebuild L1 cache, starting fresh")
	}

	return storage, nil
}

// Get wraps LocalStorage.Get with LRU tracking
func (lru *LRULocalStorage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	reader, info, err := lru.LocalStorage.Get(ctx, key)
	if err == nil {
		// Record access for LRU
		_ = lru.lruCache.RecordAccess(key, info.Size)
	}
	return reader, info, err
}

// Put wraps LocalStorage.Put with LRU tracking
func (lru *LRULocalStorage) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	info, err := lru.LocalStorage.Put(ctx, key, reader, size, contentType)
	if err == nil {
		// Record write for LRU
		_ = lru.lruCache.RecordWrite(key, info.Size)
	}
	return info, err
}

// StreamingPut wraps LocalStorage.StreamingPut with LRU tracking
func (lru *LRULocalStorage) StreamingPut(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	info, err := lru.LocalStorage.StreamingPut(ctx, key, reader, size, contentType)
	if err == nil {
		// Record write for LRU
		_ = lru.lruCache.RecordWrite(key, info.Size)
	}
	return info, err
}

// Delete wraps LocalStorage.Delete with LRU tracking
func (lru *LRULocalStorage) Delete(ctx context.Context, key string) error {
	err := lru.LocalStorage.Delete(ctx, key)
	if err == nil {
		// Record deletion for LRU
		_ = lru.lruCache.RecordDelete(key)
	}
	return err
}

// GetStats returns LRU cache statistics
func (lru *LRULocalStorage) GetStats() map[string]interface{} {
	return lru.lruCache.GetStats()
}

// Close closes both the storage and LRU cache
func (lru *LRULocalStorage) Close() error {
	_ = lru.lruCache.Close()
	return lru.LocalStorage.Close()
}
