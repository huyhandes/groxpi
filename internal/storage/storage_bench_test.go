package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"testing"
)

// BenchmarkS3Storage benchmarks S3 storage operations
func BenchmarkS3Storage(b *testing.B) {
	// Skip if S3 credentials are not set
	if os.Getenv("BENCH_S3_ENDPOINT") == "" {
		b.Skip("S3 benchmarks require BENCH_S3_ENDPOINT to be set")
	}

	cfg := &S3Config{
		Endpoint:        os.Getenv("BENCH_S3_ENDPOINT"),
		AccessKeyID:     os.Getenv("BENCH_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("BENCH_S3_SECRET_KEY"),
		Bucket:          os.Getenv("BENCH_S3_BUCKET"),
		Region:          "us-east-1",
		UseSSL:          false,
		ForcePathStyle:  true,
		Prefix:          "bench",
		PartSize:        10 * 1024 * 1024, // 10MB parts
		MaxConnections:  100,
	}

	storage, err := NewS3Storage(cfg)
	if err != nil {
		b.Fatalf("Failed to create S3 storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	ctx := context.Background()

	// Generate test data
	sizes := []int{
		1 * 1024,         // 1KB
		100 * 1024,       // 100KB
		1024 * 1024,      // 1MB
		10 * 1024 * 1024, // 10MB
	}

	for _, size := range sizes {
		sizeName := formatSize(size)

		b.Run(fmt.Sprintf("Put_%s", sizeName), func(b *testing.B) {
			data := make([]byte, size)
			_, _ = rand.Read(data)

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("bench/put_%d_%d", size, i)
				_, err := storage.Put(ctx, key, bytes.NewReader(data), int64(size), "application/octet-stream")
				if err != nil {
					b.Fatalf("Failed to put: %v", err)
				}
				// Clean up immediately to avoid filling storage
				_ = storage.Delete(ctx, key)
			}
		})

		b.Run(fmt.Sprintf("Get_%s", sizeName), func(b *testing.B) {
			// Setup: upload test data
			data := make([]byte, size)
			_, _ = rand.Read(data)
			key := fmt.Sprintf("bench/get_%d", size)
			_, err := storage.Put(ctx, key, bytes.NewReader(data), int64(size), "application/octet-stream")
			if err != nil {
				b.Fatalf("Failed to setup: %v", err)
			}
			defer func() { _ = storage.Delete(ctx, key) }()

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				reader, _, err := storage.Get(ctx, key)
				if err != nil {
					b.Fatalf("Failed to get: %v", err)
				}
				_, _ = io.Copy(io.Discard, reader)
				_ = reader.Close()
			}
		})
	}

	b.Run("List", func(b *testing.B) {
		// Setup: upload 100 small files
		for i := range 100 {
			key := fmt.Sprintf("bench/list/item_%03d", i)
			data := []byte(key)
			_, _ = storage.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "text/plain")
		}
		defer func() {
			// Cleanup
			for i := range 100 {
				key := fmt.Sprintf("bench/list/item_%03d", i)
				_ = storage.Delete(ctx, key)
			}
		}()

		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			_, err := storage.List(ctx, ListOptions{
				Prefix:  "bench/list/",
				MaxKeys: 50,
			})
			if err != nil {
				b.Fatalf("Failed to list: %v", err)
			}
		}
	})

}

// BenchmarkLocalStorage benchmarks local storage operations
func BenchmarkLocalStorage(b *testing.B) {
	tmpDir := b.TempDir()

	storage, err := NewLocalStorage(tmpDir)
	if err != nil {
		b.Fatalf("Failed to create local storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	ctx := context.Background()

	// Generate test data
	sizes := []int{
		1 * 1024,         // 1KB
		100 * 1024,       // 100KB
		1024 * 1024,      // 1MB
		10 * 1024 * 1024, // 10MB
	}

	for _, size := range sizes {
		sizeName := formatSize(size)

		b.Run(fmt.Sprintf("Put_%s", sizeName), func(b *testing.B) {
			data := make([]byte, size)
			_, _ = rand.Read(data)

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				key := fmt.Sprintf("bench/put_%d_%d", size, i)
				_, err := storage.Put(ctx, key, bytes.NewReader(data), int64(size), "application/octet-stream")
				if err != nil {
					b.Fatalf("Failed to put: %v", err)
				}
			}
		})

		b.Run(fmt.Sprintf("Get_%s", sizeName), func(b *testing.B) {
			// Setup: write test data
			data := make([]byte, size)
			_, _ = rand.Read(data)
			key := fmt.Sprintf("bench/get_%d", size)
			_, err := storage.Put(ctx, key, bytes.NewReader(data), int64(size), "application/octet-stream")
			if err != nil {
				b.Fatalf("Failed to setup: %v", err)
			}

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				reader, _, err := storage.Get(ctx, key)
				if err != nil {
					b.Fatalf("Failed to get: %v", err)
				}
				_, _ = io.Copy(io.Discard, reader)
				_ = reader.Close()
			}
		})
	}

	b.Run("Exists", func(b *testing.B) {
		// Setup: write test file
		key := "bench/exists"
		data := []byte("test")
		_, err := storage.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "text/plain")
		if err != nil {
			b.Fatalf("Failed to setup: %v", err)
		}

		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			_, err := storage.Exists(ctx, key)
			if err != nil {
				b.Fatalf("Failed to check existence: %v", err)
			}
		}
	})
}

// formatSize formats bytes into human-readable string
func formatSize(bytes int) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%d%cB", bytes/int(div), "KMGTPE"[exp])
}
