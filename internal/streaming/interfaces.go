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

// Expectation is what the index declared about a file. It is what the download
// is verified against before anything is committed to storage. A zero value
// means the index declared nothing, so the file is cached unverified.
type Expectation struct {
	SHA256 string // hex digest, empty when the index supplied none
	Size   int64  // declared size in bytes, <= 0 when unknown
}

// StreamingDownloader handles simultaneous download and cache operations
type StreamingDownloader interface {
	// DownloadAndStream downloads from URL while simultaneously streaming to
	// writer and caching, committing only bytes that satisfy expect.
	DownloadAndStream(ctx context.Context, url, storageKey string, writer io.Writer, expect Expectation) (*StreamResult, error)
}
