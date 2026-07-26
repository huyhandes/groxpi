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
		PartSize: 10 * 1024 * 1024,
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

// TestS3Storage_CalculateOptimalPartSize tests the part size calculation logic,
// including the degenerate sizes a caller can pass in.
func TestS3Storage_CalculateOptimalPartSize(t *testing.T) {
	tests := []struct {
		name        string
		fileSize    int64
		expectedMin int64
		expectedMax int64
		description string
	}{
		{
			name:        "small_file_10MB",
			fileSize:    10 * 1024 * 1024, // 10MB
			expectedMin: 5 * 1024 * 1024,  // 5MB minimum
			expectedMax: 32 * 1024 * 1024, // Should use default or small part size
			description: "Small files should use minimum viable part size",
		},
		{
			name:        "medium_file_50MB",
			fileSize:    50 * 1024 * 1024, // 50MB
			expectedMin: 5 * 1024 * 1024,  // 5MB minimum
			expectedMax: 32 * 1024 * 1024, // Should use default part size
			description: "Medium files should use default part size",
		},
		{
			name:        "large_file_500MB",
			fileSize:    500 * 1024 * 1024, // 500MB
			expectedMin: 10 * 1024 * 1024,  // Should be at least 10MB
			expectedMax: 64 * 1024 * 1024,  // Should scale up for better throughput
			description: "Large files should use larger part sizes for throughput",
		},
		{
			name:        "extra_large_file_5GB",
			fileSize:    5 * 1024 * 1024 * 1024, // 5GB
			expectedMin: 32 * 1024 * 1024,       // Should use larger parts
			expectedMax: 128 * 1024 * 1024,      // But not too large
			description: "Extra large files should balance part count vs throughput",
		},
		{
			name:        "huge_file_50GB",
			fileSize:    50 * 1024 * 1024 * 1024, // 50GB
			expectedMin: 64 * 1024 * 1024,        // Must be large enough to stay under 10k parts
			expectedMax: 256 * 1024 * 1024,       // But reasonable for memory usage
			description: "Huge files must respect 10,000 part limit",
		},
		{
			name:        "pyspark_size_317MB",
			fileSize:    317 * 1024 * 1024, // 317MB (real-world pyspark example)
			expectedMin: 10 * 1024 * 1024,  // At least 10MB
			expectedMax: 64 * 1024 * 1024,  // Should use optimized size
			description: "Real-world pyspark file should have optimized part size",
		},
		{
			name:        "zero_size",
			fileSize:    0,
			expectedMin: 5 * 1024 * 1024,
			expectedMax: 10 * 1024 * 1024,
			description: "Zero size should not crash, should return the smallest band",
		},
		{
			name:        "negative_size",
			fileSize:    -1,
			expectedMin: 5 * 1024 * 1024,
			expectedMax: 10 * 1024 * 1024,
			description: "Negative size should not crash, should return the smallest band",
		},
		{
			name:        "very_small_size_1KB",
			fileSize:    1024,
			expectedMin: 5 * 1024 * 1024,
			expectedMax: 10 * 1024 * 1024,
			description: "Very small files should use the smallest band",
		},
		{
			name:        "exact_aws_minimum_5MB",
			fileSize:    5 * 1024 * 1024,
			expectedMin: 5 * 1024 * 1024,
			expectedMax: 10 * 1024 * 1024,
			description: "Exact AWS minimum should work correctly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			partSize := calculateOptimalPartSize(tt.fileSize)

			// Verify part size is within expected range
			assert.GreaterOrEqual(t, partSize, tt.expectedMin,
				"Part size should be at least %d bytes for %s", tt.expectedMin, tt.description)
			assert.LessOrEqual(t, partSize, tt.expectedMax,
				"Part size should be at most %d bytes for %s", tt.expectedMax, tt.description)

			// Verify AWS S3 constraints
			assert.GreaterOrEqual(t, partSize, int64(5*1024*1024),
				"Part size must meet AWS S3 minimum of 5MB")

			// Verify part count doesn't exceed AWS limit
			partCount := (tt.fileSize + partSize - 1) / partSize // Ceiling division
			assert.LessOrEqual(t, partCount, int64(10000),
				"Part count (%d) must not exceed AWS S3 limit of 10,000 parts", partCount)

			t.Logf("File size: %dMB, Part size: %dMB, Part count: %d",
				tt.fileSize/(1024*1024), partSize/(1024*1024), partCount)
		})
	}
}
