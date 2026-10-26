package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/stretchr/testify/assert"
)

// testRequestIntegration performs an HTTP request against the router
func testRequestIntegration(router http.Handler, req *http.Request) *http.Response {
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

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

	// The delay simulates download time so the concurrent requests overlap.
	mockPyPI := NewFakePyPI(t).SetDelay(100*time.Millisecond).
		Add(packageName, fileName, FakeFile{Body: fileContent, Size: int64(len(fileContent))})

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 10 * time.Second,
		LogLevel:        "ERROR",
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	// Test concurrent downloads are deduplicated
	t.Run("concurrent_deduplication", func(t *testing.T) {
		numConcurrentRequests := 10
		var wg sync.WaitGroup
		results := make([]integrationTestResult, numConcurrentRequests)

		startTime := time.Now()

		// Launch concurrent requests
		for i := range numConcurrentRequests {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()

				req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
				resp := testRequestIntegration(router, req)

				results[index] = integrationTestResult{
					StatusCode: resp.StatusCode,
					Error:      nil,
				}

				if resp.Body != nil {
					body, readErr := io.ReadAll(resp.Body)
					if readErr == nil {
						results[index].BodySize = len(body)
					}
					_ = resp.Body.Close()
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
		attempts := mockPyPI.FileHits(fileName)
		assert.Equal(t, 1, attempts,
			"Expected exactly 1 download attempt due to deduplication, got %d", attempts)

		assert.Equal(t, numConcurrentRequests, successCount,
			"All %d concurrent requests should succeed", numConcurrentRequests)

		t.Logf("Integration test completed: %d concurrent requests, %d download attempts, duration: %v",
			numConcurrentRequests, attempts, duration)
	})
}

// TestServer_PackageNameNormalization_Integration asserts that every legal
// spelling of a package name reaches the same upstream path and the same cache
// entry (PEP 503).
func TestServer_PackageNameNormalization_Integration(t *testing.T) {
	const normalized = "zope-interface"

	mockPyPI := NewFakePyPI(t).Add(normalized, "zope.interface-5.0.tar.gz", FakeFile{})

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		IndexTTL:        time.Hour,
		DownloadTimeout: 10 * time.Second,
		LogLevel:        "ERROR",
	}
	router := newTestServer(t, cfg).Router()

	spellings := []string{"zope.interface", "zope-interface", "Zope__Interface", "zope---interface"}
	for _, spelling := range spellings {
		req := httptest.NewRequest("GET", "/simple/"+spelling+"/", nil)
		resp := testRequestIntegration(router, req)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode, "spelling %q should resolve", spelling)
		assert.Contains(t, string(body), "zope.interface-5.0.tar.gz", "spelling %q should list the file", spelling)
	}

	assert.Equal(t, 1, mockPyPI.IndexHits(normalized),
		"all spellings must produce exactly one upstream fetch of the normalized name")
	for _, spelling := range spellings {
		if spelling != normalized {
			assert.Zero(t, mockPyPI.IndexHits(spelling), "upstream must never see the spelling %q", spelling)
		}
	}
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

	// The delay simulates a slow download.
	mockPyPI := NewFakePyPI(t).SetDelay(200*time.Millisecond).
		Add(packageName, fileName, FakeFile{Body: fileContent, Size: int64(len(fileContent))})

	cfg := &config.Config{
		IndexURL:        mockPyPI.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 30 * time.Second, // Realistic timeout
		LogLevel:        "INFO",
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	// Simulate multiple pip clients trying to download the same large package
	t.Run("multiple_pip_clients", func(t *testing.T) {
		numClients := 5
		var wg sync.WaitGroup
		clientResults := make([]bool, numClients)

		startTime := time.Now()

		for i := range numClients {
			wg.Add(1)
			go func(clientIndex int) {
				defer wg.Done()

				req := httptest.NewRequest("GET", fmt.Sprintf("/index/%s/%s", packageName, fileName), nil)
				resp := testRequestIntegration(router, req)

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
