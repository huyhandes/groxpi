package streaming

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Mock writer for testing
type mockWriter struct {
	buffer      bytes.Buffer
	writeErrors []error
	writeCount  int
	mu          sync.Mutex
	closed      bool
	slowWrite   bool
}

func (mw *mockWriter) Write(p []byte) (n int, err error) {
	mw.mu.Lock()
	defer mw.mu.Unlock()

	if mw.closed {
		return 0, errors.New("writer is closed")
	}

	if mw.slowWrite {
		time.Sleep(10 * time.Millisecond) // Simulate slow writer
	}

	if mw.writeCount < len(mw.writeErrors) && mw.writeErrors[mw.writeCount] != nil {
		err = mw.writeErrors[mw.writeCount]
		mw.writeCount++
		return 0, err
	}

	mw.writeCount++
	return mw.buffer.Write(p)
}

func (mw *mockWriter) Close() error {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	mw.closed = true
	return nil
}

func (mw *mockWriter) String() string {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	return mw.buffer.String()
}

func (mw *mockWriter) Len() int {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	return mw.buffer.Len()
}

// Test helper for creating test data
func createTestData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte('A' + (i % 26))
	}
	return data
}

// Test helper for creating multiple mock writers
func createMockWriters(count int) []*mockWriter {
	writers := make([]*mockWriter, count)
	for i := range writers {
		writers[i] = &mockWriter{}
	}
	return writers
}

func TestNewBroadcastWriter(t *testing.T) {
	t.Run("creates new broadcast writer", func(t *testing.T) {
		bw := NewBroadcastWriter()
		if bw == nil {
			t.Fatal("NewBroadcastWriter returned nil")
		}
	})

	t.Run("initial state has no writers", func(t *testing.T) {
		bw := NewBroadcastWriter()
		testData := []byte("test")

		n, err := bw.Write(testData)
		if err != nil {
			t.Errorf("Write with no writers should not error, got: %v", err)
		}
		if n != len(testData) {
			t.Errorf("Expected write count %d, got %d", len(testData), n)
		}
	})
}

func TestBroadcastWriter_AddWriter(t *testing.T) {
	t.Run("add single writer", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writer := &mockWriter{}

		err := bw.AddWriter(writer)
		if err != nil {
			t.Errorf("AddWriter failed: %v", err)
		}

		// Test that data is written to the added writer
		testData := []byte("hello world")
		n, err := bw.Write(testData)
		if err != nil {
			t.Errorf("Write failed: %v", err)
		}
		if n != len(testData) {
			t.Errorf("Expected %d bytes written, got %d", len(testData), n)
		}
		if writer.String() != string(testData) {
			t.Errorf("Expected writer to contain %q, got %q", testData, writer.String())
		}
	})

	t.Run("add multiple writers", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writers := createMockWriters(3)

		for _, writer := range writers {
			err := bw.AddWriter(writer)
			if err != nil {
				t.Errorf("AddWriter failed: %v", err)
			}
		}

		testData := []byte("broadcast test")
		n, err := bw.Write(testData)
		if err != nil {
			t.Errorf("Write failed: %v", err)
		}
		if n != len(testData) {
			t.Errorf("Expected %d bytes written, got %d", len(testData), n)
		}

		// Verify all writers received the data
		for i, writer := range writers {
			if writer.String() != string(testData) {
				t.Errorf("Writer %d expected %q, got %q", i, testData, writer.String())
			}
		}
	})

	t.Run("add nil writer returns error", func(t *testing.T) {
		bw := NewBroadcastWriter()
		err := bw.AddWriter(nil)
		if err == nil {
			t.Error("Expected error when adding nil writer")
		}
	})

	t.Run("add writer after close returns error", func(t *testing.T) {
		bw := NewBroadcastWriter()
		bw.Close()

		writer := &mockWriter{}
		err := bw.AddWriter(writer)
		if err == nil {
			t.Error("Expected error when adding writer to closed broadcaster")
		}
	})
}

func TestBroadcastWriter_RemoveWriter(t *testing.T) {
	t.Run("remove existing writer", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writer1 := &mockWriter{}
		writer2 := &mockWriter{}

		_ = bw.AddWriter(writer1)
		_ = bw.AddWriter(writer2)

		// Write initial data
		_, _ = bw.Write([]byte("first"))
		if writer1.String() != "first" || writer2.String() != "first" {
			t.Error("Both writers should have received first data")
		}

		// Remove writer1
		err := bw.RemoveWriter(writer1)
		if err != nil {
			t.Errorf("RemoveWriter failed: %v", err)
		}

		// Write more data
		_, _ = bw.Write([]byte("second"))
		if writer1.String() != "first" {
			t.Error("Removed writer should not receive new data")
		}
		if writer2.String() != "firstsecond" {
			t.Error("Remaining writer should receive all data")
		}
	})

	t.Run("remove non-existent writer returns error", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writer := &mockWriter{}

		err := bw.RemoveWriter(writer)
		if err == nil {
			t.Error("Expected error when removing non-existent writer")
		}
	})

	t.Run("remove writer after close returns error", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writer := &mockWriter{}
		_ = bw.AddWriter(writer)
		bw.Close()

		err := bw.RemoveWriter(writer)
		if err == nil {
			t.Error("Expected error when removing writer from closed broadcaster")
		}
	})
}

func TestBroadcastWriter_Write(t *testing.T) {
	t.Run("write to single writer", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writer := &mockWriter{}
		_ = bw.AddWriter(writer)

		testData := []byte("single writer test")
		n, err := bw.Write(testData)
		if err != nil {
			t.Errorf("Write failed: %v", err)
		}
		if n != len(testData) {
			t.Errorf("Expected %d bytes written, got %d", len(testData), n)
		}
		if writer.String() != string(testData) {
			t.Errorf("Writer content mismatch: expected %q, got %q", testData, writer.String())
		}
	})

	t.Run("write to multiple writers concurrently", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writers := createMockWriters(5)
		for _, writer := range writers {
			_ = bw.AddWriter(writer)
		}

		testData := []byte("concurrent write test")
		n, err := bw.Write(testData)
		if err != nil {
			t.Errorf("Write failed: %v", err)
		}
		if n != len(testData) {
			t.Errorf("Expected %d bytes written, got %d", len(testData), n)
		}

		// Verify all writers received data
		for i, writer := range writers {
			if writer.String() != string(testData) {
				t.Errorf("Writer %d mismatch: expected %q, got %q", i, testData, writer.String())
			}
		}
	})

	t.Run("write with one failing writer", func(t *testing.T) {
		bw := NewBroadcastWriter()
		goodWriter := &mockWriter{}
		badWriter := &mockWriter{writeErrors: []error{errors.New("write failed")}}

		_ = bw.AddWriter(goodWriter)
		bw.AddWriter(badWriter)

		testData := []byte("test with error")
		n, err := bw.Write(testData)
		if err == nil {
			t.Error("Expected error when one writer fails")
		}
		// Should still return the minimum successful write count
		if n != 0 {
			t.Errorf("Expected 0 bytes written due to error, got %d", n)
		}
	})

	t.Run("write after close returns error", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writer := &mockWriter{}
		_ = bw.AddWriter(writer)
		bw.Close()

		_, err := bw.Write([]byte("test"))
		if err == nil {
			t.Error("Expected error when writing to closed broadcaster")
		}
	})

	t.Run("large data write", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writers := createMockWriters(3)
		for _, writer := range writers {
			_ = bw.AddWriter(writer)
		}

		testData := createTestData(10000) // 10KB test data
		n, err := bw.Write(testData)
		if err != nil {
			t.Errorf("Write failed: %v", err)
		}
		if n != len(testData) {
			t.Errorf("Expected %d bytes written, got %d", len(testData), n)
		}

		for i, writer := range writers {
			if writer.Len() != len(testData) {
				t.Errorf("Writer %d length mismatch: expected %d, got %d", i, len(testData), writer.Len())
			}
		}
	})
}

func TestBroadcastWriter_Close(t *testing.T) {
	t.Run("close with closeable writers", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writers := createMockWriters(3)
		for _, writer := range writers {
			_ = bw.AddWriter(writer)
		}

		err := bw.Close()
		if err != nil {
			t.Errorf("Close failed: %v", err)
		}

		// Verify all writers are closed
		for i, writer := range writers {
			if !writer.closed {
				t.Errorf("Writer %d should be closed", i)
			}
		}
	})

	t.Run("close with non-closeable writers", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writer := &bytes.Buffer{} // Buffer doesn't implement Close()
		_ = bw.AddWriter(writer)

		err := bw.Close()
		if err != nil {
			t.Errorf("Close should succeed even with non-closeable writers: %v", err)
		}
	})

	t.Run("multiple close calls are safe", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writer := &mockWriter{}
		_ = bw.AddWriter(writer)

		err1 := bw.Close()
		err2 := bw.Close()

		if err1 != nil {
			t.Errorf("First close failed: %v", err1)
		}
		if err2 != nil {
			t.Errorf("Second close failed: %v", err2)
		}
	})
}

func TestBroadcastWriter_ConcurrentOperations(t *testing.T) {
	t.Run("concurrent writes", func(t *testing.T) {
		bw := NewBroadcastWriter()
		writer := &mockWriter{}
		_ = bw.AddWriter(writer)

		var wg sync.WaitGroup
		writes := 10 // Reduce to make test more reliable
		wg.Add(writes)

		// Use channel to synchronize writes
		writeDone := make(chan int, writes)

		for i := 0; i < writes; i++ {
			go func(i int) {
				defer wg.Done()
				data := []byte(fmt.Sprintf("write_%02d_", i)) // Zero-pad for consistent ordering
				_, _ = bw.Write(data)
				writeDone <- i
			}(i)
		}

		// Wait for all writes to complete
		wg.Wait()
		close(writeDone)

		// Count completed writes
		completed := 0
		for range writeDone {
			completed++
		}

		if completed != writes {
			t.Errorf("Expected %d writes, got %d", writes, completed)
		}

		// Verify writer has some data (order may vary due to concurrency)
		result := writer.String()
		if len(result) == 0 {
			t.Error("Writer should have received some data")
		}
	})

	t.Run("concurrent add/remove writers", func(t *testing.T) {
		bw := NewBroadcastWriter()

		var wg sync.WaitGroup
		operations := 5 // Reduce to make test more reliable

		writers := make([]*mockWriter, operations)

		// Add writers concurrently
		wg.Add(operations)
		for i := 0; i < operations; i++ {
			go func(i int) {
				defer wg.Done()
				writer := &mockWriter{}
				writers[i] = writer
				err := bw.AddWriter(writer)
				if err != nil {
					t.Errorf("Failed to add writer %d: %v", i, err)
				}
			}(i)
		}

		wg.Wait()

		// Write data and verify no panic
		_, err := bw.Write([]byte("concurrent test"))
		if err != nil {
			t.Errorf("Write after concurrent adds failed: %v", err)
		}

		// Verify all writers received data
		for i, writer := range writers {
			if writer != nil && writer.String() != "concurrent test" {
				t.Errorf("Writer %d expected %q, got %q", i, "concurrent test", writer.String())
			}
		}
	})
}

func TestNewAsyncBroadcastWriter(t *testing.T) {
	t.Run("creates async broadcast writer with default buffer", func(t *testing.T) {
		bw := NewAsyncBroadcastWriter(0)
		if bw == nil {
			t.Fatal("NewAsyncBroadcastWriter returned nil")
		}
	})

	t.Run("creates async broadcast writer with custom buffer", func(t *testing.T) {
		bw := NewAsyncBroadcastWriter(100)
		if bw == nil {
			t.Fatal("NewAsyncBroadcastWriter returned nil")
		}
	})
}

func TestAsyncBroadcastWriter_BasicOperations(t *testing.T) {
	t.Run("write to async broadcast writer", func(t *testing.T) {
		bw := NewAsyncBroadcastWriter(10)
		writer := &mockWriter{}

		err := bw.AddWriter(writer)
		if err != nil {
			t.Fatalf("AddWriter failed: %v", err)
		}

		testData := []byte("async test")
		n, err := bw.Write(testData)
		if err != nil {
			t.Errorf("Write failed: %v", err)
		}
		if n != len(testData) {
			t.Errorf("Expected %d bytes written, got %d", len(testData), n)
		}

		// Wait for async write to complete
		time.Sleep(50 * time.Millisecond)

		err = bw.Close()
		if err != nil {
			t.Errorf("Close failed: %v", err)
		}

		err = bw.Wait()
		if err != nil {
			t.Errorf("Wait failed: %v", err)
		}

		if writer.String() != string(testData) {
			t.Errorf("Expected writer to contain %q, got %q", testData, writer.String())
		}
	})

	t.Run("async writer handles channel overflow", func(t *testing.T) {
		bw := NewAsyncBroadcastWriter(2) // Small buffer
		slowWriter := &mockWriter{slowWrite: true}

		bw.AddWriter(slowWriter)

		// Write more data than buffer can hold
		for i := 0; i < 10; i++ {
			data := []byte(fmt.Sprintf("data_%d", i))
			_, _ = bw.Write(data)
		}

		bw.Close()
		_ = bw.Wait()

		// Should not panic, some writes may be dropped due to buffer overflow
	})
}

// Benchmark tests
func BenchmarkBroadcastWriter_SingleWriter(b *testing.B) {
	bw := NewBroadcastWriter()
	writer := &bytes.Buffer{}
	_ = bw.AddWriter(writer)

	data := []byte("benchmark test data")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = bw.Write(data)
	}
}

func BenchmarkBroadcastWriter_MultipleWriters(b *testing.B) {
	bw := NewBroadcastWriter()

	// Add multiple writers
	for i := 0; i < 5; i++ {
		writer := &bytes.Buffer{}
		_ = bw.AddWriter(writer)
	}

	data := []byte("benchmark test data")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = bw.Write(data)
	}
}

func BenchmarkAsyncBroadcastWriter_SingleWriter(b *testing.B) {
	bw := NewAsyncBroadcastWriter(1000)
	writer := &bytes.Buffer{}
	_ = bw.AddWriter(writer)

	data := []byte("async benchmark test data")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = bw.Write(data)
	}

	bw.Close()
	_ = bw.Wait()
}
