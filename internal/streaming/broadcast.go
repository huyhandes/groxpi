package streaming

import (
	"errors"
	"io"
	"sync"
)

// broadcastWriter implements BroadcastWriter interface
type broadcastWriter struct {
	mu      sync.RWMutex
	writers []io.Writer
	closed  bool
	wg      sync.WaitGroup
	err     error
}

// NewBroadcastWriter creates a new BroadcastWriter
func NewBroadcastWriter() BroadcastWriter {
	return &broadcastWriter{
		writers: make([]io.Writer, 0),
	}
}

// Write writes data to all registered writers
func (bw *broadcastWriter) Write(p []byte) (n int, err error) {
	bw.mu.RLock()
	defer bw.mu.RUnlock()

	if bw.closed {
		return 0, errors.New("broadcast writer is closed")
	}

	if len(bw.writers) == 0 {
		return len(p), nil
	}

	// Write to all writers sequentially for reliability in tests
	minN := len(p)
	var firstErr error

	for _, writer := range bw.writers {
		n, err := writer.Write(p)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if n < minN {
			minN = n
		}
		// If any writer fails, return immediately
		if err != nil {
			return 0, firstErr
		}
	}

	return minN, firstErr
}

// AddWriter adds a writer to receive broadcasted data
func (bw *broadcastWriter) AddWriter(w io.Writer) error {
	if w == nil {
		return errors.New("writer cannot be nil")
	}

	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.closed {
		return errors.New("broadcast writer is closed")
	}

	bw.writers = append(bw.writers, w)
	return nil
}

// RemoveWriter removes a writer from broadcast
func (bw *broadcastWriter) RemoveWriter(w io.Writer) error {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.closed {
		return errors.New("broadcast writer is closed")
	}

	for i, writer := range bw.writers {
		// Use pointer comparison for io.Writer interface
		if writer == w {
			// Remove writer by replacing with last element and truncating
			bw.writers[i] = bw.writers[len(bw.writers)-1]
			bw.writers = bw.writers[:len(bw.writers)-1]
			return nil
		}
	}

	return errors.New("writer not found")
}

// Close closes all writers and stops broadcasting
func (bw *broadcastWriter) Close() error {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	if bw.closed {
		return nil
	}

	bw.closed = true

	var lastErr error
	for _, writer := range bw.writers {
		if closer, ok := writer.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				lastErr = err
			}
		}
	}

	bw.writers = nil
	return lastErr
}

// Wait waits for all writers to finish
func (bw *broadcastWriter) Wait() error {
	bw.wg.Wait()
	bw.mu.RLock()
	err := bw.err
	bw.mu.RUnlock()
	return err
}

// AsyncBroadcastWriter provides asynchronous broadcasting with buffered channels
type asyncBroadcastWriter struct {
	mu        sync.RWMutex
	writers   []chan []byte
	closed    bool
	wg        sync.WaitGroup
	bufferSize int
}

// NewAsyncBroadcastWriter creates a new asynchronous BroadcastWriter
func NewAsyncBroadcastWriter(bufferSize int) BroadcastWriter {
	if bufferSize <= 0 {
		bufferSize = 1024 // Default buffer size
	}
	
	return &asyncBroadcastWriter{
		writers:    make([]chan []byte, 0),
		bufferSize: bufferSize,
	}
}

// Write writes data to all registered writers asynchronously
func (abw *asyncBroadcastWriter) Write(p []byte) (n int, err error) {
	abw.mu.RLock()
	defer abw.mu.RUnlock()

	if abw.closed {
		return 0, errors.New("async broadcast writer is closed")
	}

	if len(abw.writers) == 0 {
		return len(p), nil
	}

	// Make a copy of the data since it will be used asynchronously
	data := make([]byte, len(p))
	copy(data, p)

	// Send to all channels non-blocking
	for _, ch := range abw.writers {
		select {
		case ch <- data:
		default:
			// Channel is full, skip this writer to avoid blocking
		}
	}

	return len(p), nil
}

// AddWriter adds a writer to receive broadcasted data
func (abw *asyncBroadcastWriter) AddWriter(w io.Writer) error {
	if w == nil {
		return errors.New("writer cannot be nil")
	}

	abw.mu.Lock()
	defer abw.mu.Unlock()

	if abw.closed {
		return errors.New("async broadcast writer is closed")
	}

	ch := make(chan []byte, abw.bufferSize)
	abw.writers = append(abw.writers, ch)

	// Start goroutine to read from channel and write to writer
	abw.wg.Add(1)
	go func() {
		defer abw.wg.Done()
		for data := range ch {
			if _, err := w.Write(data); err != nil {
				// Log error but continue with other writers
				return
			}
		}
	}()

	return nil
}

// RemoveWriter removes a writer from broadcast
func (abw *asyncBroadcastWriter) RemoveWriter(w io.Writer) error {
	abw.mu.Lock()
	defer abw.mu.Unlock()

	if abw.closed {
		return errors.New("async broadcast writer is closed")
	}

	// Note: For async writer, we can't easily match the original writer
	// This is a limitation of the async approach
	return errors.New("removing writers not supported in async mode")
}

// Close closes all channels and stops broadcasting
func (abw *asyncBroadcastWriter) Close() error {
	abw.mu.Lock()
	defer abw.mu.Unlock()

	if abw.closed {
		return nil
	}

	abw.closed = true

	// Close all channels
	for _, ch := range abw.writers {
		close(ch)
	}

	abw.writers = nil
	return nil
}

// Wait waits for all writers to finish
func (abw *asyncBroadcastWriter) Wait() error {
	abw.wg.Wait()
	return nil
}