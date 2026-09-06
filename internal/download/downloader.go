package download

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
// declared. Such a download is never committed to storage. It says nothing about
// what the client received: see DownloadAndStream.
var ErrVerification = errors.New("integrity check failed")

// teeDownloader streams a download to the client while teeing it into storage.
type teeDownloader struct {
	storage     storage.Storage
	httpClient  *http.Client
	ttfb        time.Duration
	copyBufPool *sync.Pool
}

// newTeeDownloader builds the tee downloader.
//
// The client's Timeout is taken as the time-to-first-byte budget and then
// cleared: it bounds only the wait for upstream response headers. Once headers
// are in, the body runs to completion under the caller's context, because a
// transfer that has already started streaming to the client must not be cut off
// by a budget meant for connection setup.
func newTeeDownloader(st storage.Storage, client *http.Client) *teeDownloader {
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

	return &teeDownloader{
		storage:    st,
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

// DownloadAndStream downloads url, streaming the body to writer while caching it
// under storageKey.
//
// The bytes are hashed on the way through and checked against expect before the
// storage pipe is closed cleanly, so a file that fails verification is never
// committed: the pipe is closed with an error instead and the backend discards
// its partial write.
//
// The verification guarantee is cache-only. Bytes reach the client as they
// arrive, which is the point of streaming, so by the time the digest can be
// computed the client already has all of them and a 200 has already been sent.
// What a failed check buys is that the bad bytes are not kept and not served to
// anyone else.
//
// A cache-write failure is not the client's problem: it is reported in
// StreamResult.Error and the transfer completes.
func (tsd *teeDownloader) DownloadAndStream(ctx context.Context, url, storageKey string, writer io.Writer, expect Expectation) (*StreamResult, error) {
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
	// The cache side of the tee is fenced off from the client side: caching is
	// best-effort, so a backend that gives up mid-stream (a full disk, say) must
	// cost this download its cache entry and nothing else.
	cacheSink := &bestEffortWriter{w: storageWriter}
	teeReader := io.TeeReader(resp.Body, io.MultiWriter(cacheSink, hasher))

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
	if storageErr == nil {
		storageErr = cacheSink.err
	}

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

// bestEffortWriter forwards to the cache until the cache stops accepting, then
// swallows the rest while remembering why.
//
// It is what keeps a storage failure off the client's transfer. Without it the
// tee's write error propagated out of the io.Copy driving the response: the
// backend returning early closed the pipe, the next tee write got
// io.ErrClosedPipe, and the client was handed a truncated 200. A full disk broke
// every concurrent download instead of merely not caching them.
//
// Write is only ever called from the goroutine driving that copy, so err needs no
// synchronisation; it is read once the copy has returned.
type bestEffortWriter struct {
	w   io.Writer
	err error
}

func (b *bestEffortWriter) Write(p []byte) (int, error) {
	if b.err != nil {
		return len(p), nil
	}

	if _, err := b.w.Write(p); err != nil {
		b.err = err
		return len(p), nil
	}

	return len(p), nil
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
