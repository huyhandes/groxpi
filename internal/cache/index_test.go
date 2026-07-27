package cache

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/huyhandes/groxpi/internal/pypi"
)

func testEntry(body string) *Entry {
	return NewPackageEntry([]pypi.FileInfo{{Name: "a.whl"}}, []byte(body))
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
	if e.Size() <= 0 {
		t.Fatal("entry size must be positive")
	}
}

func TestIndexCache_SetAndGet(t *testing.T) {
	c := NewIndexCache(0, 0)
	defer c.Close()

	entry := testEntry(`{"a":1}`)
	c.Set("k", entry, time.Minute)

	got, ok := c.Get("k")
	if !ok || got != entry {
		t.Fatalf("Get returned (%v, %v), want the stored entry", got, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatal("Get of a missing key must miss")
	}
}

func TestIndexCache_ExpiredEntryMisses(t *testing.T) {
	c := NewIndexCache(0, 0)
	defer c.Close()

	for _, ttl := range []time.Duration{0, -time.Second, 10 * time.Millisecond} {
		c.Set("k", testEntry(`{"a":1}`), ttl)
		if ttl > 0 {
			time.Sleep(ttl + 10*time.Millisecond)
		}
		if _, ok := c.Get("k"); ok {
			t.Fatalf("entry with ttl %v must not be readable", ttl)
		}
	}
}

func TestIndexCache_PackageAndListHelpers(t *testing.T) {
	c := NewIndexCache(0, 0)
	defer c.Close()

	c.SetPackage("numpy", testEntry(`{"a":1}`), time.Minute)
	if _, ok := c.GetPackage("numpy"); !ok {
		t.Fatal("package entry should be cached")
	}
	c.InvalidatePackage("numpy")
	if _, ok := c.GetPackage("numpy"); ok {
		t.Fatal("package entry should be invalidated")
	}

	c.Set(ListKey, NewListEntry([]string{"numpy"}, []byte(`{"a":1}`)), time.Minute)
	c.InvalidateList()
	if _, ok := c.Get(ListKey); ok {
		t.Fatal("package list should be invalidated")
	}
	if c.Bytes() != 0 {
		t.Fatalf("invalidation must refund bytes, still holding %d", c.Bytes())
	}
}

func TestIndexCache_ByteBudgetEvictsLeastRecentlyUsed(t *testing.T) {
	one := testEntry(`{"a":1}`)
	c := NewIndexCache(2*one.Size(), 0)
	defer c.Close()

	c.Set("a", testEntry(`{"a":1}`), time.Minute)
	time.Sleep(2 * time.Millisecond)
	c.Set("b", testEntry(`{"a":1}`), time.Minute)
	time.Sleep(2 * time.Millisecond)

	// Touch "a" so "b" becomes the least recently used.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should still be cached")
	}
	time.Sleep(2 * time.Millisecond)
	c.Set("c", testEntry(`{"a":1}`), time.Minute)

	if _, ok := c.Get("b"); ok {
		t.Fatal("b was least recently used and should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a was touched and should have survived")
	}
	if c.Bytes() > 2*one.Size() {
		t.Fatalf("cache holds %d bytes, over the %d budget", c.Bytes(), 2*one.Size())
	}
}

func TestIndexCache_ReplacingAKeyDoesNotDoubleCount(t *testing.T) {
	c := NewIndexCache(0, 0)
	defer c.Close()

	entry := testEntry(`{"a":1}`)
	for range 5 {
		c.Set("k", testEntry(`{"a":1}`), time.Minute)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	if c.Bytes() != entry.Size() {
		t.Fatalf("Bytes = %d, want %d", c.Bytes(), entry.Size())
	}
}

func TestIndexCache_SweepDropsExpiredWithoutARead(t *testing.T) {
	c := NewIndexCache(0, 5*time.Millisecond)
	defer c.Close()

	c.Set("k", testEntry(`{"a":1}`), 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Len() == 0 {
			if c.Bytes() != 0 {
				t.Fatalf("sweep left %d bytes accounted", c.Bytes())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background sweep did not remove the expired entry")
}

func TestIndexCache_ConcurrentAccess(t *testing.T) {
	c := NewIndexCache(1024*1024, 0)
	defer c.Close()

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 100 {
				key := fmt.Sprintf("k-%d-%d", i, j)
				c.Set(key, testEntry(`{"a":1}`), time.Minute)
				c.Get(key)
			}
		}(i)
	}
	wg.Wait()
}
