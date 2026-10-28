package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evictStore builds a store in mode over a fresh cache dir (local, hybrid) or
// spool dir (s3) and a fake bucket (hybrid, s3).
func evictStore(t *testing.T, mode string, f Fetcher) (*Store, *LocalStorage, *fakeS3, string) {
	t.Helper()
	dir := t.TempDir()
	var (
		l      *LocalStorage
		remote *S3Storage
		bucket *fakeS3
		spool  string
		err    error
	)
	if mode != "s3" {
		l, err = NewLocalStorage(dir, 0, 0)
		require.NoError(t, err)
	} else {
		spool = dir
	}
	if mode != "local" {
		remote, bucket = newFakeS3(t)
	}
	return NewStore(l, remote, f, spool, false), l, bucket, dir
}

func deletePrefixAsync(s *Store, prefix string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := s.DeletePrefix(context.Background(), prefix)
		done <- err
	}()
	return done
}

// TestStore_EvictWhileSpoolingWins: an evict that lands while the download is
// still spooling returns at once, the readers still get every byte, and the
// commit then leaves nothing in the cache or the bucket.
func TestStore_EvictWhileSpoolingWins(t *testing.T) {
	for _, mode := range []string{"local", "hybrid", "s3"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeFetcher{body: bytes.Repeat([]byte("e"), 100000), gate: make(chan struct{})}
			s, _, bucket, dir := evictStore(t, mode, f)
			key := mustKey(t, "pkg", "pkg-1.0.tar.gz")

			obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
			require.NoError(t, err)
			waitInFlight(t, s, 1)

			select {
			case err := <-deletePrefixAsync(s, mustKey(t, "pkg", "")):
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("DeletePrefix blocked on a flight that is still spooling")
			}
			assert.Len(t, s.InFlight(), 1, "the evicted flight still runs for its readers")

			close(f.gate)
			got, err := io.ReadAll(obj)
			require.NoError(t, err)
			_ = obj.Close()
			assert.Equal(t, f.body, got, "a reader of an evicted flight gets every byte")
			require.NoError(t, s.Close())

			assert.Empty(t, files(t, dir), "nothing cached, no spool left")
			if bucket != nil {
				assert.Nil(t, bucket.object(key), "nothing uploaded")
			}
		})
	}
}

// TestStore_EvictMidUploadWins: an evict that lands while the PUT is running
// waits for it, then deletes, so the object is in neither tier.
func TestStore_EvictMidUploadWins(t *testing.T) {
	for _, mode := range []string{"hybrid", "s3"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeFetcher{body: bytes.Repeat([]byte("u"), 100000)}
			s, l, bucket, _ := evictStore(t, mode, f)
			bucket.putGate, bucket.putSeen = make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(bucket.putGate) })
			t.Cleanup(release) // runs before the fake bucket's server closes
			key := mustKey(t, "pkg", "pkg-1.0.tar.gz")

			obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
			require.NoError(t, err)
			got, err := io.ReadAll(obj)
			require.NoError(t, err)
			_ = obj.Close()
			assert.Equal(t, f.body, got)
			select {
			case <-bucket.putSeen:
			case <-time.After(5 * time.Second):
				t.Fatal("the upload never reached the bucket")
			}

			done := deletePrefixAsync(s, mustKey(t, "pkg", ""))
			select {
			case err := <-done:
				t.Fatalf("DeletePrefix returned (%v) before the upload it races finished", err)
			case <-time.After(100 * time.Millisecond):
			}
			release()
			require.NoError(t, <-done)
			require.NoError(t, s.Close())

			assert.Nil(t, bucket.object(key), "the upload did not recreate the evicted object")
			if l != nil {
				ok, err := exists(l, key)
				require.NoError(t, err)
				assert.False(t, ok, "gone from the cache")
			}
		})
	}
}

// TestStore_EvictCancelledMidUploadDeletesNothing: an evict whose own context
// ends while the PUT it waits on is still running returns that error and
// deletes nothing, so it never deletes before a PUT that may still land.
func TestStore_EvictCancelledMidUploadDeletesNothing(t *testing.T) {
	for _, mode := range []string{"hybrid", "s3"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeFetcher{body: bytes.Repeat([]byte("c"), 100000)}
			s, l, bucket, _ := evictStore(t, mode, f)
			bucket.putGate, bucket.putSeen = make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(bucket.putGate) })
			t.Cleanup(release)
			key := mustKey(t, "pkg", "pkg-1.0.tar.gz")

			obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
			require.NoError(t, err)
			_, err = io.ReadAll(obj)
			require.NoError(t, err)
			_ = obj.Close()
			select {
			case <-bucket.putSeen:
			case <-time.After(5 * time.Second):
				t.Fatal("the upload never reached the bucket")
			}

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				_, err := s.DeletePrefix(ctx, key[:len(key)-len("pkg-1.0.tar.gz")])
				done <- err
			}()
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("DeletePrefix outlived its own context")
			}
			if l != nil {
				ok, err := exists(l, key)
				require.NoError(t, err)
				assert.True(t, ok, "a cancelled evict leaves the cache alone")
			}
			release()
			require.NoError(t, s.Close())
			assert.Equal(t, f.body, bucket.object(key), "a cancelled evict deleted nothing behind the PUT")
		})
	}
}

// TestStore_PromotionDuringEvictIsNotUndone: a hybrid Open that starts while
// DeletePrefix is still deleting from the bucket finds the object there and
// promotes it; that promotion commits after the cache delete, yet stores
// nothing, so the file is in neither tier and the next Open fetches upstream.
func TestStore_PromotionDuringEvictIsNotUndone(t *testing.T) {
	f := &fakeFetcher{body: []byte("upstream bytes")}
	s, l, bucket, _ := evictStore(t, "hybrid", f)
	key := mustKey(t, "pkg", "pkg-1.0.tar.gz")
	bucket.seed(key, []byte("evicted bytes"))
	bucket.delHold, bucket.delHeld = make(chan struct{}), make(chan struct{})
	bucket.gate = make(chan struct{})
	releaseDel, releaseGet := sync.OnceFunc(func() { close(bucket.delHold) }), sync.OnceFunc(func() { close(bucket.gate) })
	t.Cleanup(releaseDel)
	t.Cleanup(releaseGet)

	done := deletePrefixAsync(s, mustKey(t, "pkg", ""))
	select {
	case <-bucket.delHeld: // marked, and still deleting from the bucket
	case <-time.After(5 * time.Second):
		t.Fatal("DeletePrefix never reached the bucket delete")
	}
	obj, err := s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz") // a durable hit: promoted
	require.NoError(t, err)
	releaseDel()
	require.NoError(t, <-done)

	releaseGet() // the promotion finishes and commits after the cache delete
	_, err = io.ReadAll(obj)
	require.NoError(t, err)
	_ = obj.Close()
	require.Eventually(t, func() bool { return len(s.InFlight()) == 0 }, 5*time.Second, time.Millisecond, "the promotion committed")

	ok, err := exists(l, key)
	require.NoError(t, err)
	assert.False(t, ok, "the promotion did not restore the evicted file to the cache")
	assert.Nil(t, bucket.object(key), "gone from the bucket")

	obj, err = s.Open(t.Context(), "pkg", "pkg-1.0.tar.gz")
	require.NoError(t, err)
	got, err := io.ReadAll(obj)
	require.NoError(t, err)
	_ = obj.Close()
	require.NoError(t, s.Close())
	assert.Equal(t, "upstream bytes", string(got))
	assert.Equal(t, int64(1), f.resolves.Load(), "the next Open fetched upstream")
}

// TestStore_LookupSpanningEvictLooksAgain: a hybrid Open whose durable GET
// reads the object while DeletePrefix is deleting from the bucket, and returns
// only after DeletePrefix has finished, registers its flight once the prefix
// is no longer evicting, so nothing marks it. The generation bump after the
// deletes makes it look again and fetch upstream instead of promoting the
// deleted bytes back.
func TestStore_LookupSpanningEvictLooksAgain(t *testing.T) {
	f := &fakeFetcher{body: []byte("upstream bytes")}
	s, l, bucket, _ := evictStore(t, "hybrid", f)
	key := mustKey(t, "pkg", "pkg-1.0.tar.gz")
	bucket.seed(key, []byte("evicted bytes"))
	bucket.delHold, bucket.delHeld = make(chan struct{}), make(chan struct{})
	bucket.getHold, bucket.getHeld = make(chan struct{}), make(chan struct{})
	releaseDel, releaseGet := sync.OnceFunc(func() { close(bucket.delHold) }), sync.OnceFunc(func() { close(bucket.getHold) })
	t.Cleanup(releaseDel)
	t.Cleanup(releaseGet)

	done := deletePrefixAsync(s, mustKey(t, "pkg", ""))
	select {
	case <-bucket.delHeld: // the first generation bump is in, the bucket delete waits
	case <-time.After(5 * time.Second):
		t.Fatal("DeletePrefix never reached the bucket delete")
	}
	got := make(chan []byte, 1)
	go func() {
		obj, err := s.Open(context.Background(), "pkg", "pkg-1.0.tar.gz")
		if err != nil {
			got <- []byte(err.Error())
			return
		}
		body, _ := io.ReadAll(obj)
		_ = obj.Close()
		got <- body
	}()
	select {
	case <-bucket.getHeld: // the GET has read the old object; Open has not locked yet
	case <-time.After(5 * time.Second):
		t.Fatal("Open never reached the durable GET")
	}
	releaseDel()
	require.NoError(t, <-done)
	releaseGet()

	assert.Equal(t, "upstream bytes", string(<-got), "the deleted bytes are not served")
	require.NoError(t, s.Close())
	assert.Equal(t, int64(1), f.resolves.Load(), "the Open fetched upstream")
	cached, err := os.ReadFile(l.buildPath(key))
	require.NoError(t, err)
	assert.Equal(t, "upstream bytes", string(cached), "the evicted bytes were not promoted back into the cache")
	assert.Equal(t, "upstream bytes", string(bucket.object(key)), "the bucket holds the fresh fetch, not the evicted bytes")
}

// list answers a ListObjectsV2 under prefix in one page. Caller holds b.mu.
func (b *fakeS3) list(w http.ResponseWriter, prefix string) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated>`)
	for path := range b.objects {
		if key := strings.TrimPrefix(path, "/b/"); strings.HasPrefix(key, prefix) {
			fmt.Fprintf(&body, `<Contents><Key>%s</Key><Size>1</Size></Contents>`, key)
		}
	}
	body.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, body.String())
}

// delete answers a DeleteObjects. Caller holds b.mu.
func (b *fakeS3) delete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Objects []struct{ Key string } `xml:"Object"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if b.delHold != nil {
		b.mu.Unlock()
		b.delOnce.Do(func() { close(b.delHeld) })
		<-b.delHold
		b.mu.Lock()
	}
	for _, o := range req.Objects {
		delete(b.objects, "/b/"+o.Key)
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><DeleteResult></DeleteResult>`)
}
