// Package download is the stateless upstream fetcher: it resolves a package file
// through the index (including PEP 658 metadata siblings) and returns the open
// upstream body plus what the index declared about it. It caches nothing and
// coalesces nothing; storage does both and calls Fetch on a miss.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/huyhandes/groxpi/internal/index"
)

// ErrNotListed reports that the index does not list the requested file.
var ErrNotListed = errors.New("file not listed by the index")

// errUpstreamStalled ends a download whose body sent no bytes for the idle
// timeout. It names no URL: the file URL may carry index credentials.
var errUpstreamStalled = errors.New("upstream download stalled")

// Target is where one package file lives upstream and what the index declared
// about it.
type Target struct {
	URL    string
	SHA256 string // hex digest, empty when the index supplied none
	Size   int64  // declared by the index, -1 when unknown
}

// RedirectURL is the target as a Location for an upstream redirect: the URL
// with any userinfo removed, since a file URL resolved against a credentialed
// index carries the index password and a 302 would hand it to every client.
// stripped reports whether userinfo was removed. A URL that does not parse
// yields "": it may still hold the password, so it is never handed out.
func (t Target) RedirectURL() (u string, stripped bool) {
	parsed, err := url.Parse(t.URL)
	if err != nil {
		return "", false
	}
	if parsed.User == nil {
		return t.URL, false
	}
	parsed.User = nil
	return parsed.String(), true
}

// File is an open upstream body plus what it has to verify against. Closing
// Body releases the upstream request.
type File struct {
	Target
	Body   io.ReadCloser
	Length int64 // upstream Content-Length, -1 when unknown
}

// Service is the download module.
type Service struct {
	index  *index.Service
	client *http.Client
	ttfb   time.Duration
	idle   time.Duration
}

// New builds the fetcher over the index service. GROXPI_DOWNLOAD_TIMEOUT is the
// time-to-first-byte budget: it bounds only the wait for upstream response
// headers. GROXPI_DOWNLOAD_IDLE_TIMEOUT then bounds each gap between body
// bytes, so a slow but steady transfer completes and a stalled one fails. At a
// download timeout of 0 (redirect mode) the store never calls Fetch. Requests
// go through the index's upstream transport, so the TLS setting and connection
// pool are shared; the client has no whole-request timeout.
func New(cfg *config.Config, idx *index.Service) *Service {
	idle := cfg.DownloadIdleTimeout
	if idle <= 0 { // same rule as config.Load, for configs built without it
		idle = config.DefaultDownloadIdleTimeout
	}
	return &Service{
		index:  idx,
		client: &http.Client{Transport: idx.Transport()},
		ttfb:   cfg.DownloadTimeout,
		idle:   idle,
	}
}

// Resolve finds a package file in the index. A file the index does not list is
// ErrNotListed; any other error means the package index itself could not be
// resolved.
func (s *Service) Resolve(ctx context.Context, packageName, fileName string) (Target, error) {
	entry, err := s.index.Resolve(ctx, packageName)
	if err != nil {
		return Target{}, fmt.Errorf("failed to resolve index for package %q: %w", packageName, err)
	}
	notListed := fmt.Errorf("%w: %s/%s", ErrNotListed, packageName, fileName)

	if info, ok := findFile(entry.Files, fileName); ok {
		t := Target{URL: info.URL, SHA256: info.Hashes["sha256"], Size: -1}
		if info.Size > 0 {
			t.Size = info.Size
		}
		return t, nil
	}
	// PEP 658: the metadata file lives at the distribution's URL plus
	// ".metadata" and is only served when the index advertises it.
	if !strings.HasSuffix(fileName, metadataSuffix) {
		return Target{}, notListed
	}
	base, found := findFile(entry.Files, strings.TrimSuffix(fileName, metadataSuffix))
	if !found {
		return Target{}, notListed
	}
	hashes, advertised := base.Metadata()
	if !advertised {
		return Target{}, notListed
	}
	return Target{URL: metadataURL(base.URL), SHA256: hashes["sha256"], Size: -1}, nil
}

// Fetch opens the upstream body of a resolved target. Storage resolves once per
// miss and calls Fetch with that target.
//
// One timer guards the request. It is armed with the time-to-first-byte budget
// while waiting for headers, then re-armed with the idle timeout and reset by
// every body read that returns bytes; Close stops it. When it fires the request
// is cancelled with errUpstreamStalled, so a body that stops sending fails
// instead of holding its flight, and every request tailing it, open forever.
func (s *Service) Fetch(ctx context.Context, t Target) (*File, error) {
	slog.InfoContext(ctx, "🚀 Starting upstream download",
		"file_url", config.RedactURL(t.URL), "file_size", t.Size, "timeout", s.ttfb)

	reqCtx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(s.ttfb, func() { cancel(errUpstreamStalled) })
	release := func() {
		timer.Stop()
		cancel(nil)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, t.URL, nil) // #nosec G704 -- url is the file URL advertised by the configured index, not client input
	if err != nil {
		release()
		// An unparseable URL comes back as a *url.Error quoting it whole,
		// credentials included.
		return nil, fmt.Errorf("failed to create request: %w", config.RedactURLError(err))
	}
	req.Header.Set("User-Agent", "groxpi/1.0.0")

	resp, err := s.client.Do(req) // #nosec G704 -- see NewRequestWithContext above
	if err != nil {
		release()
		// Both the URL we format and the one net/http embeds in its *url.Error
		// carry whatever credentials the index handed us.
		return nil, fmt.Errorf("failed to download from %s: %w",
			config.RedactURL(t.URL), config.RedactURLError(err))
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		release()
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, config.RedactURL(t.URL))
	}
	timer.Reset(s.idle)
	body := &idleBody{body: resp.Body, ctx: reqCtx, timer: timer, idle: s.idle, release: release}
	return &File{Target: t, Body: body, Length: resp.ContentLength}, nil
}

// idleBody resets the stall timer on every read that returns bytes and
// releases the request on Close.
type idleBody struct {
	body    io.ReadCloser
	ctx     context.Context
	timer   *time.Timer
	idle    time.Duration
	release func()
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	if err != nil && err != io.EOF && errors.Is(context.Cause(b.ctx), errUpstreamStalled) {
		// net/http reports the cancellation as a bare "context canceled"; the
		// cause is what the caller needs, and it names no URL.
		err = errUpstreamStalled
	}
	return n, err
}

func (b *idleBody) Close() error {
	err := b.body.Close()
	b.release()
	return err
}

func findFile(files []index.FileInfo, fileName string) (index.FileInfo, bool) {
	i := slices.IndexFunc(files, func(f index.FileInfo) bool { return f.Name == fileName })
	if i < 0 {
		return index.FileInfo{}, false
	}
	return files[i], true
}

// metadataSuffix is the PEP 658 filename suffix for a distribution's METADATA.
const metadataSuffix = ".metadata"

// metadataURL is the PEP 658 location of a distribution's metadata file: the
// file URL with ".metadata" appended to its path, dropping any #sha256= fragment
// a PEP 503 index carried on the href.
func metadataURL(fileURL string) string {
	u, err := url.Parse(fileURL)
	if err != nil {
		return strings.TrimSuffix(fileURL, "#") + metadataSuffix
	}
	u.Fragment = ""
	u.Path += metadataSuffix
	if u.RawPath != "" {
		u.RawPath += metadataSuffix
	}
	return u.String()
}
