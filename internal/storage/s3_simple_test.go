package storage

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/singleflight"
)

// TestSingleflightPatternBasic tests the basic singleflight pattern used in S3Storage
func TestSingleflightPatternBasic(t *testing.T) {
	var sf singleflight.Group
	var callCount int64

	// Simulate a slow operation that we want to deduplicate
	slowOperation := func() (interface{}, error) {
		atomic.AddInt64(&callCount, 1)
		time.Sleep(10 * time.Millisecond) // Simulate network delay
		return "result", nil
	}

	// Start multiple concurrent operations
	const numGoroutines = 10
	var wg sync.WaitGroup
	results := make([]string, numGoroutines)

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(index int) {
			defer wg.Done()

			result, err, _ := sf.Do("test-key", slowOperation)
			require.NoError(t, err)
			results[index] = result.(string)
		}(i)
	}

	wg.Wait()

	// Verify all got the same result
	for i, result := range results {
		assert.Equal(t, "result", result, "Goroutine %d got wrong result", i)
	}

	// Verify singleflight worked - should only have 1 actual call despite 10 requests
	finalCount := atomic.LoadInt64(&callCount)
	assert.Equal(t, int64(1), finalCount, "Expected singleflight to reduce concurrent calls to 1")
}

// TestSingleflightPatternDifferentKeys tests that different keys don't interfere
func TestSingleflightPatternDifferentKeys(t *testing.T) {
	var sf singleflight.Group
	var callCount int64

	// Simulate operations with different keys
	slowOperation := func(key string) func() (interface{}, error) {
		return func() (interface{}, error) {
			atomic.AddInt64(&callCount, 1)
			time.Sleep(10 * time.Millisecond)
			return "result-" + key, nil
		}
	}

	// Start concurrent operations for different keys
	const numGoroutines = 10
	var wg sync.WaitGroup
	results := make([]string, numGoroutines)

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		key := "key1"
		if i%2 == 1 {
			key = "key2"
		}

		go func(index int, opKey string) {
			defer wg.Done()

			result, err, _ := sf.Do(opKey, slowOperation(opKey))
			require.NoError(t, err)
			results[index] = result.(string)
		}(i, key)
	}

	wg.Wait()

	// Verify results are correct for each key
	for i, result := range results {
		if i%2 == 0 {
			assert.Equal(t, "result-key1", result, "Goroutine %d got wrong result", i)
		} else {
			assert.Equal(t, "result-key2", result, "Goroutine %d got wrong result", i)
		}
	}

	// Verify singleflight worked - should have 2 actual calls for 2 different keys
	finalCount := atomic.LoadInt64(&callCount)
	assert.Equal(t, int64(2), finalCount, "Expected singleflight to make 2 calls for 2 different keys")
}

// TestBufferPoolOptimization tests the buffer pool pattern used in S3Storage
func TestBufferPoolOptimization(t *testing.T) {
	pool := sync.Pool{
		New: func() interface{} {
			return make([]byte, 64*1024) // 64KB buffers
		},
	}

	// Test buffer reuse
	buf1 := pool.Get().([]byte)
	assert.Len(t, buf1, 64*1024, "Buffer should be 64KB")

	// Use the buffer
	testData := []byte("test data for buffer pool")
	copy(buf1, testData)

	// Return to pool
	pool.Put(buf1)

	// Get another buffer (should reuse the same one)
	buf2 := pool.Get().([]byte)
	assert.Len(t, buf2, 64*1024, "Second buffer should also be 64KB")

	// The buffer should contain the previous data (demonstrating reuse)
	if bytes.Equal(buf2[:len(testData)], testData) {
		t.Log("Buffer was reused successfully")
	}

	pool.Put(buf2)
}

// TestS3BufferedReaderInterface tests the s3BufferedReader interface
func TestS3BufferedReaderInterface(t *testing.T) {
	testData := []byte("test data for s3 buffered reader")
	buf := make([]byte, 64*1024)
	pool := &sync.Pool{
		New: func() interface{} {
			return make([]byte, 64*1024)
		},
	}

	reader := &s3BufferedReader{
		Reader: bytes.NewReader(testData),
		buffer: buf,
		pool:   pool,
	}

	// Test Read interface
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, data)

	// Test Close interface
	err = reader.Close()
	assert.NoError(t, err)

	// Verify buffer and pool were cleared
	assert.Nil(t, reader.buffer)
	assert.Nil(t, reader.pool)
}

// BenchmarkSingleflightEffectiveness benchmarks singleflight effectiveness
func BenchmarkSingleflightEffectiveness(b *testing.B) {
	var sf singleflight.Group
	var callCount int64

	// Simulate a slow operation
	slowOperation := func() (interface{}, error) {
		atomic.AddInt64(&callCount, 1)
		time.Sleep(1 * time.Millisecond) // Simulate network delay
		return "result", nil
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			result, err, _ := sf.Do("benchmark-key", slowOperation)
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

// BenchmarkBufferPoolVsAllocation compares buffer pool vs direct allocation
func BenchmarkBufferPoolVsAllocation(b *testing.B) {
	testData := make([]byte, 32*1024) // 32KB test data

	b.Run("WithBufferPool", func(b *testing.B) {
		pool := sync.Pool{
			New: func() interface{} {
				return make([]byte, 64*1024)
			},
		}

		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			buf := pool.Get().([]byte)
			copy(buf[:len(testData)], testData)
			_ = bytes.NewReader(buf[:len(testData)])
			pool.Put(buf)
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
