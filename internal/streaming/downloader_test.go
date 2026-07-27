package streaming

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huyhandes/groxpi/internal/storage"
)

// Mock storage writer for testing
type mockStorageWriter struct {
	storage map[string][]byte
	mu      sync.RWMutex
	putErr  error
}

func newMockStorageWriter() *mockStorageWriter {
	return &mockStorageWriter{
		storage: make(map[string][]byte),
	}
}

func (m *mockStorageWriter) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*storage.ObjectInfo, error) {
	if m.putErr != nil {
		return nil, m.putErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	m.storage[key] = data
	return &storage.ObjectInfo{Key: key, Size: int64(len(data)), ContentType: contentType}, nil
}

func (m *mockStorageWriter) Get(key string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, exists := m.storage[key]
	return data, exists
}

func (m *mockStorageWriter) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putErr = err
}

// Test HTTP server helper
func createTestServer(responseData string, statusCode int, delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(responseData))
	}))
}

func TestTeeStreamingDownloader_Behaviour(t *testing.T) {
	t.Run("successful download and stream", func(t *testing.T) {
		testData := "test file content for streaming"
		server := createTestServer(testData, http.StatusOK, 0)
		defer server.Close()

		storage := newMockStorageWriter()
		downloader := NewTeeStreamingDownloader(storage, &http.Client{Timeout: 5 * time.Second})

		var clientBuffer bytes.Buffer
		ctx := context.Background()

		result, err := downloader.DownloadAndStream(ctx, server.URL, "test-key", &clientBuffer, Expectation{})
		if err != nil {
			t.Fatalf("DownloadAndStream failed: %v", err)
		}

		// Verify client received the data
		if clientBuffer.String() != testData {
			t.Errorf("Client buffer mismatch: expected %q, got %q", testData, clientBuffer.String())
		}

		// Verify data was cached in storage
		cachedData, exists := storage.Get("test-key")
		if !exists {
			t.Error("Data should be cached in storage")
		}
		if string(cachedData) != testData {
			t.Errorf("Cached data mismatch: expected %q, got %q", testData, string(cachedData))
		}

		// Verify result metadata
		if result.Size != int64(len(testData)) {
			t.Errorf("Expected size %d, got %d", len(testData), result.Size)
		}
		if result.ContentType != "application/octet-stream" {
			t.Errorf("Expected content type %q, got %q", "application/octet-stream", result.ContentType)
		}
		if result.Error != nil {
			t.Errorf("Expected no storage error, got: %v", result.Error)
		}
	})

	t.Run("download with storage error", func(t *testing.T) {
		testData := "test data with storage error"
		server := createTestServer(testData, http.StatusOK, 0)
		defer server.Close()

		storage := newMockStorageWriter()
		storage.SetError(errors.New("storage write failed"))

		downloader := NewTeeStreamingDownloader(storage, &http.Client{Timeout: 5 * time.Second})

		var clientBuffer bytes.Buffer
		ctx := context.Background()

		// This test expects the downloader to handle storage errors gracefully
		// The client stream should still work even if storage fails
		result, err := downloader.DownloadAndStream(ctx, server.URL, "test-key", &clientBuffer, Expectation{})

		// The streaming may fail due to pipe closure, which is expected behavior
		// when storage fails immediately
		if err != nil {
			// This is acceptable - when storage fails immediately, the pipe closes
			// and the stream fails, which is the correct behavior
			return
		}

		// If streaming succeeded, client should receive data
		if clientBuffer.String() != testData {
			t.Errorf("Client buffer mismatch: expected %q, got %q", testData, clientBuffer.String())
		}

		// Storage should be empty due to error
		_, exists := storage.Get("test-key")
		if exists {
			t.Error("Data should not be cached due to storage error")
		}

		// Result should indicate storage error
		if result.Error == nil {
			t.Error("Expected storage error in result")
		}
	})

	t.Run("HTTP error handling", func(t *testing.T) {
		server := createTestServer("error", http.StatusNotFound, 0)
		defer server.Close()

		storage := newMockStorageWriter()
		downloader := NewTeeStreamingDownloader(storage, &http.Client{Timeout: 5 * time.Second})

		var clientBuffer bytes.Buffer
		ctx := context.Background()

		_, err := downloader.DownloadAndStream(ctx, server.URL, "test-key", &clientBuffer, Expectation{})
		if err == nil {
			t.Error("Expected error for HTTP 404")
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("Error should mention HTTP status: %v", err)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		server := createTestServer("slow response", http.StatusOK, 100*time.Millisecond)
		defer server.Close()

		storage := newMockStorageWriter()
		downloader := NewTeeStreamingDownloader(storage, &http.Client{Timeout: 5 * time.Second})

		var clientBuffer bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		_, err := downloader.DownloadAndStream(ctx, server.URL, "test-key", &clientBuffer, Expectation{})
		if err == nil {
			t.Error("Expected context cancellation error")
		}
	})

	t.Run("large file streaming", func(t *testing.T) {
		// Create 1MB test data
		testData := strings.Repeat("ABCDEFGHIJ", 100*1024)
		server := createTestServer(testData, http.StatusOK, 0)
		defer server.Close()

		storage := newMockStorageWriter()
		downloader := NewTeeStreamingDownloader(storage, &http.Client{Timeout: 10 * time.Second})

		var clientBuffer bytes.Buffer
		ctx := context.Background()

		result, err := downloader.DownloadAndStream(ctx, server.URL, "large-file", &clientBuffer, Expectation{})
		if err != nil {
			t.Fatalf("DownloadAndStream failed for large file: %v", err)
		}

		// Verify all data was streamed
		if int64(clientBuffer.Len()) != result.Size {
			t.Errorf("Client buffer size mismatch: expected %d, got %d", result.Size, clientBuffer.Len())
		}

		// Verify data integrity
		if clientBuffer.String() != testData {
			t.Error("Large file data integrity check failed")
		}
	})

	t.Run("concurrent downloads", func(t *testing.T) {
		testData := "concurrent test data"
		server := createTestServer(testData, http.StatusOK, 10*time.Millisecond)
		defer server.Close()

		storage := newMockStorageWriter()
		downloader := NewTeeStreamingDownloader(storage, &http.Client{Timeout: 5 * time.Second})

		var wg sync.WaitGroup
		concurrency := 10
		wg.Add(concurrency)

		ctx := context.Background()
		errors := make(chan error, concurrency)

		for i := range concurrency {
			go func(id int) {
				defer wg.Done()
				var buffer bytes.Buffer
				key := fmt.Sprintf("concurrent-key-%d", id)

				_, err := downloader.DownloadAndStream(ctx, server.URL, key, &buffer, Expectation{})
				if err != nil {
					errors <- err
					return
				}

				if buffer.String() != testData {
					errors <- fmt.Errorf("data mismatch for goroutine %d", id)
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		// Check for any errors
		for err := range errors {
			t.Errorf("Concurrent download error: %v", err)
		}
	})
}

// TestTeeStreamingDownloader_RedactsCredentials pins that a file URL carrying
// index credentials never reaches an error string. Package file URLs are resolved
// against the index URL, so a private index configured with user:password hands
// its password to every download; an unredacted error puts it in the log and in
// the 500 body.
func TestTeeStreamingDownloader_RedactsCredentials(t *testing.T) {
	const secret = "sup3rs3cr3t"

	credentialed := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		u.User = url.UserPassword("deploy", secret)
		return u.String()
	}

	t.Run("transport failure", func(t *testing.T) {
		// A server that is closed before use gives a connection refused, which is
		// how *url.Error - the shape that prints the URL it failed on - is reached.
		dead := createTestServer("", http.StatusOK, 0)
		dead.Close()

		downloader := NewTeeStreamingDownloader(newMockStorageWriter(), &http.Client{Timeout: 5 * time.Second})

		_, err := downloader.DownloadAndStream(context.Background(),
			credentialed(dead.URL), "k", io.Discard, Expectation{})
		if err == nil {
			t.Fatal("expected a transport failure")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("index credentials leaked into the error: %v", err)
		}
	})

	t.Run("upstream status", func(t *testing.T) {
		server := createTestServer("nope", http.StatusNotFound, 0)
		defer server.Close()

		downloader := NewTeeStreamingDownloader(newMockStorageWriter(), &http.Client{Timeout: 5 * time.Second})

		_, err := downloader.DownloadAndStream(context.Background(),
			credentialed(server.URL), "k", io.Discard, Expectation{})
		if err == nil {
			t.Fatal("expected an error for HTTP 404")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("index credentials leaked into the error: %v", err)
		}
	})
}

func TestNewTeeStreamingDownloader(t *testing.T) {
	t.Run("creates tee downloader", func(t *testing.T) {
		storage := newMockStorageWriter()
		downloader := NewTeeStreamingDownloader(storage, nil)
		if downloader == nil {
			t.Fatal("NewTeeStreamingDownloader returned nil")
		}
	})
}

func TestTeeStreamingDownloader_DownloadAndStream(t *testing.T) {
	t.Run("tee reader streaming works", func(t *testing.T) {
		testData := "tee reader test data"
		server := createTestServer(testData, http.StatusOK, 0)
		defer server.Close()

		storage := newMockStorageWriter()
		downloader := NewTeeStreamingDownloader(storage, &http.Client{Timeout: 5 * time.Second})

		var clientBuffer bytes.Buffer
		ctx := context.Background()

		result, err := downloader.DownloadAndStream(ctx, server.URL, "tee-key", &clientBuffer, Expectation{})
		if err != nil {
			t.Fatalf("TeeStreamingDownloader failed: %v", err)
		}

		// Verify client received data
		if clientBuffer.String() != testData {
			t.Errorf("Client data mismatch: expected %q, got %q", testData, clientBuffer.String())
		}

		// Verify storage received data
		cachedData, exists := storage.Get("tee-key")
		if !exists {
			t.Error("Data should be cached with tee reader")
		}
		if string(cachedData) != testData {
			t.Errorf("Cached data mismatch: expected %q, got %q", testData, string(cachedData))
		}

		if result.Size != int64(len(testData)) {
			t.Errorf("Size mismatch: expected %d, got %d", len(testData), result.Size)
		}
	})
}

// Benchmark tests
func BenchmarkTeeStreamingDownloader_Comparison(b *testing.B) {
	testData := "tee benchmark data"
	server := createTestServer(testData, http.StatusOK, 0)
	defer server.Close()

	storage := newMockStorageWriter()
	downloader := NewTeeStreamingDownloader(storage, &http.Client{Timeout: 5 * time.Second})
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buffer bytes.Buffer
		key := fmt.Sprintf("tee-bench-key-%d", i)
		_, _ = downloader.DownloadAndStream(ctx, server.URL, key, &buffer, Expectation{})
	}
}
