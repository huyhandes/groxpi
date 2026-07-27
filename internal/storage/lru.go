package storage

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/huyhandes/groxpi/internal/telemetry"
)

// LRUEntry represents an entry in the LRU cache.
//
// Eviction order comes from the list ordering alone; CreatedAt exists only to
// drive TTL expiry. Hits is a display column for the cache listing, written on
// the access path under the same write lock. It must never become the eviction
// signal: a hit-count comparison would have to scan every entry to find a victim
// that the list already has at its back.
type LRUEntry struct {
	Key       string
	Size      int64
	CreatedAt time.Time
	Hits      int64
}

// PrefixDeleter deletes every cached object whose key starts with a prefix. It is
// a capability of the local cache, which knows its own contents, not of every
// backend: nothing here lists the object store.
type PrefixDeleter interface {
	DeletePrefix(ctx context.Context, prefix string) (int, error)
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

// maxTTLSweepInterval caps how long the cache can go without checking for
// expired entries, however long the TTL is.
const maxTTLSweepInterval = time.Minute

// ttlSweepInterval picks how often to sweep for expired entries: half the TTL,
// so an entry outlives its TTL by at most 50%, capped so a long TTL does not
// leave stale files on disk for hours.
func ttlSweepInterval(ttl time.Duration) time.Duration {
	interval := ttl / 2
	if interval > maxTTLSweepInterval {
		return maxTTLSweepInterval
	}
	if interval < time.Millisecond {
		return time.Millisecond
	}
	return interval
}

// newLRUCache builds an LRU cache that evicts through the supplied deleter.
//
// The deleter must be non-nil: it is fixed at construction time so the eviction
// worker can read it without synchronization, and a cache that cannot delete
// cannot evict, which silently unbounds the cache.
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

	slog.Info("LRU cache initialized",
		"base_dir", baseDir,
		"max_size_bytes", maxSize,
		"max_size_mb", maxSize/(1024*1024),
		"ttl", ttl)

	return cache
}

// evictionWorker runs in the background and performs evictions when needed.
//
// Two independent triggers: a size-driven pass queued by triggerEvictionLocked,
// and - only when a TTL is configured - a periodic sweep for expired entries.
// The sweep is what makes the TTL mean anything: size-driven eviction returns
// early while the cache is under quota, so without it a configured TTL would
// never expire anything on a cache that does not fill up.
func (lru *LRUCache) evictionWorker() {
	defer lru.wg.Done()

	// A nil channel blocks forever, which disables the sweep arm of the select
	// when no TTL is configured.
	var sweepC <-chan time.Time
	if lru.ttl > 0 {
		ticker := time.NewTicker(ttlSweepInterval(lru.ttl))
		defer ticker.Stop()
		sweepC = ticker.C
	}

	for {
		select {
		case <-lru.stopChan:
			slog.Info("LRU eviction worker stopping")
			return
		case <-lru.evictionChan:
			lru.performEviction()
		case <-sweepC:
			lru.expireEntries()
		}
	}
}

// expireEntries evicts every entry past its TTL, regardless of how full the
// cache is. Caller must not hold lru.mu.
func (lru *LRUCache) expireEntries() {
	lru.mu.Lock()
	keys := lru.expireLocked()
	lru.mu.Unlock()

	if err := lru.unlink(context.Background(), keys); err != nil {
		slog.Error("Failed to delete expired files", "error", err)
	}
}

// expireLocked detaches every entry past its TTL and returns their keys for
// unlinking. Caller must hold lru.mu.
func (lru *LRUCache) expireLocked() []string {
	if lru.ttl <= 0 {
		return nil
	}

	now := time.Now()
	var keys []string
	var size int64

	// CreatedAt is not ordered by list position (a rewrite restarts an entry's
	// TTL clock without moving it), so every entry is checked.
	for elem := lru.lruList.Back(); elem != nil; {
		prev := elem.Prev()

		entry := elem.Value.(*LRUEntry)
		if now.Sub(entry.CreatedAt) > lru.ttl {
			size += entry.Size
			keys = append(keys, lru.detachLocked(elem, entry))
		}

		elem = prev
	}

	if len(keys) > 0 {
		slog.Info("Expired entries from L1 cache",
			"expired_count", len(keys),
			"expired_size_mb", size/(1024*1024),
			"current_size_mb", lru.currentSize/(1024*1024),
			"ttl", lru.ttl)
	}

	return keys
}

// performEviction evicts entries until size is under limit: everything already
// expired first, then pure LRU order for as long as the cache is still over
// quota. Caller must not hold lru.mu.
func (lru *LRUCache) performEviction() {
	lru.mu.Lock()

	// If maxSize is 0, treat as unlimited (no eviction)
	if lru.maxSize == 0 || lru.currentSize <= lru.maxSize {
		lru.mu.Unlock()
		return
	}

	slog.Info("Starting LRU eviction",
		"current_size_mb", lru.currentSize/(1024*1024),
		"max_size_mb", lru.maxSize/(1024*1024),
		"ttl", lru.ttl)

	keys := lru.expireLocked()

	if lru.currentSize > lru.maxSize {
		if lru.ttl > 0 {
			slog.Warn("Evicting unexpired entries to meet size limit (all expired entries already evicted)",
				"current_size_mb", lru.currentSize/(1024*1024),
				"max_size_mb", lru.maxSize/(1024*1024))
		}

		for lru.currentSize > lru.maxSize {
			elem := lru.lruList.Back()
			if elem == nil {
				break
			}

			keys = append(keys, lru.detachLocked(elem, elem.Value.(*LRUEntry)))
		}
	}

	newSize := lru.currentSize
	lru.mu.Unlock()

	slog.Info("LRU eviction completed",
		"evicted_count", len(keys),
		"new_size_mb", newSize/(1024*1024))

	if err := lru.unlink(context.Background(), keys); err != nil {
		slog.Error("Failed to delete evicted files", "error", err)
	}
}

// detachLocked drops an entry from tracking and returns its key so the caller can
// unlink the file with the lock released. It is the one place the size accounting
// shrinks, which makes it the one place occupancy can be reported from. Caller
// must hold lru.mu.
func (lru *LRUCache) detachLocked(elem *list.Element, entry *LRUEntry) string {
	lru.currentSize -= entry.Size
	delete(lru.entries, entry.Key)
	lru.lruList.Remove(elem)

	telemetry.CacheOccupancy(context.Background(), telemetry.LayerLocal, lru.currentSize)

	slog.Debug("Detached entry from L1 cache", "key", entry.Key, "size", entry.Size)

	return entry.Key
}

// unlink removes detached objects through the storage backend, which owns the
// on-disk lifecycle. Caller must NOT hold lru.mu: one unlink per file, and
// evicting a package means thousands of them, so holding the lock across this
// would stall every read path for the duration.
func (lru *LRUCache) unlink(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	var errs []error
	for _, key := range keys {
		// A missing file is not an error for the backend.
		if err := lru.deleter.Delete(ctx, key); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete %q during eviction: %w", key, err))
		}
	}

	telemetry.CacheEviction(ctx, telemetry.LayerLocal, int64(len(keys)))

	return errors.Join(errs...)
}

// touch marks an already-tracked key as most recently used. It reports whether
// the key was tracked, letting read paths avoid a stat syscall on the hot path.
func (lru *LRUCache) touch(key string) bool {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	return lru.touchLocked(key)
}

// Snapshot copies every tracked entry, most recently used first. The copies mean
// a caller can render the listing without holding the lock or racing the
// evictor.
func (lru *LRUCache) Snapshot() []LRUEntry {
	lru.mu.RLock()
	defer lru.mu.RUnlock()

	rows := make([]LRUEntry, 0, lru.lruList.Len())
	for elem := lru.lruList.Front(); elem != nil; elem = elem.Next() {
		rows = append(rows, *elem.Value.(*LRUEntry))
	}

	return rows
}

// DeletePrefix removes every tracked object whose key starts with prefix and
// reports how many were removed. Deletion goes through the same eviction path as
// any other removal, so size accounting and the recency list stay consistent. A
// prefix that matches nothing is not an error.
func (lru *LRUCache) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	lru.mu.Lock()

	var keys []string
	for elem := lru.lruList.Back(); elem != nil; {
		prev := elem.Prev()

		entry := elem.Value.(*LRUEntry)
		if strings.HasPrefix(entry.Key, prefix) {
			keys = append(keys, lru.detachLocked(elem, entry))
		}

		elem = prev
	}

	lru.mu.Unlock()

	return len(keys), lru.unlink(ctx, keys)
}

// touchLocked bumps recency for an existing entry. Caller must hold lru.mu.
func (lru *LRUCache) touchLocked(key string) bool {
	elem, exists := lru.entries[key]
	if !exists {
		return false
	}

	lru.lruList.MoveToFront(elem)
	elem.Value.(*LRUEntry).Hits++

	slog.Debug("Updated access time for existing entry", "key", key)

	return true
}

// addEntryLocked tracks a previously unknown key and returns its entry. Caller
// must hold lru.mu.
func (lru *LRUCache) addEntryLocked(key string, size int64) *LRUEntry {
	entry := &LRUEntry{
		Key:       key,
		Size:      size,
		CreatedAt: time.Now(),
	}

	lru.entries[key] = lru.lruList.PushFront(entry)
	lru.currentSize += size
	telemetry.CacheOccupancy(context.Background(), telemetry.LayerLocal, lru.currentSize)

	slog.Debug("Added new entry to L1 cache",
		"key", key,
		"size", size,
		"current_size_mb", lru.currentSize/(1024*1024))

	lru.triggerEvictionLocked()

	return entry
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
func (lru *LRUCache) RecordAccess(key string, size int64) {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	if lru.touchLocked(key) {
		return
	}

	lru.addEntryLocked(key, size).Hits++
}

// RecordWrite records a write operation and adds/updates the entry. Unlike
// RecordAccess it reconciles the tracked size with the payload just written, so
// overwriting a key with a different size cannot drift the accounting.
func (lru *LRUCache) RecordWrite(key string, size int64) {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	elem, exists := lru.entries[key]
	if !exists {
		lru.addEntryLocked(key, size)
		return
	}

	entry := elem.Value.(*LRUEntry)

	lru.currentSize += size - entry.Size
	entry.Size = size
	entry.CreatedAt = time.Now() // Fresh content restarts the TTL clock
	lru.lruList.MoveToFront(elem)

	slog.Debug("Updated existing entry in L1 cache",
		"key", key,
		"size", size,
		"current_size_mb", lru.currentSize/(1024*1024))

	lru.triggerEvictionLocked()
}

// RecordDelete removes an entry from tracking. The file is already gone - the
// caller deleted it - so this only reconciles the accounting, which is why it
// goes through detachLocked rather than unlink: skipping detachLocked is what
// used to leave the occupancy gauge permanently high after an explicit delete.
func (lru *LRUCache) RecordDelete(key string) {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	elem, exists := lru.entries[key]
	if !exists {
		return
	}

	lru.detachLocked(elem, elem.Value.(*LRUEntry))
}

// Close stops the LRU cache and cleans up resources
func (lru *LRUCache) Close() error {
	close(lru.stopChan)
	lru.wg.Wait()

	slog.Info("LRU cache closed")
	return nil
}

// ScanAndRebuild scans the base directory and rebuilds the LRU cache from existing files
func (lru *LRUCache) ScanAndRebuild() error {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	slog.Info("Scanning directory to rebuild L1 cache", "base_dir", lru.baseDir)

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

		// A file that survived a restart keeps its age: ModTime is its CreatedAt,
		// so a configured TTL still expires it.
		lru.addEntryLocked(relPath, info.Size()).CreatedAt = info.ModTime()

		scannedCount++
		scannedSize += info.Size()

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to scan directory: %w", err)
	}

	slog.Info("L1 cache rebuild completed",
		"file_count", scannedCount,
		"total_size_mb", scannedSize/(1024*1024),
		"current_size_mb", lru.currentSize/(1024*1024),
		"max_size_mb", lru.maxSize/(1024*1024))

	lru.triggerEvictionLocked()

	return nil
}

// LRULocalStorage wraps LocalStorage with LRU eviction.
//
// The inner storage is an explicit field, never embedded, so a read path that
// forgets to record an access is a compile error rather than a file that looks
// cold to the evictor while it is being served. Capabilities are likewise
// forwarded explicitly - zero-copy yes, presigning no - and the assertions
// below fail the build if that drifts.
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
	_ PrefixDeleter   = (*LRULocalStorage)(nil)
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
	if err := lruCache.ScanAndRebuild(); err != nil {
		slog.Warn("Failed to rebuild L1 cache, starting fresh", "error", err)
	}

	return storage, nil
}

// recordAccess marks key as recently used. Entries already tracked are bumped
// without any syscall; an untracked key (for example a file written outside
// this wrapper) is stat'd once so it can be accounted for.
func (lru *LRULocalStorage) recordAccess(ctx context.Context, key string) {
	if lru.lruCache.touch(key) {
		return
	}

	info, err := lru.inner.stat(ctx, key)
	if err != nil {
		slog.Debug("Could not stat file to track L1 access", "error", err, "key", key)
		return
	}

	lru.lruCache.RecordAccess(key, info.Size)
}

// Get reads an object and records the access.
func (lru *LRULocalStorage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	reader, info, err := lru.inner.Get(ctx, key)
	if err != nil {
		return nil, nil, err
	}

	lru.lruCache.RecordAccess(key, info.Size)

	return reader, info, nil
}

// Put stores an object and records the write.
func (lru *LRULocalStorage) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	info, err := lru.inner.Put(ctx, key, reader, size, contentType)
	if err != nil {
		return nil, err
	}

	lru.lruCache.RecordWrite(key, info.Size)

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

// Delete removes an object and drops it from LRU tracking.
func (lru *LRULocalStorage) Delete(ctx context.Context, key string) error {
	if err := lru.inner.Delete(ctx, key); err != nil {
		return err
	}

	lru.lruCache.RecordDelete(key)

	return nil
}

// Snapshot reports what the cache holds, for the cache listing.
func (lru *LRULocalStorage) Snapshot() []LRUEntry {
	return lru.lruCache.Snapshot()
}

// DeletePrefix removes every cached object under prefix, for evicting one
// package's files.
func (lru *LRULocalStorage) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	return lru.lruCache.DeletePrefix(ctx, prefix)
}

// Exists forwards without recording: a presence check is a routing decision,
// not use of the content, and it yields no size to account for.
func (lru *LRULocalStorage) Exists(ctx context.Context, key string) (bool, error) {
	return lru.inner.Exists(ctx, key)
}

// Close closes both the storage and LRU cache
func (lru *LRULocalStorage) Close() error {
	_ = lru.lruCache.Close()
	return lru.inner.Close()
}
