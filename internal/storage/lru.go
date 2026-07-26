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
	CreatedAt    time.Time
	FilePath     string
}

// objectDeleter removes a stored object by key. It is the single owner of the
// on-disk lifecycle of cached files, so eviction never unlinks files behind the
// storage backend's back.
type objectDeleter interface {
	Delete(ctx context.Context, key string) error
}

// LRUCache implements an LRU eviction policy for local file storage
type LRUCache struct {
	mu           sync.RWMutex
	maxSize      int64                    // Maximum cache size in bytes
	currentSize  int64                    // Current cache size in bytes
	ttl          time.Duration            // TTL for entries (0 = disabled)
	entries      map[string]*list.Element // Key -> list element mapping
	lruList      *list.List               // Doubly-linked list for LRU ordering
	baseDir      string                   // Base directory for cached files
	deleter      objectDeleter            // Removes evicted objects from the backend
	evictionChan chan struct{}            // Channel to trigger eviction checks
	stopChan     chan struct{}            // Channel to stop background eviction
	wg           sync.WaitGroup
}

// NewLRUCache creates a new LRU cache backed by its own LocalStorage deleter.
func NewLRUCache(baseDir string, maxSize int64, ttl time.Duration) *LRUCache {
	local, err := NewLocalStorage(baseDir)
	if err != nil {
		log.Error().
			Err(err).
			Str("base_dir", baseDir).
			Msg("Failed to create local storage for LRU eviction; evictions will be skipped")
		return newLRUCache(baseDir, maxSize, ttl, nil)
	}
	return newLRUCache(baseDir, maxSize, ttl, local)
}

// newLRUCache builds an LRU cache that evicts through the supplied deleter.
// The deleter is fixed at construction time so the eviction worker can read it
// without synchronization.
func newLRUCache(baseDir string, maxSize int64, ttl time.Duration, deleter objectDeleter) *LRUCache {
	cache := &LRUCache{
		maxSize:      maxSize,
		currentSize:  0,
		ttl:          ttl,
		entries:      make(map[string]*list.Element),
		lruList:      list.New(),
		baseDir:      baseDir,
		deleter:      deleter,
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
		Dur("ttl", ttl).
		Msg("LRU cache initialized")

	return cache
}

// evictionWorker runs in the background and performs evictions when needed
func (lru *LRUCache) evictionWorker() {
	defer lru.wg.Done()

	for {
		select {
		case <-lru.stopChan:
			log.Info().Msg("LRU eviction worker stopping")
			return
		case <-lru.evictionChan:
			lru.performEviction()
		}
	}
}

// performEviction evicts entries until size is under limit
// Two-phase eviction when TTL is enabled:
// Phase 1: Evict only expired entries (in LRU order)
// Phase 2: If still over limit, fall back to pure LRU eviction
func (lru *LRUCache) performEviction() {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	// If maxSize is 0, treat as unlimited (no eviction)
	if lru.maxSize == 0 || lru.currentSize <= lru.maxSize {
		return
	}

	evictedCount := 0
	evictedSize := int64(0)
	now := time.Now()

	log.Info().
		Int64("current_size_mb", lru.currentSize/(1024*1024)).
		Int64("max_size_mb", lru.maxSize/(1024*1024)).
		Dur("ttl", lru.ttl).
		Msg("Starting LRU eviction")

	// Phase 1: Evict only expired entries (if TTL is enabled)
	if lru.ttl > 0 {
		// Collect expired entries from back to front (LRU order)
		var expiredElements []*list.Element
		for elem := lru.lruList.Back(); elem != nil; elem = elem.Prev() {
			entry := elem.Value.(*LRUEntry)
			if now.Sub(entry.CreatedAt) > lru.ttl {
				expiredElements = append(expiredElements, elem)
			}
		}

		// Evict expired entries until under size limit
		for _, elem := range expiredElements {
			if lru.currentSize <= lru.maxSize {
				break
			}

			entry := elem.Value.(*LRUEntry)
			if err := lru.evictEntry(elem, entry, true); err == nil {
				evictedCount++
				evictedSize += entry.Size
			}
		}
	}

	// Phase 2: If still over limit, fall back to pure LRU eviction
	if lru.currentSize > lru.maxSize {
		if lru.ttl > 0 {
			log.Warn().
				Int64("current_size_mb", lru.currentSize/(1024*1024)).
				Int64("max_size_mb", lru.maxSize/(1024*1024)).
				Msg("Evicting unexpired entries to meet size limit (all expired entries already evicted)")
		}

		for lru.currentSize > lru.maxSize && lru.lruList.Len() > 0 {
			elem := lru.lruList.Back()
			if elem == nil {
				break
			}

			entry := elem.Value.(*LRUEntry)
			if err := lru.evictEntry(elem, entry, false); err == nil {
				evictedCount++
				evictedSize += entry.Size
			}
		}
	}

	log.Info().
		Int("evicted_count", evictedCount).
		Int64("evicted_size_mb", evictedSize/(1024*1024)).
		Int64("new_size_mb", lru.currentSize/(1024*1024)).
		Msg("LRU eviction completed")
}

// evictEntry removes a single entry from the cache. Deletion goes through the
// storage backend so that the on-disk state and the size accounting have
// exactly one owner.
func (lru *LRUCache) evictEntry(elem *list.Element, entry *LRUEntry, expired bool) error {
	if lru.deleter == nil {
		return fmt.Errorf("cannot evict %q: no deleter configured", entry.Key)
	}

	// Delete the file (a missing file is not an error for the backend)
	if err := lru.deleter.Delete(context.Background(), entry.Key); err != nil {
		log.Error().
			Err(err).
			Str("key", entry.Key).
			Str("path", entry.FilePath).
			Msg("Failed to delete file during eviction")
		return fmt.Errorf("failed to delete %q during eviction: %w", entry.Key, err)
	}

	// Remove from tracking
	lru.currentSize -= entry.Size
	delete(lru.entries, entry.Key)
	lru.lruList.Remove(elem)

	log.Debug().
		Str("key", entry.Key).
		Int64("size", entry.Size).
		Bool("expired", expired).
		Msg("Evicted entry from L1 cache")

	return nil
}

// Touch marks an already-tracked key as most recently used. It reports whether
// the key was tracked, letting read paths avoid a stat syscall on the hot path.
func (lru *LRUCache) Touch(key string) bool {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	return lru.touchLocked(key)
}

// touchLocked bumps recency for an existing entry. Caller must hold lru.mu.
func (lru *LRUCache) touchLocked(key string) bool {
	elem, exists := lru.entries[key]
	if !exists {
		return false
	}

	entry := elem.Value.(*LRUEntry)
	entry.LastAccessed = time.Now()
	lru.lruList.MoveToFront(elem)

	log.Debug().Str("key", key).Msg("Updated access time for existing entry")

	return true
}

// addEntryLocked tracks a previously unknown key. Caller must hold lru.mu.
func (lru *LRUCache) addEntryLocked(key string, size int64) {
	now := time.Now()
	entry := &LRUEntry{
		Key:          key,
		Size:         size,
		LastAccessed: now,
		CreatedAt:    now,
		FilePath:     filepath.Join(lru.baseDir, key),
	}

	lru.entries[key] = lru.lruList.PushFront(entry)
	lru.currentSize += size

	log.Debug().
		Str("key", key).
		Int64("size", size).
		Int64("current_size_mb", lru.currentSize/(1024*1024)).
		Msg("Added new entry to L1 cache")

	lru.triggerEvictionLocked()
}

// triggerEvictionLocked queues an eviction pass if the cache is over its limit.
// Caller must hold lru.mu.
func (lru *LRUCache) triggerEvictionLocked() {
	// maxSize 0 means unlimited - never evict
	if lru.maxSize <= 0 || lru.currentSize <= lru.maxSize {
		return
	}

	select {
	case lru.evictionChan <- struct{}{}:
	default:
		// Eviction already queued
	}
}

// RecordAccess records an access to a file and updates LRU ordering
func (lru *LRUCache) RecordAccess(key string, size int64) error {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	if lru.touchLocked(key) {
		return nil
	}

	lru.addEntryLocked(key, size)

	return nil
}

// RecordWrite records a write operation and adds/updates the entry. Unlike
// RecordAccess it reconciles the tracked size with the payload just written, so
// overwriting a key with a different size cannot drift the accounting.
func (lru *LRUCache) RecordWrite(key string, size int64) error {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	elem, exists := lru.entries[key]
	if !exists {
		lru.addEntryLocked(key, size)
		return nil
	}

	entry := elem.Value.(*LRUEntry)
	now := time.Now()

	lru.currentSize += size - entry.Size
	entry.Size = size
	entry.LastAccessed = now
	entry.CreatedAt = now // Fresh content restarts the TTL clock
	lru.lruList.MoveToFront(elem)

	log.Debug().
		Str("key", key).
		Int64("size", size).
		Int64("current_size_mb", lru.currentSize/(1024*1024)).
		Msg("Updated existing entry in L1 cache")

	lru.triggerEvictionLocked()

	return nil
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
func (lru *LRUCache) GetStats() map[string]any {
	lru.mu.RLock()
	defer lru.mu.RUnlock()

	stats := map[string]any{
		"max_size_bytes":     lru.maxSize,
		"max_size_mb":        lru.maxSize / (1024 * 1024),
		"current_size_bytes": lru.currentSize,
		"current_size_mb":    lru.currentSize / (1024 * 1024),
		"entry_count":        lru.lruList.Len(),
		"usage_percent":      float64(lru.currentSize) / float64(lru.maxSize) * 100,
		"ttl_enabled":        lru.ttl > 0,
		"ttl_seconds":        int64(lru.ttl.Seconds()),
	}

	// Count expired entries (if TTL enabled)
	if lru.ttl > 0 {
		now := time.Now()
		expiredCount := 0
		for elem := lru.lruList.Front(); elem != nil; elem = elem.Next() {
			entry := elem.Value.(*LRUEntry)
			if now.Sub(entry.CreatedAt) > lru.ttl {
				expiredCount++
			}
		}
		stats["expired_count"] = expiredCount
	}

	return stats
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

		// Add to LRU cache (use ModTime as CreatedAt for existing files)
		entry := &LRUEntry{
			Key:          relPath,
			Size:         info.Size(),
			LastAccessed: info.ModTime(),
			CreatedAt:    info.ModTime(),
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

	lru.triggerEvictionLocked()

	return nil
}

// LRULocalStorage wraps LocalStorage with LRU eviction.
//
// The inner storage is held in an explicit field rather than embedded: every
// method is written out, so a read path that forgets to record an access is a
// compile error instead of a silent fall-through. That fall-through was a real
// bug - a hot file read only through GetFilePath/Stat looked cold to the LRU
// and could be evicted while it was being served.
//
// Read paths that return the object's size record an access; metadata-only or
// bulk-listing calls forward without touching recency (see each method).
//
// Capabilities are forwarded explicitly, never inherited: the wrapper exposes
// exactly the capability set of its inner *LocalStorage — zero-copy yes,
// presigning no — and the assertions below fail the build if that drifts.
type LRULocalStorage struct {
	inner    *LocalStorage
	lruCache *LRUCache
}

var (
	// LRULocalStorage must remain usable as the L1 tier of TieredStorage, which
	// requires core storage plus the zero-copy capability.
	_ l1Storage       = (*LRULocalStorage)(nil)
	_ Storage         = (*LRULocalStorage)(nil)
	_ ZeroCopyCapable = (*LRULocalStorage)(nil)
)

// NewLRULocalStorage creates a LocalStorage with LRU eviction
func NewLRULocalStorage(baseDir string, maxSize int64, ttl time.Duration) (*LRULocalStorage, error) {
	// Create base local storage
	localStorage, err := NewLocalStorage(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create local storage: %w", err)
	}

	// Create LRU cache with TTL. The eviction path deletes through the same
	// LocalStorage instance, so on-disk state has exactly one owner.
	lruCache := newLRUCache(baseDir, maxSize, ttl, localStorage)

	storage := &LRULocalStorage{
		inner:    localStorage,
		lruCache: lruCache,
	}

	// Scan and rebuild cache from existing files
	ctx := context.Background()
	if err := lruCache.ScanAndRebuild(ctx); err != nil {
		log.Warn().Err(err).Msg("Failed to rebuild L1 cache, starting fresh")
	}

	return storage, nil
}

// recordAccess marks key as recently used. Entries already tracked are bumped
// without any syscall; an untracked key (for example a file written outside
// this wrapper) is stat'd once so it can be accounted for.
func (lru *LRULocalStorage) recordAccess(ctx context.Context, key string) {
	if lru.lruCache.Touch(key) {
		return
	}

	info, err := lru.inner.Stat(ctx, key)
	if err != nil {
		log.Debug().
			Err(err).
			Str("key", key).
			Msg("Could not stat file to track L1 access")
		return
	}

	_ = lru.lruCache.RecordAccess(key, info.Size)
}

// Get reads an object and records the access.
func (lru *LRULocalStorage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	reader, info, err := lru.inner.Get(ctx, key)
	if err != nil {
		return nil, nil, err
	}

	_ = lru.lruCache.RecordAccess(key, info.Size)

	return reader, info, nil
}

// Put stores an object and records the write.
func (lru *LRULocalStorage) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	info, err := lru.inner.Put(ctx, key, reader, size, contentType)
	if err != nil {
		return nil, err
	}

	_ = lru.lruCache.RecordWrite(key, info.Size)

	return info, nil
}

// GetFilePath returns the path used for zero-copy serving and records the
// access. This is the hottest read path in the server: the file is about to be
// sent, so it must not look cold to the evictor.
func (lru *LRULocalStorage) GetFilePath(ctx context.Context, key string) (string, error) {
	path, err := lru.inner.GetFilePath(ctx, key)
	if err != nil {
		return "", err
	}

	lru.recordAccess(ctx, key)

	return path, nil
}

// Stat returns object metadata and records the access. Callers stat a file
// immediately before serving it, so this counts as use.
func (lru *LRULocalStorage) Stat(ctx context.Context, key string) (*ObjectInfo, error) {
	info, err := lru.inner.Stat(ctx, key)
	if err != nil {
		return nil, err
	}

	_ = lru.lruCache.RecordAccess(key, info.Size)

	return info, nil
}

// Delete removes an object and drops it from LRU tracking.
func (lru *LRULocalStorage) Delete(ctx context.Context, key string) error {
	if err := lru.inner.Delete(ctx, key); err != nil {
		return err
	}

	_ = lru.lruCache.RecordDelete(key)

	return nil
}

// Exists forwards without recording: a presence check is a routing decision,
// not use of the content, and it yields no size to account for.
func (lru *LRULocalStorage) Exists(ctx context.Context, key string) (bool, error) {
	return lru.inner.Exists(ctx, key)
}

// List forwards without recording: bulk enumeration would reorder the whole
// cache and make recency meaningless.
func (lru *LRULocalStorage) List(ctx context.Context, opts ListOptions) ([]*ObjectInfo, error) {
	return lru.inner.List(ctx, opts)
}

// GetStats returns LRU cache statistics
func (lru *LRULocalStorage) GetStats() map[string]any {
	return lru.lruCache.GetStats()
}

// Close closes both the storage and LRU cache
func (lru *LRULocalStorage) Close() error {
	_ = lru.lruCache.Close()
	return lru.inner.Close()
}
