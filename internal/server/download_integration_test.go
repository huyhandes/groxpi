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
	"github.com/huyhandes/groxpi/internal/config"
	"github.com/stretchr/testify/assert"
)

type integrationTestResult struct {
	StatusCode int
	BodySize   int
	Error      error
}

// TestServer_DownloadCoordination_Integration tests the complete download coordination flow
func TestServer_DownloadCoordination_Integration(t *testing.T) {
	packageName := "integration-test"
	fileName := "large-file-1.0.0.tar.gz"
	fileContent := make([]byte, 10*1024*1024) // 10MB test file
	for i := range fileContent {
		fileContent[i] = byte(i % 256)
	}

	// Track download attempts to verify deduplication
	downloadAttempts := int64(0)

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
				// Return package index HTML
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
			// Simulate download with controlled delay
			atomic.AddInt64(&downloadAttempts, 1)
			time.Sleep(100 * time.Millisecond) // Simulate download time

			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fileContent)))
			_, _ = w.Write(fileContent)
		}
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 10 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := New(cfg)
	app := srv.App()

	// Test concurrent downloads are deduplicated
	t.Run("concurrent_deduplication", func(t *testing.T) {
		atomic.StoreInt64(&downloadAttempts, 0)

		numConcurrentRequests := 10
		var wg sync.WaitGroup
		results := make([]integrationTestResult, numConcurrentRequests)

		startTime := time.Now()

		// Launch concurrent requests
		for i := 0; i < numConcurrentRequests; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()

				req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
				resp, err := app.Test(req, 15000) // 15 second timeout

				results[index] = integrationTestResult{
					StatusCode: -1,
					Error:      err,
				}

				if err == nil {
					results[index].StatusCode = resp.StatusCode
					if resp.Body != nil {
						body, readErr := io.ReadAll(resp.Body)
						if readErr == nil {
							results[index].BodySize = len(body)
						}
						_ = resp.Body.Close()
					}
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(startTime)

		// Verify all requests succeeded
		successCount := 0
		for i, result := range results {
			if result.Error != nil {
				t.Errorf("Request %d failed: %v", i, result.Error)
				continue
			}

			if result.StatusCode != http.StatusOK {
				t.Errorf("Request %d: expected status 200, got %d", i, result.StatusCode)
				continue
			}

			if result.BodySize != len(fileContent) {
				t.Errorf("Request %d: expected body size %d, got %d", i, len(fileContent), result.BodySize)
				continue
			}

			successCount++
		}

		// Verify download deduplication worked
		attempts := atomic.LoadInt64(&downloadAttempts)
		assert.Equal(t, int64(1), attempts,
			"Expected exactly 1 download attempt due to deduplication, got %d", attempts)

		assert.Equal(t, numConcurrentRequests, successCount,
			"All %d concurrent requests should succeed", numConcurrentRequests)

		t.Logf("Integration test completed: %d concurrent requests, %d download attempts, duration: %v",
			numConcurrentRequests, attempts, duration)
	})
}

// TestServer_DownloadCoordination_RealWorld tests real-world scenarios
func TestServer_DownloadCoordination_RealWorld(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping real-world integration test in short mode")
	}

	// Test with pyspark-like scenario
	packageName := "pyspark-test"
	fileName := "pyspark-3.4.0.tar.gz"
	// Create a realistic large file (not 317MB for test speed, but large enough)
	fileContent := make([]byte, 5*1024*1024) // 5MB for testing
	for i := range fileContent {
		fileContent[i] = byte(i % 256)
	}

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
			// Simulate slow download
			time.Sleep(200 * time.Millisecond)

			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fileContent)))
			_, _ = w.Write(fileContent)
		}
	}))
	defer mockPyPI.Close()

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 30 * time.Second, // Realistic timeout
		LogLevel:        "INFO",
	}

	srv := New(cfg)
	app := srv.App()

	// Simulate multiple pip clients trying to download the same large package
	t.Run("multiple_pip_clients", func(t *testing.T) {
		numClients := 5
		var wg sync.WaitGroup
		clientResults := make([]bool, numClients)

		startTime := time.Now()

		for i := 0; i < numClients; i++ {
			wg.Add(1)
			go func(clientIndex int) {
				defer wg.Done()

				req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
				resp, err := app.Test(req, 45000) // 45 second timeout for real-world scenario

				if err != nil {
					t.Errorf("Client %d failed: %v", clientIndex, err)
					return
				}

				if resp.StatusCode != http.StatusOK {
					t.Errorf("Client %d: expected status 200, got %d", clientIndex, resp.StatusCode)
					_ = resp.Body.Close()
					return
				}

				// Verify content
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				if err != nil {
					t.Errorf("Client %d: failed to read body: %v", clientIndex, err)
					return
				}

				if len(body) != len(fileContent) {
					t.Errorf("Client %d: expected body size %d, got %d", clientIndex, len(fileContent), len(body))
					return
				}

				clientResults[clientIndex] = true
			}(i)
		}

		wg.Wait()
		duration := time.Since(startTime)

		// Verify all clients succeeded
		successCount := 0
		for i, success := range clientResults {
			if success {
				successCount++
			} else {
				t.Errorf("Client %d failed", i)
			}
		}

		assert.Equal(t, numClients, successCount, "All clients should succeed")

		// With proper coordination, this should complete much faster than sequential downloads
		maxExpectedDuration := 2 * time.Second // Should be much faster than 5 * 200ms = 1 second
		assert.Less(t, duration, maxExpectedDuration,
			"Coordinated downloads should be faster than sequential: got %v, max expected %v",
			duration, maxExpectedDuration)

		t.Logf("Real-world test: %d clients, duration: %v (should be close to single download time)",
			numClients, duration)
	})
}
