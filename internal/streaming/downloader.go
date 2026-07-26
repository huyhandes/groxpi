package streaming

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/huyhandes/groxpi/internal/storage"
)

// StorageWriter is the write half of storage.Storage, narrowed to what the
// downloader needs. The signature matches storage.Storage.Put exactly so any
// backend satisfies it directly, with no adapter in between.
type StorageWriter interface {
	Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) (*storage.ObjectInfo, error)
}

// teeStreamingDownloader streams a download to the client while teeing it into storage.
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
			New: func() any {
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
	defer func() { _ = resp.Body.Close() }()

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
		defer func() {
			if err := storageReader.Close(); err != nil {
				// Log error but continue
				_ = err
			}
		}()
		_, err := tsd.storage.Put(ctx, storageKey, storageReader, resp.ContentLength, contentType)
		storageErrCh <- err
	}()

	// Copy to client using pooled buffer
	copyBufPtr := tsd.copyBufPool.Get().(*[]byte)
	defer tsd.copyBufPool.Put(copyBufPtr)
	copyBuf := *copyBufPtr

	totalSize, streamErr := io.CopyBuffer(writer, teeReader, copyBuf)

	// Close storage writer
	if err := storageWriter.Close(); err != nil {
		// Log error but continue
		_ = err
	}

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
