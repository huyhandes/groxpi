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

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
	"github.com/huyhandes/groxpi/internal/config"
)

// testRequestCoord performs an HTTP request against the router
func testRequestCoord(router *gin.Engine, req *http.Request) *http.Response {
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

// TestServer_DownloadCoordinator_ConcurrentRequests tests that concurrent requests for the same file are properly coordinated
func TestServer_DownloadCoordinator_ConcurrentRequests(t *testing.T) {
	packageName := "test-package"
	fileName := "test-file-1.0.0.tar.gz"
	fileContent := []byte("test file content")

	pypiRequestCount := int64(0)

	var mockPyPI *httptest.Server
	mockPyPI = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+packageName+"/" {
			accept := r.Header.Get("Accept")
			if strings.Contains(accept, "application/vnd.pypi.simple.v1+json") {
				// Return JSON API response
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				response := map[string]any{
					"name": packageName,
					"files": []map[string]any{
						{
							"filename": fileName,
							"url":      fmt.Sprintf("%s/files/%s", mockPyPI.URL, fileName),
							"size":     int64(len(fileContent)),
						},
					},
				}
				jsonData, _ := sonic.Marshal(response)
				_, _ = w.Write(jsonData)
			} else {
				// Return HTML response
				w.Header().Set("Content-Type", "text/html")
				_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Links for %s</title></head>
<body>
<h1>Links for %s</h1>
<a href="/files/%s">%s</a>
</body>
</html>`, packageName, packageName, fileName, fileName)
			}
		} else if strings.Contains(r.URL.Path, "/files/") {
			atomic.AddInt64(&pypiRequestCount, 1)
			// Simulate some processing time
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(fileContent)
		}
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 5 * time.Second,
		LogLevel:        "DEBUG",
	}

	srv := New(cfg)
	router := srv.Router()

	numConcurrentRequests := 10
	var wg sync.WaitGroup
	results := make([]int, numConcurrentRequests)

	// Reset counter
	atomic.StoreInt64(&pypiRequestCount, 0)

	// Launch concurrent requests
	for i := range numConcurrentRequests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
			resp := testRequestCoord(router, req)

			results[index] = resp.StatusCode
			_ = resp.Body.Close()
		}(i)
	}

	wg.Wait()

	// Verify all requests succeeded (or failed gracefully)
	successCount := 0
	for _, status := range results {
		if status == http.StatusOK {
			successCount++
		}
	}

	// Should have some successful downloads
	if successCount == 0 {
		t.Error("Expected at least some successful downloads")
	}

	// The key test: PyPI should have been contacted much less than the number of concurrent requests
	pypiRequests := atomic.LoadInt64(&pypiRequestCount)
	if pypiRequests > int64(numConcurrentRequests/2) {
		t.Errorf("Expected coordination to reduce PyPI requests, got %d PyPI requests for %d concurrent requests",
			pypiRequests, numConcurrentRequests)
	}

	t.Logf("Download coordination test completed: %d concurrent requests handled with %d PyPI requests",
		numConcurrentRequests, pypiRequests)
}

// TestServer_DownloadCoordinator_ErrorHandling tests error propagation in concurrent downloads
func TestServer_DownloadCoordinator_ErrorHandling(t *testing.T) {
	packageName := "error-package"
	fileName := "error-file-1.0.0.tar.gz"

	// Mock PyPI that returns errors
	mockPyPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+packageName+"/" {
			// Return 404 to simulate package not found
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 1 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := New(cfg)
	router := srv.Router()

	// Launch concurrent requests to a failing package
	numRequests := 5
	var wg sync.WaitGroup
	responses := make([]int, numRequests)

	for i := range numRequests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
			resp := testRequestCoord(router, req)

			responses[index] = resp.StatusCode
			_ = resp.Body.Close()
		}(i)
	}

	wg.Wait()

	// All requests should handle errors gracefully (404 or similar)
	for i, status := range responses {
		if status != http.StatusNotFound && status != -1 {
			// Allow 404 or request errors, but not 500 or hangs
			t.Errorf("Request %d: expected error handling, got status %d", i, status)
		}
	}
}

// TestServer_DownloadDedup_ReleasesStateAfterDownload is the port of the old
// hand-rolled download coordinator cleanup test. The coordinator kept a per-download entry in
// a map and needed a 30s cleanup goroutine to drop it; singleflight releases the
// key as soon as the leader returns. The observable claim is the same: the dedup
// bookkeeping for a finished download must not linger and block or misdirect the
// next request for that file.
func TestServer_DownloadDedup_ReleasesStateAfterDownload(t *testing.T) {
	packageName := "cleanup-test"
	fileName := "cleanup-file-1.0.0.tar.gz"
	fileContent := []byte("cleanup payload")

	upstreamDownloads := int64(0)

	// NOTE: the href is absolute on purpose - the HTML index parser does not
	// resolve relative hrefs against the index URL (see report).
	var mockPyPI *httptest.Server
	mockPyPI = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+packageName+"/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Links for %s</title></head>
<body>
<h1>Links for %s</h1>
<a href="%s/files/%s">%s</a>
</body>
</html>`, packageName, packageName, mockPyPI.URL, fileName, fileName)
			return
		}
		if strings.Contains(r.URL.Path, "/files/") {
			atomic.AddInt64(&upstreamDownloads, 1)
			_, _ = w.Write(fileContent)
		}
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 5 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := New(cfg)
	router := srv.Router()

	// First request downloads and caches.
	req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
	resp := testRequestCoord(router, req)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}
	if len(body) != len(fileContent) {
		t.Fatalf("Expected body size %d, got %d", len(fileContent), len(body))
	}
	if got := atomic.LoadInt64(&upstreamDownloads); got != 1 {
		t.Fatalf("Expected 1 upstream download, got %d", got)
	}

	// A later request for the same file must be served immediately from the cache
	// populated by the first one - no stale dedup entry, no second upstream fetch.
	req = httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
	resp = testRequestCoord(router, req)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200 on the cached request, got %d", resp.StatusCode)
	}
	if len(body) != len(fileContent) {
		t.Errorf("Expected cached body size %d, got %d", len(fileContent), len(body))
	}
	if got := atomic.LoadInt64(&upstreamDownloads); got != 1 {
		t.Errorf("Expected the cached request to skip upstream, got %d downloads", got)
	}
}

// TestServer_CalculateDynamicTimeout tests timeout calculation for various file sizes
func TestServer_CalculateDynamicTimeout(t *testing.T) {
	cfg := &config.Config{
		IndexURL:        "https://pypi.org/simple/",
		CacheDir:        t.TempDir(),
		DownloadTimeout: 900 * time.Millisecond, // 900ms default
	}

	srv := New(cfg)

	tests := []struct {
		name        string
		fileSize    int64
		expectedMin time.Duration
		expectedMax time.Duration
		description string
	}{
		{
			name:        "zero_size",
			fileSize:    0,
			expectedMin: 900 * time.Millisecond, // Should use default config timeout
			expectedMax: 1 * time.Second,
			description: "Zero size should return default timeout",
		},
		{
			name:        "small_file_1KB",
			fileSize:    1024,            // 1KB
			expectedMin: 2 * time.Minute, // All files get at least 2min due to calculateDynamicTimeout logic
			expectedMax: 3 * time.Minute,
			description: "Small files should use minimum timeout",
		},
		{
			name:        "medium_file_1MB",
			fileSize:    1024 * 1024, // 1MB
			expectedMin: 2 * time.Minute,
			expectedMax: 5 * time.Minute,
			description: "Medium files should scale reasonably",
		},
		{
			name:        "large_file_100MB",
			fileSize:    100 * 1024 * 1024, // 100MB
			expectedMin: 15 * time.Minute,
			expectedMax: 25 * time.Minute,
			description: "Large files should have generous timeout",
		},
		{
			name:        "pyspark_317MB",
			fileSize:    317 * 1024 * 1024, // 317MB (real case)
			expectedMin: 50 * time.Minute,
			expectedMax: 60 * time.Minute,
			description: "Pyspark-sized files should have very generous timeout",
		},
		{
			name:        "huge_file_1GB",
			fileSize:    1024 * 1024 * 1024, // 1GB
			expectedMin: 60 * time.Minute,
			expectedMax: 60 * time.Minute,
			description: "Huge files should have maximum reasonable timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timeout := srv.packageFiles.calculateDynamicTimeout(tt.fileSize)

			if timeout < tt.expectedMin {
				t.Errorf("%s: timeout %v is less than expected minimum %v", tt.description, timeout, tt.expectedMin)
			}

			if timeout > tt.expectedMax {
				t.Errorf("%s: timeout %v is greater than expected maximum %v", tt.description, timeout, tt.expectedMax)
			}

			t.Logf("%s - File size: %dMB, Calculated timeout: %v",
				tt.description, tt.fileSize/(1024*1024), timeout)
		})
	}
}

// TestServer_CalculateDynamicTimeout_EdgeCases tests edge cases for timeout calculation
func TestServer_CalculateDynamicTimeout_EdgeCases(t *testing.T) {
	cfg := &config.Config{
		IndexURL:        "https://pypi.org/simple/",
		CacheDir:        t.TempDir(),
		DownloadTimeout: 5 * time.Minute,
	}

	srv := New(cfg)
	tests := []struct {
		name        string
		fileSize    int64
		description string
	}{
		{
			name:        "negative_size",
			fileSize:    -1,
			description: "Negative file size should not crash",
		},
		{
			name:        "max_int64",
			fileSize:    9223372036854775807, // Max int64
			description: "Maximum int64 should not cause overflow",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timeout := srv.packageFiles.calculateDynamicTimeout(tt.fileSize)

			// Should not panic and should return reasonable timeout
			if timeout < time.Minute || timeout > time.Hour {
				t.Errorf("%s: timeout %v is outside reasonable range", tt.description, timeout)
			}

			t.Logf("Edge case - File size: %d bytes, Calculated timeout: %v", tt.fileSize, timeout)
		})
	}
}
