package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingS3Server stands in for an S3 endpoint and records the requests it
// saw. It answers everything with 200, which is enough for the SDK to consider
// a HEAD or a PUT successful.
type recordingS3Server struct {
	*httptest.Server

	mu       sync.Mutex
	requests []*http.Request
}

func newRecordingS3Server(t *testing.T) *recordingS3Server {
	t.Helper()

	rec := &recordingS3Server{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		rec.mu.Lock()
		rec.requests = append(rec.requests, r)
		rec.mu.Unlock()

		w.Header().Set("ETag", `"deadbeef"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rec.Close)

	return rec
}

// lastRequest returns the most recent request the fake endpoint served.
func (r *recordingS3Server) lastRequest(t *testing.T) *http.Request {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	require.NotEmpty(t, r.requests, "the fake endpoint was never contacted")
	return r.requests[len(r.requests)-1]
}

// newFakeS3Storage builds an S3 backend pointed at an in-process fake endpoint.
// Anything needing a live bucket belongs in s3_integration_test.go.
func newFakeS3Storage(t *testing.T, rec *recordingS3Server) *S3Storage {
	t.Helper()

	s, err := NewS3Storage(&S3Config{
		Endpoint:        rec.URL,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		Bucket:          "test-bucket",
		Prefix:          "groxpi",
		ForcePathStyle:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func responseError(status int) error {
	return &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      errors.New("api error"),
	}
}

// TestS3Storage_NotFoundIsSentinel pins the translation from the SDK's error
// shapes to the shared sentinel. Without it TieredStorage cannot tell an absent
// object from a broken bucket.
func TestS3Storage_NotFoundIsSentinel(t *testing.T) {
	const key = "packages/numpy/numpy-1.26.0.tar.gz"

	t.Run("NoSuchKey", func(t *testing.T) {
		err := s3Error(&types.NoSuchKey{}, key)
		require.ErrorIs(t, err, ErrNotFound)
		assert.Contains(t, err.Error(), key)
	})

	t.Run("NotFound from HeadObject", func(t *testing.T) {
		require.ErrorIs(t, s3Error(&types.NotFound{}, key), ErrNotFound)
	})

	t.Run("bare 404 response", func(t *testing.T) {
		require.ErrorIs(t, s3Error(responseError(http.StatusNotFound), key), ErrNotFound)
	})

	t.Run("other failures are not misses", func(t *testing.T) {
		err := s3Error(responseError(http.StatusForbidden), key)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotFound)
	})

	t.Run("transport failures are not misses", func(t *testing.T) {
		err := s3Error(errors.New("dial tcp: connection refused"), key)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotFound)
	})
}

// TestS3Storage_Capabilities pins which capabilities S3 claims. S3 objects are
// not local files, so zero-copy is impossible and must not be advertised.
func TestS3Storage_Capabilities(t *testing.T) {
	var backend Storage = newFakeS3Storage(t, newRecordingS3Server(t))

	_, isZeroCopy := backend.(ZeroCopyCapable)
	assert.False(t, isZeroCopy, "S3Storage must not advertise ZeroCopyCapable")
}

// TestS3Storage_PathStyleAddressing pins that the bucket is addressed in the
// path rather than as a virtual host, which is what MinIO requires.
func TestS3Storage_PathStyleAddressing(t *testing.T) {
	rec := newRecordingS3Server(t)
	s := newFakeS3Storage(t, rec)

	_, err := s.Put(context.Background(), "packages/x/x-1.0.tar.gz",
		bytes.NewReader([]byte("payload")), 7, "application/gzip")
	require.NoError(t, err)

	assert.Equal(t, "/test-bucket/groxpi/packages/x/x-1.0.tar.gz", rec.lastRequest(t).URL.Path)
}

// TestS3Storage_NoChecksumTrailers pins the MinIO compatibility setting: older
// releases reject the aws-chunked checksum trailers the SDK now sends by
// default, so a put must carry none.
func TestS3Storage_NoChecksumTrailers(t *testing.T) {
	rec := newRecordingS3Server(t)
	s := newFakeS3Storage(t, rec)

	_, err := s.Put(context.Background(), "packages/x/x-1.0.tar.gz",
		bytes.NewReader([]byte("payload")), 7, "application/gzip")
	require.NoError(t, err)

	put := rec.lastRequest(t)
	assert.Empty(t, put.Header.Get("X-Amz-Trailer"), "checksum trailer must not be declared")
	assert.NotContains(t, strings.ToLower(put.Header.Get("Content-Encoding")), "aws-chunked")
	for name := range put.Header {
		assert.NotContains(t, strings.ToLower(name), "x-amz-checksum-",
			"no checksum header may be sent when the server does not require one")
	}
}

// fakeBucket is an in-process stand-in for a bucket that answers the two
// operations a prefix delete needs: a paginated listing and a batch delete.
// Everything else answers 200, which is enough for the SDK's bucket check.
type fakeBucket struct {
	*httptest.Server

	// pageSize truncates listings so the paginating path is exercised.
	pageSize int

	mu      sync.Mutex
	objects map[string]bool
}

func newFakeBucket(t *testing.T, pageSize int, keys ...string) *fakeBucket {
	t.Helper()

	b := &fakeBucket{pageSize: pageSize, objects: make(map[string]bool, len(keys))}
	for _, key := range keys {
		b.objects[key] = true
	}

	b.Server = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.Close)

	return b
}

func (b *fakeBucket) serve(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	switch {
	case query.Get("list-type") == "2":
		b.list(w, query)
	case r.Method == http.MethodPost && query.Has("delete"):
		b.delete(w, r)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// list answers with at most pageSize keys under the requested prefix, in sorted
// order, continuing from the supplied token.
func (b *fakeBucket) list(w http.ResponseWriter, query url.Values) {
	b.mu.Lock()
	matching := make([]string, 0, len(b.objects))
	for key := range b.objects {
		if strings.HasPrefix(key, query.Get("prefix")) && key > query.Get("continuation-token") {
			matching = append(matching, key)
		}
	}
	b.mu.Unlock()
	sort.Strings(matching)

	truncated := len(matching) > b.pageSize
	if truncated {
		matching = matching[:b.pageSize]
	}

	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	fmt.Fprintf(&body, `<IsTruncated>%t</IsTruncated>`, truncated)
	for _, key := range matching {
		fmt.Fprintf(&body, `<Contents><Key>%s</Key><Size>1</Size></Contents>`, key)
	}
	if truncated {
		fmt.Fprintf(&body, `<NextContinuationToken>%s</NextContinuationToken>`, matching[len(matching)-1])
	}
	body.WriteString(`</ListBucketResult>`)

	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, body.String())
}

func (b *fakeBucket) delete(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Objects []struct{ Key string } `xml:"Object"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	b.mu.Lock()
	for _, object := range request.Objects {
		delete(b.objects, object.Key)
	}
	b.mu.Unlock()

	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
		`<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></DeleteResult>`)
}

func (b *fakeBucket) remaining() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	keys := make([]string, 0, len(b.objects))
	for key := range b.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

// TestS3Storage_DeletePrefix pins that evicting a package reaches the object
// store. Without it, pure-S3 deployments answered an admin eviction with 200
// having deleted nothing: the transport only evicts through PrefixDeleter, and
// S3Storage did not implement it.
func TestS3Storage_DeletePrefix(t *testing.T) {
	// Two pages' worth of one package, plus a package whose name merely starts
	// with the evicted one, plus an unrelated one.
	bucket := newFakeBucket(t, 2,
		"groxpi/packages/numpy/numpy-1.0.tar.gz",
		"groxpi/packages/numpy/numpy-2.0.tar.gz",
		"groxpi/packages/numpy/numpy-3.0.tar.gz",
		"groxpi/packages/numpy-stubs/numpy-stubs-1.0.tar.gz",
		"groxpi/packages/pandas/pandas-2.0.tar.gz",
	)

	s, err := NewS3Storage(&S3Config{
		Endpoint:        bucket.URL,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		Bucket:          "test-bucket",
		Prefix:          "groxpi",
		ForcePathStyle:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	deleter, ok := any(s).(PrefixDeleter)
	require.True(t, ok, "S3Storage must be able to evict a package prefix")

	deleted, err := deleter.DeletePrefix(context.Background(), "packages/numpy/")
	require.NoError(t, err)
	assert.Equal(t, 3, deleted, "every page of the prefix must be deleted")

	assert.Equal(t, []string{
		"groxpi/packages/numpy-stubs/numpy-stubs-1.0.tar.gz",
		"groxpi/packages/pandas/pandas-2.0.tar.gz",
	}, bucket.remaining(), "prefix delete removed the wrong objects")

	// A prefix that matches nothing is not an error.
	deleted, err = deleter.DeletePrefix(context.Background(), "packages/absent/")
	require.NoError(t, err)
	assert.Zero(t, deleted)
}

// TestS3Storage_ExistsSurvivesAnotherCallersCancellation pins that the
// deduplicated lookup belongs to no single caller. The flight used to run on the
// context of whichever caller started it, so one client disconnecting failed
// every other caller waiting on the same key with context.Canceled - which the
// read path reads as a miss and re-downloads a file that is in the bucket.
func TestS3Storage_ExistsSurvivesAnotherCallersCancellation(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var heads atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The bucket check at construction time is also a HEAD; only object heads
		// are the ones under test.
		if r.Method != http.MethodHead || !strings.Contains(r.URL.Path, "/packages/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if heads.Add(1) == 1 {
			close(arrived)
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s, err := NewS3Storage(&S3Config{
		Endpoint:        server.URL,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		Bucket:          "test-bucket",
		ForcePathStyle:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	const key = "packages/numpy/numpy-1.26.0.tar.gz"

	// Caller A starts the flight and then goes away, as a client that hung up
	// mid-request does.
	ctxA, cancelA := context.WithCancel(context.Background())
	go func() { _, _ = s.Exists(ctxA, key) }()
	<-arrived

	// Caller B joins A's in-flight lookup: A cannot have finished, it is parked in
	// the handler.
	type answer struct {
		exists bool
		err    error
	}
	answers := make(chan answer, 1)
	go func() {
		exists, err := s.Exists(context.Background(), key)
		answers <- answer{exists, err}
	}()

	// Give B time to join the flight, then take A away and let the lookup finish.
	time.Sleep(50 * time.Millisecond)
	cancelA()
	close(release)

	got := <-answers
	require.NoError(t, got.err, "a second caller was failed by the first one disconnecting")
	assert.True(t, got.exists, "the object is in the bucket and must not be reported absent")
	assert.Equal(t, int64(1), heads.Load(), "the lookup must still be deduplicated")
}

// TestS3Storage_ContextIsHonoured is a cheap guard that read paths propagate a
// dead context rather than blocking.
func TestS3Storage_ContextIsHonoured(t *testing.T) {
	s := newFakeS3Storage(t, newRecordingS3Server(t))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Exists(ctx, "whatever")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound)
}
