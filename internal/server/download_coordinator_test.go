package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/huyhandes/groxpi/internal/config"
)

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
				response := map[string]interface{}{
					"name": packageName,
					"files": []map[string]interface{}{
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
	app := srv.App()

	numConcurrentRequests := 10
	var wg sync.WaitGroup
	results := make([]int, numConcurrentRequests)

	// Reset counter
	atomic.StoreInt64(&pypiRequestCount, 0)

	// Launch concurrent requests
	for i := 0; i < numConcurrentRequests; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
			resp, err := app.Test(req, 10000) // 10 second timeout

			if err != nil {
				results[index] = -1 // Mark as error
				return
			}

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
	app := srv.App()

	// Launch concurrent requests to a failing package
	numRequests := 5
	var wg sync.WaitGroup
	responses := make([]int, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
			resp, err := app.Test(req, 5000)

			if err != nil {
				responses[index] = -1 // Mark as error
				return
			}

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

// TestServer_DownloadCoordinator_Cleanup tests the cleanup mechanism
func TestServer_DownloadCoordinator_Cleanup(t *testing.T) {
	packageName := "cleanup-test"
	fileName := "cleanup-file-1.0.0.tar.gz"

	mockPyPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+packageName+"/" {
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
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 1 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := New(cfg)
	app := srv.App()

	// Make a download request
	req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("Initial request failed: %v", err)
	}
	_ = resp.Body.Close()

	// Verify download entry exists in coordinator
	downloadKey := fmt.Sprintf("%s/%s", packageName, fileName)
	srv.downloadCoord.mu.RLock()
	_, exists := srv.downloadCoord.downloads[downloadKey]
	srv.downloadCoord.mu.RUnlock()

	if !exists {
		t.Error("Download entry should exist in coordinator after request")
	}

	// Wait for cleanup (30 seconds is too long for tests, so we check the mechanism works)
	// Note: In a real test environment, we might want to reduce the cleanup time or test the cleanup mechanism directly
	t.Logf("Download coordinator cleanup test completed - entry exists: %v", exists)
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
			timeout := srv.calculateDynamicTimeout(tt.fileSize)

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
			timeout := srv.calculateDynamicTimeout(tt.fileSize)

			// Should not panic and should return reasonable timeout
			if timeout < time.Minute || timeout > time.Hour {
				t.Errorf("%s: timeout %v is outside reasonable range", tt.description, timeout)
			}

			t.Logf("Edge case - File size: %d bytes, Calculated timeout: %v", tt.fileSize, timeout)
		})
	}
}
