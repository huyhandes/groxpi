package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/huyhandes/groxpi/internal/config"
)

// testRequestBench performs an HTTP request against the router
func testRequestBench(router *gin.Engine, req *http.Request) *http.Response {
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

// BenchmarkDownloadCoordination_SingleFile tests download coordination performance
func BenchmarkDownloadCoordination_SingleFile(b *testing.B) {
	packageName := "bench-test"
	fileName := "bench-file-1.0.0.tar.gz"
	fileContent := make([]byte, 1024*1024) // 1MB test file
	for i := range fileContent {
		fileContent[i] = byte(i % 256)
	}

	// Track download attempts
	downloadAttempts := int64(0)

	mockPyPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, fmt.Sprintf("/%s/", packageName)) {
			// Return package file list (JSON format expected by pypi client)
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			baseURL := fmt.Sprintf("http://%s", r.Host)
			_, _ = fmt.Fprintf(w, `{
  "meta": {"api-version": "1.0"},
  "name": "%s",
  "files": [
    {
      "filename": "%s",
      "url": "%s/files/%s",
      "size": %d
    }
  ]
}`, packageName, fileName, baseURL, fileName, len(fileContent))
		} else if strings.Contains(r.URL.Path, "/files/") {
			atomic.AddInt64(&downloadAttempts, 1)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fileContent)))
			_, _ = w.Write(fileContent)
		}
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        b.TempDir(),
		DownloadTimeout: 30 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := New(cfg)
	router := srv.Router()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		atomic.StoreInt64(&downloadAttempts, 0)

		req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
		resp := testRequestBench(router, req)

		if resp.StatusCode != http.StatusOK {
			b.Fatalf("Expected status 200, got %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if err != nil {
			b.Fatalf("Failed to read body: %v", err)
		}

		if len(body) != len(fileContent) {
			b.Fatalf("Expected body size %d, got %d", len(fileContent), len(body))
		}
	}

	b.ReportMetric(float64(atomic.LoadInt64(&downloadAttempts)), "downloads/op")
}

// BenchmarkDownloadCoordination_ConcurrentRequests tests concurrent performance
func BenchmarkDownloadCoordination_ConcurrentRequests(b *testing.B) {
	packageName := "concurrent-bench"
	fileName := "concurrent-file-1.0.0.tar.gz"
	fileContent := make([]byte, 1024*1024) // 1MB test file
	for i := range fileContent {
		fileContent[i] = byte(i % 256)
	}

	downloadAttempts := int64(0)

	mockPyPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, fmt.Sprintf("/%s/", packageName)) {
			// Return package file list (JSON format expected by pypi client)
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			baseURL := fmt.Sprintf("http://%s", r.Host)
			_, _ = fmt.Fprintf(w, `{
  "meta": {"api-version": "1.0"},
  "name": "%s",
  "files": [
    {
      "filename": "%s",
      "url": "%s/files/%s",
      "size": %d
    }
  ]
}`, packageName, fileName, baseURL, fileName, len(fileContent))
		} else if strings.Contains(r.URL.Path, "/files/") {
			atomic.AddInt64(&downloadAttempts, 1)
			// Simulate download time
			time.Sleep(10 * time.Millisecond)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fileContent)))
			_, _ = w.Write(fileContent)
		}
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        b.TempDir(),
		DownloadTimeout: 30 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := New(cfg)
	router := srv.Router()

	concurrencyLevels := []int{1, 2, 5, 10, 20}

	for _, concurrency := range concurrencyLevels {
		b.Run(fmt.Sprintf("concurrent_%d", concurrency), func(b *testing.B) {
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				atomic.StoreInt64(&downloadAttempts, 0)

				var wg sync.WaitGroup
				errors := make(chan error, concurrency)

				startTime := time.Now()

				for j := 0; j < concurrency; j++ {
					wg.Add(1)
					go func() {
						defer wg.Done()

						req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
						resp := testRequestBench(router, req)

						if resp.StatusCode != http.StatusOK {
							errors <- fmt.Errorf("expected status 200, got %d", resp.StatusCode)
							_ = resp.Body.Close()
							return
						}

						body, err := io.ReadAll(resp.Body)
						_ = resp.Body.Close()

						if err != nil {
							errors <- fmt.Errorf("failed to read body: %v", err)
							return
						}

						if len(body) != len(fileContent) {
							errors <- fmt.Errorf("expected body size %d, got %d", len(fileContent), len(body))
							return
						}
					}()
				}

				wg.Wait()
				close(errors)

				// Check for errors
				for err := range errors {
					b.Fatalf("Concurrent request failed: %v", err)
				}

				duration := time.Since(startTime)
				attempts := atomic.LoadInt64(&downloadAttempts)

				b.ReportMetric(float64(attempts), "downloads/op")
				b.ReportMetric(duration.Seconds(), "duration_sec")
				b.ReportMetric(float64(concurrency), "concurrency")
			}
		})
	}
}

// BenchmarkCalculateDynamicTimeout tests timeout calculation performance
func BenchmarkCalculateDynamicTimeout(b *testing.B) {
	cfg := &config.Config{
		IndexURL:        "http://example.com",
		CacheDir:        "/tmp/bench",
		DownloadTimeout: 30 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := New(cfg)

	fileSizes := []int64{
		1024,                    // 1KB
		1024 * 1024,             // 1MB
		10 * 1024 * 1024,        // 10MB
		100 * 1024 * 1024,       // 100MB
		317 * 1024 * 1024,       // 317MB (pyspark)
		1024 * 1024 * 1024,      // 1GB
		10 * 1024 * 1024 * 1024, // 10GB
	}

	for _, size := range fileSizes {
		b.Run(fmt.Sprintf("size_%dMB", size/(1024*1024)), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = srv.calculateDynamicTimeout(size)
			}
		})
	}
}

// BenchmarkDownloadCoordination_LargeFile tests performance with larger files
func BenchmarkDownloadCoordination_LargeFile(b *testing.B) {
	packageName := "large-bench"
	fileName := "large-file-1.0.0.tar.gz"
	fileSize := 10 * 1024 * 1024 // 10MB test file
	fileContent := make([]byte, fileSize)
	for i := range fileContent {
		fileContent[i] = byte(i % 256)
	}

	mockPyPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, fmt.Sprintf("/%s/", packageName)) {
			// Return package file list (JSON format expected by pypi client)
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			baseURL := fmt.Sprintf("http://%s", r.Host)
			_, _ = fmt.Fprintf(w, `{
  "meta": {"api-version": "1.0"},
  "name": "%s",
  "files": [
    {
      "filename": "%s",
      "url": "%s/files/%s",
      "size": %d
    }
  ]
}`, packageName, fileName, baseURL, fileName, len(fileContent))
		} else if strings.Contains(r.URL.Path, "/files/") {
			// Simulate network bandwidth limitation
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fileContent)))

			// Stream the content in chunks to simulate real-world download
			chunkSize := 64 * 1024 // 64KB chunks
			for i := 0; i < len(fileContent); i += chunkSize {
				end := i + chunkSize
				if end > len(fileContent) {
					end = len(fileContent)
				}
				_, _ = w.Write(fileContent[i:end])
				time.Sleep(time.Millisecond) // Simulate network latency
			}
		}
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        b.TempDir(),
		DownloadTimeout: 60 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := New(cfg)
	router := srv.Router()

	b.ResetTimer()
	b.SetBytes(int64(fileSize)) // Report throughput

	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
		resp := testRequestBench(router, req)

		if resp.StatusCode != http.StatusOK {
			b.Fatalf("Expected status 200, got %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if err != nil {
			b.Fatalf("Failed to read body: %v", err)
		}

		if len(body) != len(fileContent) {
			b.Fatalf("Expected body size %d, got %d", len(fileContent), len(body))
		}
	}
}
