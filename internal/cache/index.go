// Package cache holds the in-memory index cache: one entry per cached index
// resource, carrying every representation that resource is served in.
package cache

import (
	"bytes"
	"compress/gzip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/huyhandes/groxpi/internal/pypi"
)

// Entry is one cached index resource. The parsed list and the serialised bodies
// are produced together by the constructors below and never mutated afterwards,
// so a reader cannot observe a body that disagrees with the list it came from.
// HTML is not stored: it is the uncommon content type and is cheap to render
// from Files on demand.
type Entry struct {
	Files []pypi.FileInfo // parsed package file list; nil for the package-list entry
	Names []string        // parsed package list; nil for package-file entries
	JSON  []byte          // marshalled PEP 691 body
	GZIP  []byte          // gzip of JSON, produced once at fill time

	size      int64
	expiresAt time.Time
	// lastAccess is nanoseconds since the epoch, written by readers holding only
	// the read lock. Recency bookkeeping must not serialise concurrent reads.
	lastAccess atomic.Int64
}

// NewPackageEntry builds the entry for one package's file list. body must be the
// marshalled form of files.
func NewPackageEntry(files []pypi.FileInfo, body []byte) *Entry {
	e := &Entry{Files: files, JSON: body}
	e.finish()
	return e
}

// NewListEntry builds the entry for the full package list. body must be the
// marshalled form of names.
func NewListEntry(names []string, body []byte) *Entry {
	e := &Entry{Names: names, JSON: body}
	e.finish()
	return e
}

func (e *Entry) finish() {
	var buf bytes.Buffer
	buf.Grow(len(e.JSON) / 2)
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err == nil {
		if _, err = zw.Write(e.JSON); err == nil {
			err = zw.Close()
		}
	}
	if err == nil {
		e.GZIP = buf.Bytes()
	}
	// The parsed structures are not walked to be measured; the marshalled body is
	// the same order of magnitude and costs nothing to count.
	e.size = int64(2*len(e.JSON) + len(e.GZIP))
}

// Size is the entry's charge against the cache byte budget.
func (e *Entry) Size() int64 { return e.size }

// IndexCache maps an index key to its Entry, bounded by a total byte budget with
// least-recently-used eviction, and swept for expired entries on a timer so
// staleness is reclaimed without waiting for a read.
type IndexCache struct {
	mu       sync.RWMutex
	entries  map[string]*Entry
	bytes    int64
	maxBytes int64

	stop     chan struct{}
	stopOnce sync.Once
}

// NewIndexCache returns a cache bounded to maxBytes across every stored
// representation (<= 0 means unbounded) that sweeps expired entries every
// sweepEvery (<= 0 disables the sweep). Call Close to stop the sweeper.
func NewIndexCache(maxBytes int64, sweepEvery time.Duration) *IndexCache {
	c := &IndexCache{
		entries:  make(map[string]*Entry),
		maxBytes: maxBytes,
		stop:     make(chan struct{}),
	}
	if sweepEvery > 0 {
		go c.sweep(sweepEvery)
	}
	return c
}

// Close stops the background sweeper. Safe to call more than once.
func (c *IndexCache) Close() {
	c.stopOnce.Do(func() { close(c.stop) })
}

func (c *IndexCache) sweep(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.Expire()
		}
	}
}

// Expire drops every entry past its TTL. Called by the sweeper; exported so the
// sweep can be exercised without waiting on a timer.
func (c *IndexCache) Expire() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			c.removeLocked(key)
		}
	}
}

func (c *IndexCache) Get(key string) (*Entry, bool) {
	c.mu.RLock()
	entry, exists := c.entries[key]
	c.mu.RUnlock()

	if !exists || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	entry.lastAccess.Store(time.Now().UnixNano())
	return entry, true
}

func (c *IndexCache) Set(key string, entry *Entry, ttl time.Duration) {
	now := time.Now()
	entry.expiresAt = now.Add(ttl)
	entry.lastAccess.Store(now.UnixNano())

	c.mu.Lock()
	defer c.mu.Unlock()

	// Replacing a key subtracts the old charge, so a key cannot be accounted for
	// twice however often it is refilled.
	c.removeLocked(key)
	c.entries[key] = entry
	c.bytes += entry.size
	c.evictLocked()
}

// removeLocked deletes a key and refunds its bytes. Accounting is incremental:
// nothing re-sums the map.
func (c *IndexCache) removeLocked(key string) {
	if entry, exists := c.entries[key]; exists {
		c.bytes -= entry.size
		delete(c.entries, key)
	}
}

// evictLocked drops least-recently-used entries until the cache fits its budget.
// Picking the victim scans the map, but only while over budget: the common write
// costs one map assignment regardless of how many packages are cached.
func (c *IndexCache) evictLocked() {
	for c.maxBytes > 0 && c.bytes > c.maxBytes && len(c.entries) > 0 {
		var victim string
		var oldest int64
		for key, entry := range c.entries {
			if access := entry.lastAccess.Load(); victim == "" || access < oldest {
				victim, oldest = key, access
			}
		}
		c.removeLocked(victim)
	}
}

func (c *IndexCache) InvalidateList() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(ListKey)
}

func (c *IndexCache) InvalidatePackage(packageName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(packageKey(packageName))
}

func (c *IndexCache) GetPackage(packageName string) (*Entry, bool) {
	return c.Get(packageKey(packageName))
}

func (c *IndexCache) SetPackage(packageName string, entry *Entry, ttl time.Duration) {
	c.Set(packageKey(packageName), entry, ttl)
}

// Len reports the number of live entries, Bytes their total charge. Both exist
// for tests and future metrics.
func (c *IndexCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func (c *IndexCache) Bytes() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bytes
}

// ListKey names the full package list.
const ListKey = "package-list"

func packageKey(packageName string) string { return "package:" + packageName }
