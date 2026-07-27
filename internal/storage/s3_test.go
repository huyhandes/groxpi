package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
