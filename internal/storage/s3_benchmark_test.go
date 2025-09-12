package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkS3Storage_BufferPools benchmarks the buffer pool optimizations
func BenchmarkS3Storage_BufferPools(b *testing.B) {
	testData := make([]byte, 32*1024) // 32KB test data
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	b.Run("S3BufferPool_vs_DirectAllocation", func(b *testing.B) {
		b.Run("WithS3BufferPool", func(b *testing.B) {
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				buf := s3BufferPool.Get().([]byte)
				copy(buf[:len(testData)], testData)
				_ = bytes.NewReader(buf[:len(testData)])
				s3BufferPool.Put(buf)
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
	})

	b.Run("S3ResponsePool_vs_DirectAllocation", func(b *testing.B) {
		testContent := "This is test content for response pool benchmarking"

		b.Run("WithS3ResponsePool", func(b *testing.B) {
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				buf := s3ResponsePool.Get().(*bytes.Buffer)
				buf.Reset()
				buf.WriteString(testContent)
				_ = buf.Bytes()
				s3ResponsePool.Put(buf)
			}
		})

		b.Run("WithDirectAllocation", func(b *testing.B) {
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				buf := &bytes.Buffer{}
				buf.WriteString(testContent)
				_ = buf.Bytes()
			}
		})
	})
}

// BenchmarkS3Storage_SingleflightDeduplication benchmarks singleflight effectiveness
func BenchmarkS3Storage_SingleflightDeduplication(b *testing.B) {
	s := &S3Storage{
		bucket: "benchmark-bucket",
		prefix: "benchmark",
	}

	var operationCount int64

	// Simulate a slow operation that we want to deduplicate
	slowOperation := func() (interface{}, error) {
		atomic.AddInt64(&operationCount, 1)
		time.Sleep(100 * time.Microsecond) // Simulate network latency
		return "benchmark-result", nil
	}

	b.Run("SingleflightEffectiveness", func(b *testing.B) {
		atomic.StoreInt64(&operationCount, 0)
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				result, err, _ := s.getSF.Do("benchmark-key", slowOperation)
				if err != nil {
					b.Fatal(err)
				}
				if result.(string) != "benchmark-result" {
					b.Fatal("Wrong result")
				}
			}
		})

		finalCount := atomic.LoadInt64(&operationCount)
		reduction := float64(b.N-int(finalCount)) / float64(b.N) * 100
		b.Logf("Singleflight: %d operations resulted in %d actual calls (%.1f%% reduction)",
			b.N, finalCount, reduction)
	})

	b.Run("WithoutSingleflight", func(b *testing.B) {
		atomic.StoreInt64(&operationCount, 0)
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				// Call operation directly without singleflight
				result, err := slowOperation()
				if err != nil {
					b.Fatal(err)
				}
				if result.(string) != "benchmark-result" {
					b.Fatal("Wrong result")
				}
			}
		})

		finalCount := atomic.LoadInt64(&operationCount)
		b.Logf("Without singleflight: %d operations resulted in %d actual calls (no reduction)",
			b.N, finalCount)
	})

	b.Run("SingleflightDifferentKeys", func(b *testing.B) {
		atomic.StoreInt64(&operationCount, 0)
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			keyIndex := 0
			for pb.Next() {
				// Use different keys to test that singleflight doesn't interfere
				key := fmt.Sprintf("benchmark-key-%d", keyIndex%10)
				keyIndex++

				result, err, _ := s.getSF.Do(key, slowOperation)
				if err != nil {
					b.Fatal(err)
				}
				if result.(string) != "benchmark-result" {
					b.Fatal("Wrong result")
				}
			}
		})

		finalCount := atomic.LoadInt64(&operationCount)
		b.Logf("Singleflight with different keys: %d operations resulted in %d actual calls",
			b.N, finalCount)
	})
}

// BenchmarkS3Storage_BufferedReader benchmarks the s3BufferedReader implementation
func BenchmarkS3Storage_BufferedReader(b *testing.B) {
	testData := make([]byte, 16*1024) // 16KB test data
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	pool := &sync.Pool{
		New: func() interface{} {
			return make([]byte, 64*1024)
		},
	}

	b.Run("S3BufferedReader_Read", func(b *testing.B) {
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			buf := pool.Get().([]byte)
			reader := &s3BufferedReader{
				Reader: bytes.NewReader(testData),
				buffer: buf,
				pool:   pool,
			}

			// Read all data
			readBuf := make([]byte, len(testData))
			n, err := io.ReadFull(reader, readBuf)
			if err != nil {
				b.Fatal(err)
			}
			if n != len(testData) {
				b.Fatalf("Expected to read %d bytes, got %d", len(testData), n)
			}

			reader.Close()
		}
	})

	b.Run("DirectReader_Read", func(b *testing.B) {
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			reader := bytes.NewReader(testData)

			// Read all data
			readBuf := make([]byte, len(testData))
			n, err := io.ReadFull(reader, readBuf)
			if err != nil {
				b.Fatal(err)
			}
			if n != len(testData) {
				b.Fatalf("Expected to read %d bytes, got %d", len(testData), n)
			}
		}
	})
}

// BenchmarkS3Storage_KeyBuilding benchmarks key building operations
func BenchmarkS3Storage_KeyBuilding(b *testing.B) {
	testCases := []struct {
		name   string
		prefix string
		key    string
	}{
		{"no_prefix", "", "simple/key/path.txt"},
		{"short_prefix", "app", "simple/key/path.txt"},
		{"long_prefix", "application/environment/service/version", "simple/key/path.txt"},
		{"deep_key", "app", "very/deep/nested/directory/structure/file.txt"},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			s := &S3Storage{
				prefix: tc.prefix,
			}

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_ = s.buildKey(tc.key)
			}
		})
	}
}

// BenchmarkS3Storage_ConcurrentOperations benchmarks concurrent access patterns
func BenchmarkS3Storage_ConcurrentOperations(b *testing.B) {

	var operationCount int64
	mockOperation := func() (interface{}, error) {
		atomic.AddInt64(&operationCount, 1)
		// Simulate some work
		time.Sleep(10 * time.Microsecond)
		return "result", nil
	}

	// Create a storage instance for testing singleflight
	s := &S3Storage{
		bucket: "benchmark-bucket",
		prefix: "concurrent",
	}

	b.Run("ConcurrentSingleflightSameKey", func(b *testing.B) {
		atomic.StoreInt64(&operationCount, 0)
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				result, err, _ := s.getSF.Do("same-key", mockOperation)
				if err != nil {
					b.Fatal(err)
				}
				if result.(string) != "result" {
					b.Fatal("Wrong result")
				}
			}
		})

		finalCount := atomic.LoadInt64(&operationCount)
		b.Logf("Concurrent same key: %d requests -> %d operations", b.N, finalCount)
	})

	b.Run("ConcurrentSingleflightDifferentSF", func(b *testing.B) {
		atomic.StoreInt64(&operationCount, 0)
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			counter := 0
			for pb.Next() {
				counter++
				sfGroup := &s.getSF
				if counter%2 == 0 {
					sfGroup = &s.statSF
				}

				result, err, _ := sfGroup.Do("mixed-key", mockOperation)
				if err != nil {
					b.Fatal(err)
				}
				if result.(string) != "result" {
					b.Fatal("Wrong result")
				}
			}
		})

		finalCount := atomic.LoadInt64(&operationCount)
		b.Logf("Concurrent mixed singleflight: %d requests -> %d operations", b.N, finalCount)
	})
}

// BenchmarkS3Storage_MemoryAllocation benchmarks memory allocation patterns
func BenchmarkS3Storage_MemoryAllocation(b *testing.B) {
	testSizes := []int{
		1 * 1024,    // 1KB
		16 * 1024,   // 16KB
		64 * 1024,   // 64KB (buffer pool size)
		256 * 1024,  // 256KB
		1024 * 1024, // 1MB
	}

	for _, size := range testSizes {
		sizeName := fmt.Sprintf("%dB", size)
		if size >= 1024 {
			sizeName = fmt.Sprintf("%dKB", size/1024)
		}
		if size >= 1024*1024 {
			sizeName = fmt.Sprintf("%dMB", size/(1024*1024))
		}

		b.Run(fmt.Sprintf("Size_%s", sizeName), func(b *testing.B) {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i % 256)
			}

			b.Run("WithBufferPool", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					if size <= 64*1024 {
						// Use buffer pool for small sizes
						buf := s3BufferPool.Get().([]byte)
						if len(data) <= len(buf) {
							copy(buf[:len(data)], data)
							reader := bytes.NewReader(buf[:len(data)])
							io.ReadAll(reader)
						}
						s3BufferPool.Put(buf)
					} else {
						// Direct allocation for large sizes
						reader := bytes.NewReader(data)
						io.ReadAll(reader)
					}
				}
			})

			b.Run("WithoutBufferPool", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					// Always use direct allocation
					reader := bytes.NewReader(data)
					io.ReadAll(reader)
				}
			})
		})
	}
}

// BenchmarkS3Storage_StringOperations benchmarks string operations for key building
func BenchmarkS3Storage_StringOperations(b *testing.B) {
	testKeys := []string{
		"simple.txt",
		"path/to/file.txt",
		"very/deep/nested/directory/structure/with/many/levels/file.txt",
		"file-with-many-hyphens-and-dots.tar.gz",
		"unicode/文件名/файл.txt",
	}

	prefixes := []string{
		"",
		"app",
		"application/production/cache",
		"very/long/prefix/with/many/segments/for/organization",
	}

	for _, prefix := range prefixes {
		prefixName := "no_prefix"
		if prefix != "" {
			prefixName = fmt.Sprintf("prefix_%d_chars", len(prefix))
		}

		b.Run(prefixName, func(b *testing.B) {
			s := &S3Storage{prefix: prefix}

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				key := testKeys[i%len(testKeys)]
				_ = s.buildKey(key)
			}
		})
	}
}

// BenchmarkS3Storage_BufferedReaderPool benchmarks buffer pool effectiveness with realistic usage
func BenchmarkS3Storage_BufferedReaderPool(b *testing.B) {
	// Create test data of various sizes
	testSizes := []int{1024, 16 * 1024, 32 * 1024, 64 * 1024, 128 * 1024}
	testData := make(map[int][]byte)

	for _, size := range testSizes {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i % 256)
		}
		testData[size] = data
	}

	pool := &sync.Pool{
		New: func() interface{} {
			return make([]byte, 64*1024)
		},
	}

	b.Run("RealisticUsagePattern", func(b *testing.B) {
		b.ResetTimer()

		b.RunParallel(func(pb *testing.PB) {
			sizeIndex := 0
			for pb.Next() {
				size := testSizes[sizeIndex%len(testSizes)]
				sizeIndex++
				data := testData[size]

				if size <= 64*1024 {
					// Use buffer pool for small files
					buf := pool.Get().([]byte)
					reader := &s3BufferedReader{
						Reader: bytes.NewReader(data),
						buffer: buf,
						pool:   pool,
					}

					// Simulate reading the data
					readData := make([]byte, size)
					_, err := io.ReadFull(reader, readData)
					if err != nil {
						b.Fatal(err)
					}

					reader.Close()
				} else {
					// Direct reader for large files
					reader := bytes.NewReader(data)
					readData := make([]byte, size)
					_, err := io.ReadFull(reader, readData)
					if err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	})
}

// BenchmarkS3Storage_ContextCancellation benchmarks context handling performance
func BenchmarkS3Storage_ContextCancellation(b *testing.B) {

	b.Run("ContextWithTimeout", func(b *testing.B) {
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)

			// Simulate checking context
			select {
			case <-ctx.Done():
				// Context cancelled
			default:
				// Context still valid
			}

			cancel()
		}
	})

	b.Run("ContextWithCancel", func(b *testing.B) {
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			ctx, cancel := context.WithCancel(context.Background())

			// Simulate some work
			select {
			case <-ctx.Done():
				// Context cancelled
			default:
				// Context still valid
			}

			cancel()
		}
	})
}
