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
