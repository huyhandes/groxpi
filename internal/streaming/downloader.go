package streaming

import (
	"context"
	"crypto/md5"
	"fmt"
	"hash"
	"io"
	"net/http"
	"sync"
	"time"
)

// StorageWriter interface to avoid import cycle with storage package
type StorageWriter interface {
	Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error
}

// streamingDownloader implements StreamingDownloader interface
type streamingDownloader struct {
	storage     StorageWriter
	httpClient  *http.Client
	copyBufPool *sync.Pool
}

// NewStreamingDownloader creates a new StreamingDownloader
func NewStreamingDownloader(storage StorageWriter, client *http.Client) StreamingDownloader {
	if client == nil {
		client = &http.Client{
			Timeout: 5 * time.Minute, // Use 5 minute timeout for large files
		}
	}

	return &streamingDownloader{
		storage:    storage,
		httpClient: client,
		copyBufPool: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 64*1024) // 64KB buffer
				return &buf
			},
		},
	}
}

// DownloadAndStream downloads from URL while simultaneously streaming to writer and caching
func (sd *streamingDownloader) DownloadAndStream(ctx context.Context, url, storageKey string, writer io.Writer) (*StreamResult, error) {
	// Logging disabled for tests

	start := time.Now()

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add appropriate headers
	req.Header.Set("User-Agent", "groxpi/1.0.0")
	req.Header.Set("Accept", "*/*")

	// Perform request
	resp, err := sd.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	// Get content metadata
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	contentLength := resp.ContentLength

	// Debug logging disabled for tests

	// Create a pipe for storing to storage
	storageReader, storageWriter := io.Pipe()

	// Create a hash writer for integrity checking
	hasher := md5.New()

	// Create multi-writer that writes to:
	// 1. Client (writer parameter)
	// 2. Storage (via pipe)
	// 3. Hash calculation
	multiWriter := io.MultiWriter(writer, storageWriter, hasher)

	// Create buffered reader for better performance
	var totalSize int64
	var streamErr error

	// Use goroutine to store data to storage backend
	storageErrCh := make(chan error, 1)
	go func() {
		defer storageReader.Close()
		err := sd.storage.Put(ctx, storageKey, storageReader, contentLength, contentType)
		storageErrCh <- err
	}()

	// Stream data using pooled buffer for optimal performance
	copyBufPtr := sd.copyBufPool.Get().(*[]byte)
	defer sd.copyBufPool.Put(copyBufPtr)
	copyBuf := *copyBufPtr

	totalSize, streamErr = io.CopyBuffer(multiWriter, resp.Body, copyBuf)

	// Close storage writer to signal completion
	storageWriter.Close()

	// Wait for storage operation to complete
	storageErr := <-storageErrCh

	// duration calculation for logging (disabled in tests)
	_ = time.Since(start)

	if streamErr != nil {
		// Error logging disabled for tests
		return nil, fmt.Errorf("streaming failed: %w", streamErr)
	}

	// Storage error is logged but not returned - client still got the data
	_ = storageErr

	// Calculate ETag from MD5 hash
	etag := fmt.Sprintf("\"%x\"", hasher.Sum(nil))

	result := &StreamResult{
		Size:        totalSize,
		ContentType: contentType,
		ETag:        etag,
		Error:       storageErr, // Include storage error for caller to decide
	}

	// Info logging disabled for tests

	return result, nil
}

// TeeStreamingDownloader provides streaming with broadcast capability
type teeStreamingDownloader struct {
	storage     StorageWriter
	httpClient  *http.Client
	copyBufPool *sync.Pool
}

// NewTeeStreamingDownloader creates a StreamingDownloader with TeeReader broadcasting
func NewTeeStreamingDownloader(storage StorageWriter, client *http.Client) StreamingDownloader {
	if client == nil {
		client = &http.Client{
			Timeout: 5 * time.Minute, // Use 5 minute timeout for large files
		}
	}

	return &teeStreamingDownloader{
		storage:    storage,
		httpClient: client,
		copyBufPool: &sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 64*1024) // 64KB buffer
				return &buf
			},
		},
	}
}

// DownloadAndStream downloads using TeeReader for better streaming performance
func (tsd *teeStreamingDownloader) DownloadAndStream(ctx context.Context, url, storageKey string, writer io.Writer) (*StreamResult, error) {
	// Debug logging disabled for tests

	start := time.Now()

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "groxpi/1.0.0")

	// Perform request
	resp, err := tsd.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Create hash calculator
	hasher := md5.New()

	// Create storage writer
	storageReader, storageWriter := io.Pipe()

	// Create TeeReader that sends data to both client and storage
	teeReader := io.TeeReader(resp.Body, io.MultiWriter(storageWriter, hasher))

	// Start storage goroutine
	storageErrCh := make(chan error, 1)
	go func() {
		defer storageReader.Close()
		err := tsd.storage.Put(ctx, storageKey, storageReader, resp.ContentLength, contentType)
		storageErrCh <- err
	}()

	// Copy to client using pooled buffer
	copyBufPtr := tsd.copyBufPool.Get().(*[]byte)
	defer tsd.copyBufPool.Put(copyBufPtr)
	copyBuf := *copyBufPtr

	totalSize, streamErr := io.CopyBuffer(writer, teeReader, copyBuf)

	// Close storage writer
	storageWriter.Close()

	// Wait for storage completion
	storageErr := <-storageErrCh

	// duration calculation for logging (disabled in tests)
	_ = time.Since(start)

	if streamErr != nil {
		// TeeReader error logging disabled for tests
		return nil, fmt.Errorf("tee streaming failed: %w", streamErr)
	}

	etag := fmt.Sprintf("\"%x\"", hasher.Sum(nil))

	result := &StreamResult{
		Size:        totalSize,
		ContentType: contentType,
		ETag:        etag,
		Error:       storageErr,
	}

	// TeeReader info logging disabled for tests

	return result, nil
}

// HashingWriter wraps an io.Writer with hash calculation
type HashingWriter struct {
	writer io.Writer
	hasher hash.Hash
}

// NewHashingWriter creates a writer that calculates hash while writing
func NewHashingWriter(w io.Writer, h hash.Hash) *HashingWriter {
	return &HashingWriter{
		writer: w,
		hasher: h,
	}
}

// Write writes data to underlying writer and updates hash
func (hw *HashingWriter) Write(p []byte) (n int, err error) {
	n, err = hw.writer.Write(p)
	if n > 0 {
		hw.hasher.Write(p[:n])
	}
	return n, err
}

// Sum returns the hash sum
func (hw *HashingWriter) Sum() []byte {
	return hw.hasher.Sum(nil)
}
