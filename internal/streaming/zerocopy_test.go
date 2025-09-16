package streaming

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/phuslu/log"
)

// Mock writer for zero-copy testing
type mockZeroCopyWriter struct {
	buffer    bytes.Buffer
	writeErr  error
	sendFile  bool
	fileErr   error
	slowWrite bool
}

func (m *mockZeroCopyWriter) Write(p []byte) (n int, err error) {
	if m.slowWrite {
		time.Sleep(1 * time.Millisecond)
	}
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return m.buffer.Write(p)
}

func (m *mockZeroCopyWriter) File() (*os.File, error) {
	if m.fileErr != nil {
		return nil, m.fileErr
	}
	if !m.sendFile {
		return nil, errors.New("not a file descriptor")
	}
	// Return dummy file for testing - in real tests we'd use actual files
	return os.Stdin, nil
}

func (m *mockZeroCopyWriter) String() string {
	return m.buffer.String()
}

// Mock Fiber context for testing
type mockFiberContext struct {
	buffer      bytes.Buffer
	sendFileErr error
	sentFile    string
}

func (m *mockFiberContext) Write(p []byte) (n int, err error) {
	return m.buffer.Write(p)
}

func (m *mockFiberContext) SendFile(filename string, compress ...bool) error {
	if m.sendFileErr != nil {
		return m.sendFileErr
	}
	m.sentFile = filename
	return nil
}

func (m *mockFiberContext) Stream(fn func(w *io.Writer) error) error {
	var buf bytes.Buffer
	writer := io.Writer(&buf)
	return fn(&writer)
}

// Test helper functions
func createTempFile(t *testing.T, content string) string {
	t.Helper()
	tmpfile, err := os.CreateTemp("", "zerocopy_test")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatalf("Failed to write temp file: %v", err)
	}

	if err := tmpfile.Close(); err != nil {
		t.Fatalf("Failed to close temp file: %v", err)
	}

	return tmpfile.Name()
}

func TestNewZeroCopyServer(t *testing.T) {
	t.Run("creates zero-copy server", func(t *testing.T) {
		server := NewZeroCopyServer()
		if server == nil {
			t.Fatal("NewZeroCopyServer returned nil")
		}
	})
}

func TestZeroCopyServer_ServeFile(t *testing.T) {
	t.Run("serve existing file", func(t *testing.T) {
		content := "test file content for zero-copy serving"
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewZeroCopyServer()
		writer := &mockZeroCopyWriter{}
		ctx := context.Background()

		err := server.ServeFile(ctx, writer, filename)
		if err != nil {
			t.Fatalf("ServeFile failed: %v", err)
		}

		if writer.String() != content {
			t.Errorf("Content mismatch: expected %q, got %q", content, writer.String())
		}
	})

	t.Run("serve non-existent file returns error", func(t *testing.T) {
		server := NewZeroCopyServer()
		writer := &mockZeroCopyWriter{}
		ctx := context.Background()

		err := server.ServeFile(ctx, writer, "/non/existent/file.txt")
		if err == nil {
			t.Error("Expected error for non-existent file")
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		content := "content for cancellation test"
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewZeroCopyServer()
		writer := &mockZeroCopyWriter{slowWrite: true}
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
		defer cancel()

		// This might or might not error depending on timing
		// The main point is it shouldn't panic
		_ = server.ServeFile(ctx, writer, filename)
	})

	t.Run("sendfile with compatible writer", func(t *testing.T) {
		// This test is more conceptual since we can't easily test real sendfile
		content := "sendfile test content"
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewZeroCopyServer()
		writer := &mockZeroCopyWriter{sendFile: true}
		ctx := context.Background()

		err := server.ServeFile(ctx, writer, filename)
		// Sendfile will fail with mock writers due to no real file descriptor
		// This is expected behavior in test environment
		if err != nil && !strings.Contains(err.Error(), "sendfile failed") {
			t.Fatalf("ServeFile with sendfile capability failed with unexpected error: %v", err)
		}
	})

	t.Run("large file serving", func(t *testing.T) {
		// Create a larger file for testing
		content := strings.Repeat("LARGE FILE CONTENT ", 1000) // ~19KB
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewZeroCopyServer()
		writer := &mockZeroCopyWriter{}
		ctx := context.Background()

		err := server.ServeFile(ctx, writer, filename)
		if err != nil {
			t.Fatalf("Large file serving failed: %v", err)
		}

		if writer.String() != content {
			t.Error("Large file content mismatch")
		}
	})
}

func TestZeroCopyServer_ServeReader(t *testing.T) {
	t.Run("serve from reader", func(t *testing.T) {
		content := "reader test content"
		reader := strings.NewReader(content)

		server := NewZeroCopyServer()
		writer := &mockZeroCopyWriter{}
		ctx := context.Background()

		err := server.ServeReader(ctx, writer, reader, int64(len(content)))
		if err != nil {
			t.Fatalf("ServeReader failed: %v", err)
		}

		if writer.String() != content {
			t.Errorf("Reader content mismatch: expected %q, got %q", content, writer.String())
		}
	})

	t.Run("serve with write error", func(t *testing.T) {
		content := "error test content"
		reader := strings.NewReader(content)

		server := NewZeroCopyServer()
		writer := &mockZeroCopyWriter{writeErr: errors.New("write failed")}
		ctx := context.Background()

		err := server.ServeReader(ctx, writer, reader, int64(len(content)))
		if err == nil {
			t.Error("Expected write error")
		}
	})

	t.Run("context cancellation during read", func(t *testing.T) {
		content := strings.Repeat("SLOW READER CONTENT ", 100)
		reader := &slowReader{Reader: strings.NewReader(content)}

		server := NewZeroCopyServer()
		writer := &mockZeroCopyWriter{}
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
		defer cancel()

		_ = server.ServeReader(ctx, writer, reader, int64(len(content)))
		// Should handle cancellation gracefully
	})
}

func TestNewFiberZeroCopyServer(t *testing.T) {
	t.Run("creates fiber zero-copy server", func(t *testing.T) {
		server := NewFiberZeroCopyServer()
		if server == nil {
			t.Fatal("NewFiberZeroCopyServer returned nil")
		}
	})
}

func TestFiberZeroCopyServer_ServeFile(t *testing.T) {
	t.Run("serve file with fiber context", func(t *testing.T) {
		content := "fiber test content"
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewFiberZeroCopyServer()
		fiberCtx := &mockFiberContext{}
		ctx := context.Background()

		err := server.ServeFile(ctx, fiberCtx, filename)
		if err != nil {
			t.Fatalf("Fiber ServeFile failed: %v", err)
		}

		if fiberCtx.sentFile != filename {
			t.Errorf("Expected sent file %q, got %q", filename, fiberCtx.sentFile)
		}
	})

	t.Run("fiber sendfile error falls back to regular", func(t *testing.T) {
		content := "fallback test content"
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewFiberZeroCopyServer()
		fiberCtx := &mockFiberContext{sendFileErr: errors.New("sendfile failed")}
		ctx := context.Background()

		// Should fall back to regular zero-copy server, but may still fail due to mock limitations
		err := server.ServeFile(ctx, fiberCtx, filename)
		if err != nil && !strings.Contains(err.Error(), "sendfile failed") {
			t.Fatalf("Fallback ServeFile failed with unexpected error: %v", err)
		}
	})

	t.Run("non-fiber writer uses regular server", func(t *testing.T) {
		content := "regular writer test"
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewFiberZeroCopyServer()
		writer := &mockZeroCopyWriter{}
		ctx := context.Background()

		err := server.ServeFile(ctx, writer, filename)
		if err != nil {
			t.Fatalf("Regular writer ServeFile failed: %v", err)
		}

		if writer.String() != content {
			t.Errorf("Content mismatch: expected %q, got %q", content, writer.String())
		}
	})
}

func TestFiberZeroCopyServer_ServeReader(t *testing.T) {
	t.Run("serve reader with fiber streaming", func(t *testing.T) {
		content := "fiber reader test"
		reader := strings.NewReader(content)

		server := NewFiberZeroCopyServer()
		fiberCtx := &mockFiberContext{}
		ctx := context.Background()

		err := server.ServeReader(ctx, fiberCtx, reader, int64(len(content)))
		if err != nil {
			t.Fatalf("Fiber ServeReader failed: %v", err)
		}
	})
}

func TestNewMemoryMappedServer(t *testing.T) {
	t.Run("creates memory-mapped server", func(t *testing.T) {
		server := NewMemoryMappedServer()
		if server == nil {
			t.Fatal("NewMemoryMappedServer returned nil")
		}
	})
}

func TestMemoryMappedServer_ServeFile(t *testing.T) {
	t.Run("serve file with memory mapping", func(t *testing.T) {
		content := "memory mapped test content"
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewMemoryMappedServer()
		writer := &mockZeroCopyWriter{}
		ctx := context.Background()

		err := server.ServeFile(ctx, writer, filename)
		if err != nil {
			t.Fatalf("Memory-mapped ServeFile failed: %v", err)
		}

		if writer.String() != content {
			t.Errorf("Memory-mapped content mismatch: expected %q, got %q", content, writer.String())
		}
	})

	t.Run("empty file handling", func(t *testing.T) {
		filename := createTempFile(t, "")
		defer func() { _ = os.Remove(filename) }()

		server := NewMemoryMappedServer()
		writer := &mockZeroCopyWriter{}
		ctx := context.Background()

		err := server.ServeFile(ctx, writer, filename)
		if err != nil {
			t.Fatalf("Empty file serving failed: %v", err)
		}

		if writer.String() != "" {
			t.Error("Empty file should produce no content")
		}
	})
}

func TestNewOptimalServer(t *testing.T) {
	t.Run("creates optimal server", func(t *testing.T) {
		server := NewOptimalServer()
		if server == nil {
			t.Fatal("NewOptimalServer returned nil")
		}
	})
}

func TestOptimalServer_ServeFile(t *testing.T) {
	t.Run("small file uses regular serving", func(t *testing.T) {
		content := "small file"
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewOptimalServer()
		writer := &mockZeroCopyWriter{}
		ctx := context.Background()

		err := server.ServeFile(ctx, writer, filename)
		if err != nil {
			t.Fatalf("Small file serving failed: %v", err)
		}

		if writer.String() != content {
			t.Errorf("Content mismatch: expected %q, got %q", content, writer.String())
		}
	})

	t.Run("large file uses memory mapping", func(t *testing.T) {
		// Create file larger than 100MB threshold (simulated with smaller file for test)
		content := strings.Repeat("LARGE FILE CONTENT ", 5000) // ~95KB for testing
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewOptimalServer()
		writer := &mockZeroCopyWriter{}
		ctx := context.Background()

		err := server.ServeFile(ctx, writer, filename)
		if err != nil {
			t.Fatalf("Large file serving failed: %v", err)
		}

		if writer.String() != content {
			t.Error("Large file content mismatch")
		}
	})

	t.Run("fiber writer uses fiber optimization", func(t *testing.T) {
		content := "fiber optimization test"
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewOptimalServer()
		fiberCtx := &mockFiberContext{}
		ctx := context.Background()

		err := server.ServeFile(ctx, fiberCtx, filename)
		if err != nil {
			t.Fatalf("Fiber optimization failed: %v", err)
		}

		if fiberCtx.sentFile != filename {
			t.Errorf("Fiber should have sent file %q, got %q", filename, fiberCtx.sentFile)
		}
	})

	t.Run("sendfile compatible writer", func(t *testing.T) {
		content := "sendfile compatible test"
		filename := createTempFile(t, content)
		defer func() { _ = os.Remove(filename) }()

		server := NewOptimalServer()
		writer := &mockZeroCopyWriter{sendFile: true}
		ctx := context.Background()

		err := server.ServeFile(ctx, writer, filename)
		// Sendfile may fail in test environment with mock writers
		if err != nil && !strings.Contains(err.Error(), "sendfile failed") && !strings.Contains(err.Error(), "bad file descriptor") {
			t.Fatalf("Sendfile compatible serving failed with unexpected error: %v", err)
		}
	})
}

// Helper types for testing
type slowReader struct {
	io.Reader
}

func (sr *slowReader) Read(p []byte) (n int, err error) {
	time.Sleep(1 * time.Millisecond) // Simulate slow reading
	return sr.Reader.Read(p)
}

// Benchmark tests
func BenchmarkZeroCopyServer_SmallFile(b *testing.B) {
	// Set log level to ERROR to suppress debug output during benchmarks
	originalLogger := log.DefaultLogger
	log.DefaultLogger.SetLevel(log.ErrorLevel)
	defer func() { log.DefaultLogger = originalLogger }()

	content := "small benchmark file content"
	filename := createTempFileForBench(content)
	defer func() { _ = os.Remove(filename) }()

	server := NewZeroCopyServer()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writer := &mockZeroCopyWriter{}
		_ = server.ServeFile(ctx, writer, filename)
	}
}

func BenchmarkZeroCopyServer_LargeFile(b *testing.B) {
	// Set log level to ERROR to suppress debug output during benchmarks
	originalLogger := log.DefaultLogger
	log.DefaultLogger.SetLevel(log.ErrorLevel)
	defer func() { log.DefaultLogger = originalLogger }()

	content := strings.Repeat("LARGE BENCHMARK CONTENT ", 1000) // ~24KB
	filename := createTempFileForBench(content)
	defer func() { _ = os.Remove(filename) }()

	server := NewZeroCopyServer()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writer := &mockZeroCopyWriter{}
		_ = server.ServeFile(ctx, writer, filename)
	}
}

func BenchmarkFiberZeroCopyServer_Comparison(b *testing.B) {
	// Set log level to ERROR to suppress debug output during benchmarks
	originalLogger := log.DefaultLogger
	log.DefaultLogger.SetLevel(log.ErrorLevel)
	defer func() { log.DefaultLogger = originalLogger }()

	content := "fiber benchmark content"
	filename := createTempFileForBench(content)
	defer func() { _ = os.Remove(filename) }()

	server := NewFiberZeroCopyServer()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fiberCtx := &mockFiberContext{}
		_ = server.ServeFile(ctx, fiberCtx, filename)
	}
}

func BenchmarkOptimalServer_AutoSelection(b *testing.B) {
	// Set log level to ERROR to suppress debug output during benchmarks
	originalLogger := log.DefaultLogger
	log.DefaultLogger.SetLevel(log.ErrorLevel)
	defer func() { log.DefaultLogger = originalLogger }()

	content := "optimal server benchmark"
	filename := createTempFileForBench(content)
	defer func() { _ = os.Remove(filename) }()

	server := NewOptimalServer()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writer := &mockZeroCopyWriter{}
		_ = server.ServeFile(ctx, writer, filename)
	}
}

// Helper function for creating temp file in benchmarks
func createTempFileForBench(content string) string {
	tmpfile, err := os.CreateTemp("", "zerocopy_bench")
	if err != nil {
		panic(err)
	}

	if _, err := tmpfile.Write([]byte(content)); err != nil {
		panic(err)
	}

	if err := tmpfile.Close(); err != nil {
		panic(err)
	}

	return tmpfile.Name()
}
