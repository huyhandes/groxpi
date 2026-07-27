// Package cache holds the in-memory index cache: one entry per cached index
// resource, carrying every representation that resource is served in.
package cache

import (
	"bytes"
	"compress/gzip"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/huyhandes/groxpi/internal/pypi"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// Entry is one cached index resource. The parsed list and the serialised bodies
// are produced together by the constructors below and never mutated afterwards,
// so a reader cannot observe a body that disagrees with the list it came from.
// HTML is not stored: it is the uncommon content type and is cheap to render
// from Files on demand.
type Entry struct {
	Files []pypi.FileInfo // parsed package file list
	JSON  []byte          // marshalled PEP 691 body
	GZIP  []byte          // gzip of JSON, produced once at fill time
	// Index is the redacted URL of the index that answered. The entry is stored
	// with that index's TTL, so recording it is what makes a stale entry
	// attributable to the schedule it expired on.
	Index string

	size      int64
	expiresAt time.Time
	// lastAccess is nanoseconds since the epoch, written by readers holding only
	// the read lock. Recency bookkeeping must not serialise concurrent reads.
	lastAccess atomic.Int64
}

// NewPackageEntry builds the entry for one package's file list. body must be the
// marshalled form of files, and index the redacted URL of the index that answered.
func NewPackageEntry(files []pypi.FileInfo, body []byte, index string) *Entry {
	e := &Entry{Files: files, JSON: body, Index: index}
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

	expired := int64(0)
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			c.removeLocked(key)
			expired++
		}
	}
	if expired > 0 {
		telemetry.CacheEviction(context.Background(), telemetry.LayerIndex, expired)
	}
}

func (c *IndexCache) Get(key string) (*Entry, bool) {
	c.mu.RLock()
	entry, exists := c.entries[key]
	c.mu.RUnlock()

	if !exists {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		// A miss is the moment the entry's bytes are known to be worthless, so
		// they are refunded here rather than left charged against maxBytes until
		// the next sweep - which is half an hour away by default, long enough to
		// evict live entries to make room for dead ones.
		c.mu.Lock()
		if current, still := c.entries[key]; still && current == entry {
			c.removeLocked(key)
		}
		c.mu.Unlock()
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
	telemetry.CacheOccupancy(context.Background(), telemetry.LayerIndex, c.bytes)
}

// removeLocked deletes a key and refunds its bytes. Accounting is incremental:
// nothing re-sums the map. Occupancy is reported here, the one place the total
// changes downwards, and there is no request context to attribute it to.
func (c *IndexCache) removeLocked(key string) {
	if entry, exists := c.entries[key]; exists {
		c.bytes -= entry.size
		delete(c.entries, key)
		telemetry.CacheOccupancy(context.Background(), telemetry.LayerIndex, c.bytes)
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
		telemetry.CacheEviction(context.Background(), telemetry.LayerIndex, 1)
	}
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

func packageKey(packageName string) string { return "package:" + packageName }
