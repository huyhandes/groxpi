package index

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The tests here cover only the accounting Resolve cannot observe: what the
// cache charges and refunds. Behaviour (hits, expiry, misses, invalidation) is
// tested through Service in service_test.go and negative_test.go.

func testEntry(body string) *Entry {
	return newPackageEntry([]FileInfo{{Name: "a.whl"}}, []byte(body))
}

// stats reads the cache's entry count and byte total under its lock.
func (c *indexCache) stats() (int, int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries), c.bytes
}

func TestEntry_GzipDecodesToJSON(t *testing.T) {
	body := `{"files":[{"filename":"a.whl","url":"/simple/a/a.whl"}],"meta":{"api-version":"1.0"},"name":"a"}`
	e := testEntry(body)

	zr, err := gzip.NewReader(bytes.NewReader(e.GZIP))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if string(got) != body {
		t.Fatalf("gzip form decodes to %q, want %q", got, body)
	}
	if e.size <= 0 {
		t.Fatal("entry size must be positive")
	}
}

// TestIndexCache_MissIsChargedAFixedSize pins that a cached miss counts against
// the byte budget, so a spray of unknown names cannot grow the cache unbounded.
func TestIndexCache_MissIsChargedAFixedSize(t *testing.T) {
	c := newIndexCache(0, 0)
	defer c.close()

	c.set("ghost", newMissEntry(), time.Minute)
	c.set("other", newMissEntry(), time.Minute)
	if n, b := c.stats(); n != 2 || b != 2*missEntrySize {
		t.Fatalf("two misses hold %d entries, %d bytes; want 2, %d", n, b, 2*missEntrySize)
	}
	c.invalidate("ghost")
	if n, b := c.stats(); n != 1 || b != missEntrySize {
		t.Fatalf("after invalidate: %d entries, %d bytes; want 1, %d", n, b, missEntrySize)
	}
}

// TestIndexCache_MissDropsTheStaleEntry pins that a read which finds an expired
// entry also reclaims it. Leaving it charged against maxBytes until the sweep
// (half an hour away) means live entries get evicted to make room for bytes
// already known to be worthless.
func TestIndexCache_MissDropsTheStaleEntry(t *testing.T) {
	c := newIndexCache(0, 0)
	defer c.close()

	c.set("k", testEntry(`{"a":1}`), -time.Second)

	if _, ok := c.get("k"); ok {
		t.Fatal("an expired entry must not be readable")
	}
	if n, b := c.stats(); n != 0 || b != 0 {
		t.Fatalf("stale entry still charged: %d entries, %d bytes", n, b)
	}
}

func TestIndexCache_ByteBudgetEvictsLeastRecentlyUsed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		one := testEntry(`{"a":1}`)
		c := newIndexCache(2*one.size, 0)
		defer c.close()

		// The sleeps are instant on the bubble's clock; they only keep the
		// recency stamps apart.
		c.set("a", testEntry(`{"a":1}`), time.Minute)
		time.Sleep(time.Millisecond)
		c.set("b", testEntry(`{"a":1}`), time.Minute)
		time.Sleep(time.Millisecond)

		// Touch "a" so "b" becomes the least recently used.
		if _, ok := c.get("a"); !ok {
			t.Fatal("a should still be cached")
		}
		time.Sleep(time.Millisecond)
		c.set("c", testEntry(`{"a":1}`), time.Minute)

		if _, ok := c.get("b"); ok {
			t.Fatal("b was least recently used and should have been evicted")
		}
		if _, ok := c.get("a"); !ok {
			t.Fatal("a was touched and should have survived")
		}
		if _, b := c.stats(); b > 2*one.size {
			t.Fatalf("cache holds %d bytes, over the %d budget", b, 2*one.size)
		}
	})
}

func TestIndexCache_ReplacingAKeyDoesNotDoubleCount(t *testing.T) {
	c := newIndexCache(0, 0)
	defer c.close()

	entry := testEntry(`{"a":1}`)
	for range 5 {
		c.set("k", testEntry(`{"a":1}`), time.Minute)
	}
	if n, b := c.stats(); n != 1 || b != entry.size {
		t.Fatalf("%d entries, %d bytes; want 1, %d", n, b, entry.size)
	}
}

// TestIndexCache_SweepDropsExpiredWithoutARead pins the sweeper: an expired
// entry's bytes are refunded with no read to notice it.
func TestIndexCache_SweepDropsExpiredWithoutARead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newIndexCache(0, time.Minute)
		defer c.close()

		c.set("k", testEntry(`{"a":1}`), 30*time.Second)
		time.Sleep(time.Minute)
		synctest.Wait()

		if n, b := c.stats(); n != 0 || b != 0 {
			t.Fatalf("sweep left %d entries, %d bytes", n, b)
		}
	})
}

// BenchmarkIndexCache_SetOverBudget is the check on the bounded eviction sample:
// the per-write cost must not grow with how many packages are cached. A full scan
// per victim made this quadratic in the number of entries.
func BenchmarkIndexCache_SetOverBudget(b *testing.B) {
	one := testEntry(`{"a":1}`)

	for _, resident := range []int{100, 10000} {
		b.Run(fmt.Sprintf("resident=%d", resident), func(b *testing.B) {
			c := newIndexCache(int64(resident)*one.size, 0)
			defer c.close()

			for i := range resident {
				c.set(fmt.Sprintf("k-%d", i), testEntry(`{"a":1}`), time.Hour)
			}

			for i := 0; b.Loop(); i++ {
				c.set(fmt.Sprintf("new-%d", i), testEntry(`{"a":1}`), time.Hour)
			}
		})
	}
}

func TestIndexCache_ConcurrentAccess(t *testing.T) {
	c := newIndexCache(1024*1024, 0)
	defer c.close()

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 100 {
				key := fmt.Sprintf("k-%d-%d", i, j)
				c.set(key, testEntry(`{"a":1}`), time.Minute)
				c.get(key)
			}
		}(i)
	}
	wg.Wait()
}

// TestIndexCache_ZeroTTLStoresNothing pins that a TTL of zero disables caching
// rather than storing entries that are expired on arrival and never swept.
func TestIndexCache_ZeroTTLStoresNothing(t *testing.T) {
	c := newIndexCache(0, 0)
	defer c.close()

	for i := range 100 {
		c.set(fmt.Sprintf("p%d", i), testEntry(`{"a":1}`), 0)
	}
	if n, b := c.stats(); n != 0 || b != 0 {
		t.Fatalf("TTL 0 left %d entries, %d bytes; want none", n, b)
	}
}

// TestIndexCache_ZeroTTLDropsTheOlderEntry pins that refilling a package with
// TTL 0 removes what was cached before, rather than leaving the old entry to be
// served until it expires.
func TestIndexCache_ZeroTTLDropsTheOlderEntry(t *testing.T) {
	c := newIndexCache(0, 0)
	defer c.close()

	c.set("k", testEntry(`{"a":1}`), time.Minute)
	c.set("k", testEntry(`{"a":2}`), 0)
	if _, ok := c.get("k"); ok {
		t.Fatal("TTL 0 left the older entry in place")
	}
	if n, b := c.stats(); n != 0 || b != 0 {
		t.Fatalf("TTL 0 left %d entries, %d bytes; want none", n, b)
	}
}

// TestIndexCache_OversizeEntryKeepsTheRest pins that one entry bigger than the
// whole budget is not stored, instead of evicting everything to make room it
// can never have.
func TestIndexCache_OversizeEntryKeepsTheRest(t *testing.T) {
	one := testEntry(`{"a":1}`)
	c := newIndexCache(50*one.size, 0)
	defer c.close()

	for i := range 50 {
		c.set(fmt.Sprintf("p%d", i), testEntry(`{"a":1}`), time.Minute)
	}
	c.set("big", testEntry(`{"a":"`+string(bytes.Repeat([]byte("x"), int(50*one.size)))+`"}`), time.Minute)

	if n, b := c.stats(); n != 50 || b != 50*one.size {
		t.Fatalf("after an oversize set: %d entries, %d bytes; want 50, %d", n, b, 50*one.size)
	}
	if _, ok := c.get("big"); ok {
		t.Fatal("an entry bigger than the budget must not be stored")
	}
}
