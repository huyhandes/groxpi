package streaming

import (
	"context"
	"io"
)

// StreamResult represents the result of a streaming operation
type StreamResult struct {
	Size        int64
	ContentType string
	ETag        string
	Error       error
}

// StreamingDownloader handles simultaneous download and cache operations
type StreamingDownloader interface {
	// DownloadAndStream downloads from URL while simultaneously streaming to writer and caching
	DownloadAndStream(ctx context.Context, url, storageKey string, writer io.Writer) (*StreamResult, error)
}

// BroadcastWriter allows multiple writers to receive the same data stream
type BroadcastWriter interface {
	io.Writer

	// AddWriter adds a writer to receive broadcasted data
	AddWriter(w io.Writer) error

	// RemoveWriter removes a writer from broadcast
	RemoveWriter(w io.Writer) error

	// Close closes all writers and stops broadcasting
	Close() error

	// Wait waits for all writers to finish
	Wait() error
}

// ZeroCopyServer provides zero-copy file serving capabilities
type ZeroCopyServer interface {
	// ServeFile serves a file using zero-copy techniques when possible
	ServeFile(ctx context.Context, writer io.Writer, filepath string) error

	// ServeReader serves data from reader using optimized copy techniques
	ServeReader(ctx context.Context, writer io.Writer, reader io.Reader, size int64) error
}
