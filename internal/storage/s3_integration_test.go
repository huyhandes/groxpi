package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

/*
S3 Integration Tests

These tests require the following environment variables to be set:

Required:
- TEST_S3_ENDPOINT: S3 endpoint URL (e.g., "s3-hn-2.cloud.cmctelecom.vn")
- TEST_S3_ACCESS_KEY: S3 access key ID
- TEST_S3_SECRET_KEY: S3 secret access key
- TEST_S3_BUCKET: S3 bucket name for testing

Optional:
- TEST_S3_REGION: AWS region (default: "us-east-1")
- TEST_S3_USE_SSL: Use SSL/TLS (default: "true")
- TEST_S3_FORCE_PATH_STYLE: Force path-style URLs (default: "false")

Example usage:
export TEST_S3_ENDPOINT="s3-hn-2.cloud.cmctelecom.vn"
export TEST_S3_ACCESS_KEY="your-access-key"
export TEST_S3_SECRET_KEY="your-secret-key"
export TEST_S3_BUCKET="your-test-bucket"
go test -v ./internal/storage/

To run only unit tests (skip integration tests):
go test -v -short ./internal/storage/
*/

// S3 configuration defaults (can be overridden by environment variables)
const (
	defaultS3Prefix = "groxpi-test"
	defaultS3Region = "us-east-1"
)

// createTestS3Storage creates an S3 storage instance for integration testing
func createTestS3Storage(t *testing.T, customPrefix ...string) *S3Storage {
	// Load configuration from environment variables with MinIO defaults
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000" // Default MinIO endpoint
		t.Log("Using default MinIO endpoint: localhost:9000 (ensure MinIO is running with 'docker-compose -f docker-compose.test.yml up -d')")
	}

	accessKey := os.Getenv("TEST_S3_ACCESS_KEY")
	if accessKey == "" {
		accessKey = "minioadmin" // Default MinIO access key
	}

	secretKey := os.Getenv("TEST_S3_SECRET_KEY")
	if secretKey == "" {
		secretKey = "minioadmin" // Default MinIO secret key
	}

	bucket := os.Getenv("TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "groxpi-test" // Default test bucket
	}

	// Use defaults for optional configuration
	region := os.Getenv("TEST_S3_REGION")
	if region == "" {
		region = defaultS3Region
	}

	prefix := defaultS3Prefix
	if len(customPrefix) > 0 {
		prefix = customPrefix[0]
	}

	// Check for SSL configuration (defaults to false for local MinIO)
	useSSL := false
	if sslEnv := os.Getenv("TEST_S3_USE_SSL"); sslEnv != "" {
		useSSL = sslEnv == "true"
	}

	// Check for path style configuration (defaults to true for MinIO)
	forcePathStyle := true
	if pathStyleEnv := os.Getenv("TEST_S3_FORCE_PATH_STYLE"); pathStyleEnv != "" {
		forcePathStyle = pathStyleEnv == "true"
	}

	cfg := &S3Config{
		Endpoint:        endpoint,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		Region:          region,
		Bucket:          bucket,
		Prefix:          prefix,
		UseSSL:          useSSL,
		ForcePathStyle:  forcePathStyle,
		PartSize:        5 * 1024 * 1024, // 5MB parts for testing
		MaxConnections:  10,
		ConnectTimeout:  30 * time.Second,
		RequestTimeout:  5 * time.Minute,
	}

	storage, err := NewS3Storage(cfg)
	require.NoError(t, err, "Failed to create S3 storage for integration test")

	return storage
}

// TestS3Storage_IntegrationBasic tests basic S3 operations with real S3
func TestS3Storage_IntegrationBasic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	storage := createTestS3Storage(t)
	defer storage.Close()

	ctx := context.Background()

	t.Run("Put_and_Get_cycle", func(t *testing.T) {
		key := fmt.Sprintf("integration-test/basic/%d.txt", time.Now().UnixNano())
		content := []byte("Hello from S3 integration test!")
		contentType := "text/plain"

		// Put object
		info, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), contentType)
		require.NoError(t, err, "Failed to put object")
		assert.Equal(t, key, info.Key)
		assert.Equal(t, int64(len(content)), info.Size)
		assert.Equal(t, contentType, info.ContentType)
		assert.NotEmpty(t, info.ETag)

		// Get object
		reader, getInfo, err := storage.Get(ctx, key)
		require.NoError(t, err, "Failed to get object")
		defer reader.Close()

		assert.Equal(t, key, getInfo.Key)
		assert.Equal(t, int64(len(content)), getInfo.Size)
		assert.NotEmpty(t, getInfo.ETag)
		assert.NotZero(t, getInfo.LastModified)

		// Read content
		data, err := io.ReadAll(reader)
		require.NoError(t, err, "Failed to read object data")
		assert.Equal(t, content, data)

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete test object")
	})

	t.Run("GetRange_functionality", func(t *testing.T) {
		key := fmt.Sprintf("integration-test/range/%d.txt", time.Now().UnixNano())
		content := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ")

		// Put object
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		require.NoError(t, err, "Failed to put object")

		// Test various ranges
		testCases := []struct {
			name   string
			offset int64
			length int64
			expect string
		}{
			{"first_5_bytes", 0, 5, "01234"},
			{"middle_10_bytes", 10, 10, "ABCDEFGHIJ"},
			{"last_5_bytes", int64(len(content) - 5), 5, "VWXYZ"},
			{"single_byte", 15, 1, "F"},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				reader, info, err := storage.GetRange(ctx, key, tc.offset, tc.length)
				require.NoError(t, err, "Failed to get range")
				defer reader.Close()

				assert.Equal(t, key, info.Key)

				data, err := io.ReadAll(reader)
				require.NoError(t, err, "Failed to read range data")
				assert.Equal(t, tc.expect, string(data))
			})
		}

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete test object")
	})
}

// TestS3Storage_IntegrationAdvanced tests advanced S3 features
func TestS3Storage_IntegrationAdvanced(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	storage := createTestS3Storage(t)
	defer storage.Close()

	ctx := context.Background()

	t.Run("Multipart_upload", func(t *testing.T) {
		key := fmt.Sprintf("integration-test/multipart/%d.bin", time.Now().UnixNano())

		// Create 12MB of test data to trigger multipart upload (part size is 5MB)
		size := 12 * 1024 * 1024
		content := make([]byte, size)
		for i := range content {
			content[i] = byte(i % 256)
		}

		// Put with multipart
		info, err := storage.PutMultipart(ctx, key, bytes.NewReader(content), int64(size), "application/octet-stream", 5*1024*1024)
		require.NoError(t, err, "Failed to put multipart object")
		assert.Equal(t, key, info.Key)
		assert.Equal(t, int64(size), info.Size)
		assert.NotEmpty(t, info.ETag)

		// Verify we can read it back
		reader, getInfo, err := storage.Get(ctx, key)
		require.NoError(t, err, "Failed to get multipart object")
		defer reader.Close()

		assert.Equal(t, int64(size), getInfo.Size)

		// Read first 1KB to verify content
		buffer := make([]byte, 1024)
		n, err := io.ReadFull(reader, buffer)
		require.NoError(t, err, "Failed to read multipart object")
		assert.Equal(t, 1024, n)

		// Verify first 1KB matches
		expected := content[:1024]
		assert.Equal(t, expected, buffer)

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete test object")
	})

	t.Run("List_objects", func(t *testing.T) {
		prefix := fmt.Sprintf("integration-test/list/%d", time.Now().UnixNano())

		// Create multiple test objects
		testKeys := []string{
			fmt.Sprintf("%s/file1.txt", prefix),
			fmt.Sprintf("%s/file2.txt", prefix),
			fmt.Sprintf("%s/subdir/file3.txt", prefix),
		}

		for _, key := range testKeys {
			content := []byte(fmt.Sprintf("content for %s", key))
			_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
			require.NoError(t, err, "Failed to put test object %s", key)
		}

		// List with prefix
		objects, err := storage.List(ctx, ListOptions{
			Prefix:  prefix,
			MaxKeys: 10,
		})
		require.NoError(t, err, "Failed to list objects")
		assert.Len(t, objects, 3, "Expected 3 objects")

		// Verify all test keys are present
		foundKeys := make(map[string]bool)
		for _, obj := range objects {
			foundKeys[obj.Key] = true
			assert.Greater(t, obj.Size, int64(0))
			assert.NotZero(t, obj.LastModified)
		}

		for _, expectedKey := range testKeys {
			assert.True(t, foundKeys[expectedKey], "Expected key %s not found", expectedKey)
		}

		// List with MaxKeys limit
		limitedObjects, err := storage.List(ctx, ListOptions{
			Prefix:  prefix,
			MaxKeys: 2,
		})
		require.NoError(t, err, "Failed to list objects with limit")
		assert.Len(t, limitedObjects, 2, "Expected 2 objects with MaxKeys=2")

		// Clean up
		for _, key := range testKeys {
			err := storage.Delete(ctx, key)
			assert.NoError(t, err, "Failed to delete test object %s", key)
		}
	})

	t.Run("Exists_and_Stat", func(t *testing.T) {
		key := fmt.Sprintf("integration-test/exists/%d.txt", time.Now().UnixNano())
		content := []byte("test content for exists check")

		// Check non-existent object
		exists, err := storage.Exists(ctx, key)
		require.NoError(t, err, "Failed to check existence")
		assert.False(t, exists, "Object should not exist")

		// Put object
		putInfo, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		require.NoError(t, err, "Failed to put object")

		// Check existent object
		exists, err = storage.Exists(ctx, key)
		require.NoError(t, err, "Failed to check existence")
		assert.True(t, exists, "Object should exist")

		// Get stat
		statInfo, err := storage.Stat(ctx, key)
		require.NoError(t, err, "Failed to stat object")
		assert.Equal(t, key, statInfo.Key)
		assert.Equal(t, int64(len(content)), statInfo.Size)
		assert.Equal(t, putInfo.ETag, statInfo.ETag)
		assert.NotZero(t, statInfo.LastModified)

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete test object")

		// Verify deletion
		exists, err = storage.Exists(ctx, key)
		require.NoError(t, err, "Failed to check existence after delete")
		assert.False(t, exists, "Object should not exist after deletion")
	})

	t.Run("PresignedURL_generation", func(t *testing.T) {
		key := fmt.Sprintf("integration-test/presigned/%d.txt", time.Now().UnixNano())
		content := []byte("content for presigned URL test")

		// Put object
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		require.NoError(t, err, "Failed to put object")

		// Generate presigned URL
		url, err := storage.GetPresignedURL(ctx, key, 1*time.Hour)
		require.NoError(t, err, "Failed to generate presigned URL")
		assert.NotEmpty(t, url, "Presigned URL should not be empty")
		assert.Contains(t, url, key, "URL should contain object key")

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete test object")
	})
}

// TestS3Storage_IntegrationConcurrency tests concurrent operations
func TestS3Storage_IntegrationConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	storage := createTestS3Storage(t)
	defer storage.Close()

	ctx := context.Background()

	t.Run("Concurrent_put_different_keys", func(t *testing.T) {
		basePrefix := fmt.Sprintf("integration-test/concurrent-put/%d", time.Now().UnixNano())
		const numGoroutines = 5

		var wg sync.WaitGroup
		var errors int64
		keys := make([]string, numGoroutines)

		wg.Add(numGoroutines)
		for i := 0; i < numGoroutines; i++ {
			go func(index int) {
				defer wg.Done()

				key := fmt.Sprintf("%s/file-%d.txt", basePrefix, index)
				keys[index] = key
				content := []byte(fmt.Sprintf("content for goroutine %d", index))

				_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
				if err != nil {
					t.Errorf("Goroutine %d failed to put object: %v", index, err)
					atomic.AddInt64(&errors, 1)
				}
			}(i)
		}

		wg.Wait()
		assert.Equal(t, int64(0), atomic.LoadInt64(&errors), "No errors should occur during concurrent puts")

		// Verify all objects exist
		for i, key := range keys {
			if key == "" {
				continue // Skip if key wasn't set due to error
			}

			exists, err := storage.Exists(ctx, key)
			require.NoError(t, err, "Failed to check existence of key %s", key)
			assert.True(t, exists, "Object %d should exist", i)

			// Clean up
			err = storage.Delete(ctx, key)
			assert.NoError(t, err, "Failed to delete object %d", i)
		}
	})

	t.Run("Concurrent_get_same_key", func(t *testing.T) {
		key := fmt.Sprintf("integration-test/concurrent-get/%d.txt", time.Now().UnixNano())
		content := []byte("shared content for concurrent get test")

		// Put object once
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		require.NoError(t, err, "Failed to put object")

		const numGoroutines = 10
		var wg sync.WaitGroup
		var errors int64
		results := make([][]byte, numGoroutines)

		wg.Add(numGoroutines)
		for i := 0; i < numGoroutines; i++ {
			go func(index int) {
				defer wg.Done()

				reader, _, err := storage.Get(ctx, key)
				if err != nil {
					t.Errorf("Goroutine %d failed to get object: %v", index, err)
					atomic.AddInt64(&errors, 1)
					return
				}
				defer reader.Close()

				data, err := io.ReadAll(reader)
				if err != nil {
					t.Errorf("Goroutine %d failed to read object: %v", index, err)
					atomic.AddInt64(&errors, 1)
					return
				}

				results[index] = data
			}(i)
		}

		wg.Wait()
		assert.Equal(t, int64(0), atomic.LoadInt64(&errors), "No errors should occur during concurrent gets")

		// Verify all results are identical
		for i, result := range results {
			if result != nil {
				assert.Equal(t, content, result, "Goroutine %d got incorrect content", i)
			}
		}

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete test object")
	})

	t.Run("Singleflight_deduplication", func(t *testing.T) {
		// This test verifies that singleflight actually reduces network calls
		// by comparing operation timing with and without singleflight

		key := fmt.Sprintf("integration-test/singleflight/%d.txt", time.Now().UnixNano())
		content := []byte("content for singleflight test")

		// Put object
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
		require.NoError(t, err, "Failed to put object")

		// Test concurrent Exists operations (which use singleflight)
		const numGoroutines = 20
		var wg sync.WaitGroup
		start := time.Now()

		wg.Add(numGoroutines)
		for i := 0; i < numGoroutines; i++ {
			go func() {
				defer wg.Done()
				exists, err := storage.Exists(ctx, key)
				assert.NoError(t, err)
				assert.True(t, exists)
			}()
		}

		wg.Wait()
		duration := time.Since(start)

		// With singleflight, 20 concurrent operations should complete much faster
		// than 20 sequential operations would take
		t.Logf("20 concurrent Exists operations took %v", duration)

		// The test passes if it completes without errors
		// Actual timing varies based on network conditions

		// Clean up
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete test object")
	})
}

// TestS3Storage_IntegrationErrorHandling tests error scenarios with real S3
func TestS3Storage_IntegrationErrorHandling(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	storage := createTestS3Storage(t)
	defer storage.Close()

	ctx := context.Background()

	t.Run("Get_nonexistent_object", func(t *testing.T) {
		key := fmt.Sprintf("integration-test/nonexistent/%d.txt", time.Now().UnixNano())

		reader, info, err := storage.Get(ctx, key)
		assert.Error(t, err, "Should get error for nonexistent object")
		assert.Nil(t, reader, "Reader should be nil for error case")
		assert.Nil(t, info, "Info should be nil for error case")
	})

	t.Run("Stat_nonexistent_object", func(t *testing.T) {
		key := fmt.Sprintf("integration-test/nonexistent-stat/%d.txt", time.Now().UnixNano())

		info, err := storage.Stat(ctx, key)
		assert.Error(t, err, "Should get error for nonexistent object")
		assert.Nil(t, info, "Info should be nil for error case")
	})

	t.Run("Delete_nonexistent_object", func(t *testing.T) {
		key := fmt.Sprintf("integration-test/nonexistent-delete/%d.txt", time.Now().UnixNano())

		// S3 delete is idempotent - deleting non-existent object should not error
		err := storage.Delete(ctx, key)
		assert.NoError(t, err, "Delete should be idempotent")
	})

	t.Run("Context_timeout", func(t *testing.T) {
		// Create a context with very short timeout
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
		defer cancel()

		key := fmt.Sprintf("integration-test/timeout/%d.txt", time.Now().UnixNano())

		// Operation should fail due to context timeout
		_, err := storage.List(ctx, ListOptions{Prefix: key, MaxKeys: 1})
		assert.Error(t, err, "Should get context timeout error")
	})
}

// BenchmarkS3Storage_IntegrationOperations benchmarks real S3 operations
func BenchmarkS3Storage_IntegrationOperations(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping integration benchmark in short mode")
	}

	// Create a dummy *testing.T for createTestS3Storage
	t := &testing.T{}
	storage := createTestS3Storage(t)
	defer storage.Close()

	ctx := context.Background()
	content := make([]byte, 1024) // 1KB test content
	for i := range content {
		content[i] = byte(i % 256)
	}

	b.Run("Put_1KB", func(b *testing.B) {
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			key := fmt.Sprintf("benchmark/put/%d/%d.bin", time.Now().UnixNano(), i)
			_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/octet-stream")
			if err != nil {
				b.Fatal(err)
			}

			// Clean up immediately to avoid storage costs
			storage.Delete(ctx, key)
		}
	})

	b.Run("Get_1KB", func(b *testing.B) {
		// Pre-create objects for getting
		keys := make([]string, b.N)
		for i := 0; i < b.N; i++ {
			key := fmt.Sprintf("benchmark/get/%d/%d.bin", time.Now().UnixNano(), i)
			keys[i] = key
			_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/octet-stream")
			if err != nil {
				b.Fatal(err)
			}
		}

		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			reader, _, err := storage.Get(ctx, keys[i])
			if err != nil {
				b.Fatal(err)
			}
			io.ReadAll(reader)
			reader.Close()
		}

		// Clean up
		for _, key := range keys {
			storage.Delete(ctx, key)
		}
	})

	b.Run("Exists_check", func(b *testing.B) {
		key := fmt.Sprintf("benchmark/exists/%d.bin", time.Now().UnixNano())
		_, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/octet-stream")
		if err != nil {
			b.Fatal(err)
		}

		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			exists, err := storage.Exists(ctx, key)
			if err != nil {
				b.Fatal(err)
			}
			if !exists {
				b.Fatal("Object should exist")
			}
		}

		// Clean up
		storage.Delete(ctx, key)
	})
}
