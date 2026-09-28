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

// Target is where one package file lives upstream and what the index declared
// about it.
type Target struct {
	URL    string
	SHA256 string // hex digest, empty when the index supplied none
	Size   int64  // declared by the index, -1 when unknown
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
}

// New builds the fetcher over the index service. GROXPI_DOWNLOAD_TIMEOUT is the
// time-to-first-byte budget: it bounds only the wait for upstream response
// headers. Once headers are in, the body runs to completion under the caller's
// context, because a transfer already streaming must not be cut off by a budget
// meant for connection setup. At 0 (redirect mode) a fetch still gets 5 minutes.
func New(cfg *config.Config, idx *index.Service) *Service {
	ttfb := cfg.DownloadTimeout
	if ttfb <= 0 {
		ttfb = 5 * time.Minute
	}
	return &Service{index: idx, client: &http.Client{}, ttfb: ttfb}
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

// Fetch resolves a package file and opens its upstream body. It is the one
// method storage calls on a miss.
func (s *Service) Fetch(ctx context.Context, packageName, fileName string) (*File, error) {
	t, err := s.Resolve(ctx, packageName, fileName)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "🚀 Starting upstream download",
		"package", packageName, "file", fileName,
		"file_url", config.RedactURL(t.URL), "file_size", t.Size, "timeout", s.ttfb)

	// A deadline on the request context would outlive the headers and kill the
	// body read, so the budget is a timer that cancels and is then stopped: it
	// can only fire while we are still waiting for the response headers.
	reqCtx, cancel := context.WithCancel(ctx)
	budget := time.AfterFunc(s.ttfb, cancel)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, t.URL, nil) // #nosec G704 -- url is the file URL advertised by the configured index, not client input
	if err != nil {
		budget.Stop()
		cancel()
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "groxpi/1.0.0")

	resp, err := s.client.Do(req) // #nosec G704 -- see NewRequestWithContext above
	budget.Stop()
	if err != nil {
		cancel()
		// Both the URL we format and the one net/http embeds in its *url.Error
		// carry whatever credentials the index handed us.
		return nil, fmt.Errorf("failed to download from %s: %w",
			config.RedactURL(t.URL), config.RedactURLError(err))
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, config.RedactURL(t.URL))
	}
	return &File{Target: t, Body: cancelOnClose{resp.Body, cancel}, Length: resp.ContentLength}, nil
}

// cancelOnClose releases the request context along with the body.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
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
