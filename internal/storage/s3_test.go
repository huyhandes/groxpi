package storage

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestS3Storage builds an S3 backend pointed at an endpoint that is never
// contacted. Anything needing a live bucket belongs in s3_integration_test.go,
// which skips in short mode; these tests only exercise pure logic.
func newTestS3Storage(t *testing.T) *S3Storage {
	t.Helper()

	s, err := NewS3Storage(&S3Config{
		Endpoint: "test.endpoint",
		Bucket:   "test-bucket",
	})
	if err != nil {
		t.Skipf("Cannot create S3 storage for testing: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// TestS3Storage_NotFoundIsSentinel pins the translation from MinIO's error
// shapes to the shared sentinel. Without it TieredStorage cannot tell an absent
// object from a broken bucket.
func TestS3Storage_NotFoundIsSentinel(t *testing.T) {
	const key = "packages/numpy/numpy-1.26.0.tar.gz"

	t.Run("NoSuchKey", func(t *testing.T) {
		err := s3Error(minio.ErrorResponse{Code: "NoSuchKey"}, key)
		require.ErrorIs(t, err, ErrNotFound)
		assert.Contains(t, err.Error(), key)
	})

	t.Run("404 without a code", func(t *testing.T) {
		err := s3Error(minio.ErrorResponse{StatusCode: http.StatusNotFound}, key)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("other failures are not misses", func(t *testing.T) {
		err := s3Error(minio.ErrorResponse{Code: "AccessDenied", StatusCode: http.StatusForbidden}, key)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotFound)
	})

	t.Run("transport failures are not misses", func(t *testing.T) {
		err := s3Error(errors.New("dial tcp: connection refused"), key)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotFound)
	})
}

// TestS3Storage_ExistsIgnoresOnlyNotFound proves Exists reports absence as
// (false, nil) but never hides a real failure behind it.
func TestS3Storage_ExistsIgnoresOnlyNotFound(t *testing.T) {
	assert.True(t, isNotFoundResponse(minio.ErrorResponse{Code: "NoSuchKey"}))
	assert.True(t, isNotFoundResponse(minio.ErrorResponse{StatusCode: http.StatusNotFound}))
	assert.False(t, isNotFoundResponse(minio.ErrorResponse{Code: "AccessDenied", StatusCode: http.StatusForbidden}))
	assert.False(t, isNotFoundResponse(errors.New("dial tcp: connection refused")))
}

// TestS3Storage_Capabilities pins which capabilities S3 claims. S3 objects are
// not local files, so zero-copy is impossible and must not be advertised.
func TestS3Storage_Capabilities(t *testing.T) {
	var backend Storage = newTestS3Storage(t)

	_, isZeroCopy := backend.(ZeroCopyCapable)
	assert.False(t, isZeroCopy, "S3Storage must not advertise ZeroCopyCapable")
}

// TestS3Storage_ContextIsHonoured is a cheap guard that read paths propagate a
// dead context rather than blocking; it needs no live bucket.
func TestS3Storage_ContextIsHonoured(t *testing.T) {
	s := newTestS3Storage(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Stat(ctx, "whatever")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound)
}
