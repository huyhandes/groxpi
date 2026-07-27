package streaming

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/storage"
	"github.com/huyhandes/groxpi/internal/telemetry"
)

// ErrVerification marks a download whose bytes did not match what the index
// declared. Such a download is never committed to storage.
var ErrVerification = errors.New("integrity check failed")

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
	ttfb        time.Duration
	copyBufPool *sync.Pool
}

// NewTeeStreamingDownloader creates a StreamingDownloader with TeeReader broadcasting.
//
// The client's Timeout is taken as the time-to-first-byte budget and then
// cleared: it bounds only the wait for upstream response headers. Once headers
// are in, the body runs to completion under the caller's context, because a
// transfer that has already started streaming to the client must not be cut off
// by a budget meant for connection setup.
func NewTeeStreamingDownloader(storage StorageWriter, client *http.Client) StreamingDownloader {
	ttfb := 5 * time.Minute
	if client == nil {
		client = &http.Client{}
	} else {
		if client.Timeout > 0 {
			ttfb = client.Timeout
		}
		clone := *client
		client = &clone
	}
	client.Timeout = 0

	return &teeStreamingDownloader{
		storage:    storage,
		httpClient: client,
		ttfb:       ttfb,
		copyBufPool: &sync.Pool{
			New: func() any {
				buf := make([]byte, 64*1024) // 64KB buffer
				return &buf
			},
		},
	}
}

// DownloadAndStream downloads using TeeReader for better streaming performance.
// The bytes are hashed on the way through and checked against expect before the
// storage pipe is closed cleanly, so a file that fails verification is never
// committed: the pipe is closed with an error instead and the backend discards
// its partial write.
func (tsd *teeStreamingDownloader) DownloadAndStream(ctx context.Context, url, storageKey string, writer io.Writer, expect Expectation) (*StreamResult, error) {
	// A deadline on the request context would outlive the headers and kill the
	// body read, so the budget is a timer that cancels and is then stopped: it
	// can only fire while we are still waiting for the response headers.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	budget := time.AfterFunc(tsd.ttfb, cancel)

	req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
	if err != nil {
		budget.Stop()
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "groxpi/1.0.0")

	resp, err := tsd.httpClient.Do(req)
	budget.Stop()
	if err != nil {
		// Both the URL we format and the one net/http embeds in its *url.Error
		// carry whatever credentials the index handed us, so neither may reach a
		// log or a response body as-is.
		return nil, fmt.Errorf("failed to download from %s: %w",
			config.RedactURL(url), config.RedactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, config.RedactURL(url))
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	hasher := sha256.New()
	storageReader, storageWriter := io.Pipe()
	teeReader := io.TeeReader(resp.Body, io.MultiWriter(storageWriter, hasher))

	storageErrCh := make(chan error, 1)
	go func() {
		defer func() { _ = storageReader.Close() }()

		putCtx, span := telemetry.Tracer().Start(ctx, "storage.put")
		defer span.End()

		_, err := tsd.storage.Put(putCtx, storageKey, storageReader, resp.ContentLength, contentType)
		if err != nil {
			span.RecordError(err)
		}
		storageErrCh <- err
	}()

	copyBufPtr := tsd.copyBufPool.Get().(*[]byte)
	defer tsd.copyBufPool.Put(copyBufPtr)

	totalSize, streamErr := io.CopyBuffer(writer, teeReader, *copyBufPtr)
	digest := hex.EncodeToString(hasher.Sum(nil))

	// Closing the pipe cleanly is what tells the backend to commit, so it may
	// only happen once the bytes are known to be complete and correct.
	failure := streamErr
	if failure == nil {
		failure = verify(storageKey, expect, digest, totalSize, resp.ContentLength)
	}
	if failure != nil {
		if errors.Is(failure, ErrVerification) {
			telemetry.VerificationFailure(ctx)
		}
		_ = storageWriter.CloseWithError(failure)
	} else {
		_ = storageWriter.Close()
	}

	storageErr := <-storageErrCh

	if streamErr != nil {
		return nil, fmt.Errorf("tee streaming failed: %w", streamErr)
	}
	if failure != nil {
		return nil, failure
	}

	return &StreamResult{
		Size:        totalSize,
		ContentType: contentType,
		ETag:        `"` + digest + `"`,
		Error:       storageErr,
	}, nil
}

// verify checks the received bytes against what the index declared: SHA-256 when
// it supplied one, otherwise the declared length. With neither, the file is
// accepted unverified — a deliberate hole so that sparse private indexes keep
// working — and the weakening is logged.
func verify(storageKey string, expect Expectation, digest string, received, upstreamLength int64) error {
	if expect.SHA256 != "" {
		if !strings.EqualFold(expect.SHA256, digest) {
			return fmt.Errorf("%w: %s: sha256 %s, expected %s", ErrVerification, storageKey, digest, expect.SHA256)
		}
		return nil
	}

	expectedSize := expect.Size
	if expectedSize <= 0 {
		expectedSize = upstreamLength
	}
	if expectedSize >= 0 {
		if received != expectedSize {
			return fmt.Errorf("%w: %s: %d bytes, expected %d", ErrVerification, storageKey, received, expectedSize)
		}
		return nil
	}

	slog.Warn("⚠️ Caching unverified file: index supplied neither a hash nor a length",
		"key", storageKey,
		"size", received)
	return nil
}
