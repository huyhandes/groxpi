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
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
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

// apiError builds what the SDK hands callers for a modelled S3 error: an API
// error code wrapped in the HTTP response it arrived on.
func apiError(code string, status int) error {
	return &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      &smithy.GenericAPIError{Code: code, Message: code},
	}
}

// TestS3Storage_NotFoundIsSentinel pins the translation from the SDK's error
// shapes to the shared sentinel. Without it the store cannot tell an absent
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

	// A bodyless 404 is what HeadObject answers with; the SDK derives the error
	// code from the status line alone.
	t.Run("bodyless 404 carrying a NotFound code", func(t *testing.T) {
		require.ErrorIs(t, s3Error(apiError("NotFound", http.StatusNotFound), key), ErrNotFound)
	})

	// A deleted or misnamed bucket also answers 404. Reading that as "the object
	// is absent" turns a broken configuration into a silent permanent 100% miss:
	// every request re-downloads from upstream and every write is discarded.
	t.Run("NoSuchBucket is not a miss", func(t *testing.T) {
		err := s3Error(apiError("NoSuchBucket", http.StatusNotFound), key)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotFound)
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

// TestS3Storage_PathStyleAddressing pins that the bucket is addressed in the
// path rather than as a virtual host, which is what MinIO requires.
func TestS3Storage_PathStyleAddressing(t *testing.T) {
	rec := newRecordingS3Server(t)
	s := newFakeS3Storage(t, rec)

	err := s.Put(context.Background(), "packages/x/x-1.0.tar.gz",
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

	err := s.Put(context.Background(), "packages/x/x-1.0.tar.gz",
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
// store, page by page, and only under the prefix.
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

	deleted, err := s.DeletePrefix(context.Background(), "packages/numpy/")
	require.NoError(t, err)
	assert.Equal(t, 3, deleted, "every page of the prefix must be deleted")

	assert.Equal(t, []string{
		"groxpi/packages/numpy-stubs/numpy-stubs-1.0.tar.gz",
		"groxpi/packages/pandas/pandas-2.0.tar.gz",
	}, bucket.remaining(), "prefix delete removed the wrong objects")

	// A prefix that matches nothing is not an error.
	deleted, err = s.DeletePrefix(context.Background(), "packages/absent/")
	require.NoError(t, err)
	assert.Zero(t, deleted)
}
