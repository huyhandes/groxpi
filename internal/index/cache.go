package index

import (
	"bytes"
	"compress/gzip"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/huyhandes/groxpi/internal/telemetry"
)

// Entry is one cached package index. The parsed list and the serialised bodies
// are produced together by the constructors below and never mutated afterwards,
// so a reader cannot observe a body that disagrees with the list it came from.
// HTML is not stored: it is the uncommon content type and is cheap to render
// from Files on demand.
type Entry struct {
	Files []FileInfo // parsed package file list
	JSON  []byte     // marshalled PEP 691 body
	GZIP  []byte     // gzip of JSON, produced once at fill time

	// missing marks a cached "not found on every index". Such an entry has no
	// files or bodies, and Resolve answers it with ErrNotFound instead of
	// handing it out.
	missing   bool
	size      int64
	expiresAt time.Time
	// lastAccess is nanoseconds since the epoch, written by readers holding only
	// the read lock. Recency bookkeeping must not serialise concurrent reads.
	lastAccess atomic.Int64
}

// newPackageEntry builds the entry for one package's file list. body must be the
// marshalled form of files.
func newPackageEntry(files []FileInfo, body []byte) *Entry {
	e := &Entry{Files: files, JSON: body}
	e.finish()
	return e
}

// missEntrySize is what a cached miss charges against the byte budget. It holds
// no body, but a scanner spraying unknown names must still be bounded by the
// budget rather than grow the map without limit.
const missEntrySize = 64

// newMissEntry is the entry for a package no configured index has.
func newMissEntry() *Entry {
	return &Entry{missing: true, size: missEntrySize}
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

// indexCache maps a normalised package name to its Entry, bounded by a total
// byte budget with least-recently-used eviction, and swept for expired entries
// on a timer so staleness is reclaimed without waiting for a read.
type indexCache struct {
	mu       sync.RWMutex
	entries  map[string]*Entry
	bytes    int64
	maxBytes int64

	stop     chan struct{}
	stopOnce sync.Once
}

// newIndexCache returns a cache bounded to maxBytes across every stored
// representation (<= 0 means unbounded) that sweeps expired entries every
// sweepEvery (<= 0 disables the sweep). Call close to stop the sweeper.
func newIndexCache(maxBytes int64, sweepEvery time.Duration) *indexCache {
	c := &indexCache{
		entries:  make(map[string]*Entry),
		maxBytes: maxBytes,
		stop:     make(chan struct{}),
	}
	if sweepEvery > 0 {
		go c.sweep(sweepEvery)
	}
	return c
}

// close stops the background sweeper. Safe to call more than once.
func (c *indexCache) close() {
	c.stopOnce.Do(func() { close(c.stop) })
}

func (c *indexCache) sweep(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.expire()
		}
	}
}

// expire drops every entry past its TTL. Only the sweeper calls it.
func (c *indexCache) expire() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	expired := int64(0)
	for pkg, entry := range c.entries {
		if now.After(entry.expiresAt) {
			c.removeLocked(pkg)
			expired++
		}
	}
	if expired > 0 {
		telemetry.CacheEviction(context.Background(), telemetry.LayerIndex, expired)
	}
}

// get returns a package's live entry, which may be a cached miss.
func (c *indexCache) get(pkg string) (*Entry, bool) {
	c.mu.RLock()
	entry, exists := c.entries[pkg]
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
		if current, still := c.entries[pkg]; still && current == entry {
			c.removeLocked(pkg)
		}
		c.mu.Unlock()
		return nil, false
	}
	entry.lastAccess.Store(time.Now().UnixNano())
	return entry, true
}

// set stores a package's entry for ttl, evicting to stay inside the budget. A
// ttl of zero means "do not cache": the entry would be expired on arrival, and
// with no positive TTL there is no sweeper to reclaim it. An entry bigger than
// the whole budget is not stored either, since evicting everything else would
// still not make room for it.
func (c *indexCache) set(pkg string, entry *Entry, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	now := time.Now()
	entry.expiresAt = now.Add(ttl)
	entry.lastAccess.Store(now.UnixNano())

	c.mu.Lock()
	defer c.mu.Unlock()

	// Replacing a package subtracts the old charge, so it cannot be accounted
	// for twice however often it is refilled.
	c.removeLocked(pkg)
	if c.maxBytes > 0 && entry.size > c.maxBytes {
		return
	}
	c.entries[pkg] = entry
	c.bytes += entry.size
	c.evictLocked()
	telemetry.CacheOccupancy(context.Background(), telemetry.LayerIndex, c.bytes)
}

// invalidate drops a package's entry, a cached miss included.
func (c *indexCache) invalidate(pkg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(pkg)
}

// removeLocked deletes a package and refunds its bytes. Accounting is
// incremental: nothing re-sums the map. Occupancy is reported here, the one
// place the total changes downwards, and there is no request context to
// attribute it to.
func (c *indexCache) removeLocked(pkg string) {
	if entry, exists := c.entries[pkg]; exists {
		c.bytes -= entry.size
		delete(c.entries, pkg)
		telemetry.CacheOccupancy(context.Background(), telemetry.LayerIndex, c.bytes)
	}
}

// evictSample bounds how many entries are examined to pick one victim. Go
// randomises the iteration order of a map, so the sample is a random one and the
// oldest of a handful is very likely to be genuinely cold.
//
// ponytail: approximate LRU. An exact one needs a recency list, which means
// taking the write lock on every read - the read path deliberately updates
// recency under the read lock with an atomic. Swap in the list only if a measured
// hit rate says the approximation costs something.
const evictSample = 8

// evictLocked drops least-recently-used entries until the cache fits its budget.
// Each victim costs a bounded sample rather than a full scan, so filling a cache
// of n packages stays linear in n instead of quadratic.
func (c *indexCache) evictLocked() {
	for c.maxBytes > 0 && c.bytes > c.maxBytes && len(c.entries) > 0 {
		var victim string
		var oldest int64
		seen := 0
		for pkg, entry := range c.entries {
			if access := entry.lastAccess.Load(); victim == "" || access < oldest {
				victim, oldest = pkg, access
			}
			if seen++; seen == evictSample {
				break
			}
		}
		c.removeLocked(victim)
		telemetry.CacheEviction(context.Background(), telemetry.LayerIndex, 1)
	}
}
