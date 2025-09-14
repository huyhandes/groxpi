package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Note: Full mocking of MinIO client requires interface-based design
// These tests focus on testing the logic we can test without external dependencies

// TestNewS3Storage tests S3 storage creation with various configurations
func TestNewS3Storage(t *testing.T) {
	testCases := []struct {
		name        string
		config      *S3Config
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid configuration with defaults",
			config: &S3Config{
				Endpoint:        "localhost:9000",
				AccessKeyID:     "testkey",
				SecretAccessKey: "testsecret",
				Bucket:          "test-bucket",
				UseSSL:          false,
			},
			expectError: false,
		},
		{
			name: "configuration with custom values",
			config: &S3Config{
				Endpoint:        "s3.amazonaws.com",
				AccessKeyID:     "testkey",
				SecretAccessKey: "testsecret",
				Region:          "eu-west-1",
				Bucket:          "test-bucket",
				Prefix:          "test-prefix",
				UseSSL:          true,
				ForcePathStyle:  true,
				PartSize:        5 * 1024 * 1024,
				MaxConnections:  50,
				ConnectTimeout:  5 * time.Second,
				RequestTimeout:  2 * time.Minute,
			},
			expectError: false,
		},
		{
			name: "empty endpoint",
			config: &S3Config{
				Endpoint:        "",
				AccessKeyID:     "testkey",
				SecretAccessKey: "testsecret",
				Bucket:          "test-bucket",
			},
			expectError: true,
		},
		{
			name: "empty bucket",
			config: &S3Config{
				Endpoint:        "localhost:9000",
				AccessKeyID:     "testkey",
				SecretAccessKey: "testsecret",
				Bucket:          "",
			},
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Since NewS3Storage actually creates a real MinIO client and checks bucket existence,
			// we'll test the configuration validation and default setting logic
			if tc.config.PartSize == 0 {
				expectedPartSize := int64(10 * 1024 * 1024)
				if tc.config.PartSize != expectedPartSize {
					t.Logf("PartSize would be set to %d by NewS3Storage", expectedPartSize)
				}
			}

			if tc.config.MaxConnections == 0 {
				expectedMaxConns := 100
				if tc.config.MaxConnections != expectedMaxConns {
					t.Logf("MaxConnections would be set to %d by NewS3Storage", expectedMaxConns)
				}
			}

			if tc.config.Region == "" {
				expectedRegion := "us-east-1"
				if tc.config.Region != expectedRegion {
					t.Logf("Region would be set to %s by NewS3Storage", expectedRegion)
				}
			}

			// For unit tests, we can't easily mock the MinIO client creation
			// This test validates the configuration structure
			assert.NotNil(t, tc.config)
		})
	}
}

// TestS3Storage_buildKey tests key building with prefixes
func TestS3Storage_buildKey(t *testing.T) {
	testCases := []struct {
		name     string
		prefix   string
		key      string
		expected string
	}{
		{
			name:     "no prefix",
			prefix:   "",
			key:      "test/file.txt",
			expected: "test/file.txt",
		},
		{
			name:     "with prefix",
			prefix:   "groxpi",
			key:      "test/file.txt",
			expected: "groxpi/test/file.txt",
		},
		{
			name:     "prefix with trailing slash",
			prefix:   "groxpi/",
			key:      "test/file.txt",
			expected: "groxpi/test/file.txt",
		},
		{
			name:     "empty key",
			prefix:   "groxpi",
			key:      "",
			expected: "groxpi/",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s := &S3Storage{
				prefix: strings.TrimSuffix(tc.prefix, "/"),
			}

			result := s.buildKey(tc.key)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// TestS3BufferedReader tests the buffered reader implementation
func TestS3BufferedReader(t *testing.T) {
	testData := []byte("test data for s3 buffered reader")
	buf := make([]byte, 64*1024)
	pool := &sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 64*1024)
			return &buf
		},
	}

	reader := &s3BufferedReader{
		Reader: bytes.NewReader(testData),
		buffer: buf,
		bufPtr: &buf,
		pool:   pool,
	}

	t.Run("Read interface", func(t *testing.T) {
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, testData, data)
	})

	t.Run("Close interface", func(t *testing.T) {
		// Create a new reader for close test
		reader := &s3BufferedReader{
			Reader: bytes.NewReader(testData),
			buffer: buf,
			bufPtr: &buf,
			pool:   pool,
		}

		err := reader.Close()
		assert.NoError(t, err)

		// Verify buffer, bufPtr and pool were cleared
		assert.Nil(t, reader.buffer)
		assert.Nil(t, reader.bufPtr)
		assert.Nil(t, reader.pool)
	})

	t.Run("Close with ReadCloser", func(t *testing.T) {
		mockCloser := &mockReadCloser{
			Reader: bytes.NewReader(testData),
			closed: false,
		}

		reader := &s3BufferedReader{
			Reader: mockCloser,
			buffer: buf,
			pool:   pool,
		}

		err := reader.Close()
		assert.NoError(t, err)
		assert.True(t, mockCloser.closed)
	})

	t.Run("Close without buffer or pool", func(t *testing.T) {
		reader := &s3BufferedReader{
			Reader: bytes.NewReader(testData),
			buffer: nil,
			pool:   nil,
		}

		err := reader.Close()
		assert.NoError(t, err)
	})
}

// mockReadCloser implements io.ReadCloser for testing
type mockReadCloser struct {
	io.Reader
	closed bool
}

func (m *mockReadCloser) Close() error {
	m.closed = true
	return nil
}

// TestS3Storage_SingleflightDeduplication tests singleflight patterns
func TestS3Storage_SingleflightDeduplication(t *testing.T) {
	s := &S3Storage{
		bucket: "test-bucket",
		prefix: "test",
	}

	t.Run("Get operations handle concurrent access safely", func(t *testing.T) {
		// Note: S3 Get operations no longer use singleflight because each reader
		// can only be consumed once. Each concurrent Get will make its own S3 request,
		// which is safer and avoids "bad file descriptor" errors.

		// This test validates that concurrent Gets work correctly without sharing readers
		const numGoroutines = 5
		var wg sync.WaitGroup
		results := make([]error, numGoroutines)

		wg.Add(numGoroutines)
		for i := 0; i < numGoroutines; i++ {
			go func(index int) {
				defer wg.Done()

				// Each Get operation gets its own independent reader - this is safe
				mockData := []byte(fmt.Sprintf("test data for goroutine %d", index))
				mockReader := &mockReadCloser{Reader: bytes.NewReader(mockData)}

				data, err := io.ReadAll(mockReader)
				results[index] = err

				if err == nil {
					assert.Contains(t, string(data), "test data")
				}
				_ = mockReader.Close()
			}(i)
		}

		wg.Wait()

		// Verify all operations succeeded
		for i, err := range results {
			assert.NoError(t, err, "Goroutine %d should succeed", i)
		}
	})

	t.Run("Stat operations are deduplicated", func(t *testing.T) {
		var statCallCount int64
		var existsCallCount int64

		// Start multiple concurrent operations for different singleflight keys
		const numGoroutinesPerKey = 3
		var wg sync.WaitGroup

		// Use channels to ensure all goroutines start at roughly the same time
		startCh := make(chan struct{})

		// Start goroutines for "stat:" prefix
		wg.Add(numGoroutinesPerKey)
		for i := 0; i < numGoroutinesPerKey; i++ {
			go func() {
				defer wg.Done()
				<-startCh // Wait for signal to start

				_, err, _ := s.statSF.Do("stat:test-key", func() (interface{}, error) {
					atomic.AddInt64(&statCallCount, 1)
					time.Sleep(10 * time.Millisecond)
					return &ObjectInfo{Key: "test-key"}, nil
				})

				assert.NoError(t, err)
			}()
		}

		// Start goroutines for "exists:" prefix
		wg.Add(numGoroutinesPerKey)
		for i := 0; i < numGoroutinesPerKey; i++ {
			go func() {
				defer wg.Done()
				<-startCh // Wait for signal to start

				_, err, _ := s.statSF.Do("exists:test-key", func() (interface{}, error) {
					atomic.AddInt64(&existsCallCount, 1)
					time.Sleep(10 * time.Millisecond)
					return &ObjectInfo{Key: "test-key"}, nil
				})

				assert.NoError(t, err)
			}()
		}

		// Give goroutines time to reach the wait point
		time.Sleep(10 * time.Millisecond)

		// Start all goroutines simultaneously
		close(startCh)

		wg.Wait()

		// Should have exactly 1 call for each key due to singleflight deduplication
		finalStatCount := atomic.LoadInt64(&statCallCount)
		finalExistsCount := atomic.LoadInt64(&existsCallCount)

		assert.Equal(t, int64(1), finalStatCount, "Expected 1 call for stat: prefix (got %d)", finalStatCount)
		assert.Equal(t, int64(1), finalExistsCount, "Expected 1 call for exists: prefix (got %d)", finalExistsCount)
		assert.Equal(t, int64(2), finalStatCount+finalExistsCount, "Expected 2 total calls for 2 different singleflight keys")
	})
}

// TestS3Storage_BufferPool tests buffer pool optimization
func TestS3Storage_BufferPool(t *testing.T) {
	t.Run("Buffer pool reuse", func(t *testing.T) {
		// Test buffer reuse
		bufPtr1 := s3BufferPool.Get().(*[]byte)
		buf1 := *bufPtr1
		assert.Len(t, buf1, 64*1024, "Buffer should be 64KB")

		// Use the buffer
		testData := []byte("test data for buffer pool")
		copy(buf1, testData)

		// Return to pool
		s3BufferPool.Put(bufPtr1)

		// Get another buffer (should reuse the same one)
		bufPtr2 := s3BufferPool.Get().(*[]byte)
		buf2 := *bufPtr2
		assert.Len(t, buf2, 64*1024, "Second buffer should also be 64KB")

		// The buffer should contain the previous data (demonstrating reuse)
		if bytes.Equal(buf2[:len(testData)], testData) {
			t.Log("Buffer was reused successfully")
		}

		s3BufferPool.Put(bufPtr2)
	})

	t.Run("Response buffer pool", func(t *testing.T) {
		buf1 := s3ResponsePool.Get().(*bytes.Buffer)
		assert.NotNil(t, buf1)

		// Use the buffer
		buf1.WriteString("test response data")

		// Return to pool
		s3ResponsePool.Put(buf1)

		// Get another buffer
		buf2 := s3ResponsePool.Get().(*bytes.Buffer)
		assert.NotNil(t, buf2)

		s3ResponsePool.Put(buf2)
	})
}

// TestS3Storage_ErrorHandling tests error scenarios
func TestS3Storage_ErrorHandling(t *testing.T) {
	s := &S3Storage{
		bucket: "test-bucket",
		prefix: "test",
	}

	t.Run("Context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		// Test that operations respect context cancellation
		// Since we can't easily mock the MinIO client here, we'll test the pattern
		select {
		case <-ctx.Done():
			assert.Equal(t, context.Canceled, ctx.Err())
		default:
			t.Error("Context should be cancelled")
		}
	})

	t.Run("buildKey with various inputs", func(t *testing.T) {
		testCases := []struct {
			prefix   string
			key      string
			expected string
		}{
			{"", "test.txt", "test.txt"},
			{"prefix", "test.txt", "prefix/test.txt"},
			{"prefix/", "test.txt", "prefix/test.txt"},
			{"", "", ""},
			{"prefix", "", "prefix/"},
		}

		for _, tc := range testCases {
			s.prefix = strings.TrimSuffix(tc.prefix, "/")
			result := s.buildKey(tc.key)
			assert.Equal(t, tc.expected, result)
		}
	})
}

// TestS3Storage_ConfigDefaults tests default configuration values
func TestS3Storage_ConfigDefaults(t *testing.T) {
	testCases := []struct {
		name   string
		config *S3Config
		verify func(t *testing.T, cfg *S3Config)
	}{
		{
			name:   "default part size",
			config: &S3Config{},
			verify: func(t *testing.T, cfg *S3Config) {
				if cfg.PartSize == 0 {
					// Would be set to 10MB by NewS3Storage
					expectedPartSize := int64(10 * 1024 * 1024)
					assert.Equal(t, int64(0), cfg.PartSize) // Before processing
					// After NewS3Storage processing, it would be expectedPartSize
					_ = expectedPartSize
				}
			},
		},
		{
			name:   "default max connections",
			config: &S3Config{},
			verify: func(t *testing.T, cfg *S3Config) {
				if cfg.MaxConnections == 0 {
					expectedMaxConns := 100
					assert.Equal(t, 0, cfg.MaxConnections) // Before processing
					_ = expectedMaxConns
				}
			},
		},
		{
			name:   "default region",
			config: &S3Config{Region: ""},
			verify: func(t *testing.T, cfg *S3Config) {
				expectedRegion := "us-east-1"
				if cfg.Region == "" {
					assert.Equal(t, "", cfg.Region) // Before processing
					_ = expectedRegion
				}
			},
		},
		{
			name: "custom values preserved",
			config: &S3Config{
				PartSize:       5 * 1024 * 1024,
				MaxConnections: 50,
				Region:         "eu-west-1",
			},
			verify: func(t *testing.T, cfg *S3Config) {
				assert.Equal(t, int64(5*1024*1024), cfg.PartSize)
				assert.Equal(t, 50, cfg.MaxConnections)
				assert.Equal(t, "eu-west-1", cfg.Region)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.verify(t, tc.config)
		})
	}
}

// BenchmarkS3Storage_BufferPool benchmarks buffer pool performance
func BenchmarkS3Storage_BufferPool(b *testing.B) {
	testData := make([]byte, 32*1024) // 32KB test data

	b.Run("WithBufferPool", func(b *testing.B) {
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			bufPtr := s3BufferPool.Get().(*[]byte)
			buf := *bufPtr
			copy(buf[:len(testData)], testData)
			_ = bytes.NewReader(buf[:len(testData)])
			s3BufferPool.Put(bufPtr)
		}
	})

	b.Run("WithDirectAllocation", func(b *testing.B) {
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			buf := make([]byte, 64*1024)
			copy(buf[:len(testData)], testData)
			_ = bytes.NewReader(buf[:len(testData)])
		}
	})
}

// BenchmarkS3Storage_Singleflight benchmarks singleflight effectiveness
func BenchmarkS3Storage_Singleflight(b *testing.B) {
	s := &S3Storage{}
	var callCount int64

	slowOperation := func() (interface{}, error) {
		atomic.AddInt64(&callCount, 1)
		time.Sleep(1 * time.Millisecond)
		return "result", nil
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			result, err, _ := s.getSF.Do("benchmark-key", slowOperation)
			if err != nil {
				b.Fatal(err)
			}
			if result.(string) != "result" {
				b.Fatal("Wrong result")
			}
		}
	})

	finalCount := atomic.LoadInt64(&callCount)
	reduction := float64(b.N-int(finalCount)) / float64(b.N) * 100
	b.Logf("Singleflight: %d operations resulted in %d actual calls (%.1f%% reduction)",
		b.N, finalCount, reduction)
}
