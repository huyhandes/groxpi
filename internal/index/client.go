package index

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/huyhandes/groxpi/internal/config"
)

// ErrNotFound reports that an index does not list a package. Index resolution has
// to tell a miss from a failure: a miss falls through to the next index, a
// failure does not.
var ErrNotFound = errors.New("package not found")

// Client fetches index pages from an upstream PyPI-compatible index. Concurrent
// requests for the same package are coalesced by the package-file service, not
// here.
type Client struct {
	httpClient *http.Client
}

type FileInfo struct {
	Name           string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes,omitempty"`
	RequiresPython string            `json:"requires-python,omitempty"`
	Size           int64             `json:"size,omitempty"`
	UploadTime     string            `json:"upload-time,omitempty"`
	Yanked         any               `json:"yanked,omitempty"` // Can be bool or string
	YankedReason   string            `json:"yanked-reason,omitempty"`
	// PEP 658/714: true or a hash map when <url>.metadata exists upstream.
	// Both spellings are accepted; CoreMetadata wins when both are present.
	CoreMetadata     any `json:"core-metadata,omitempty"`
	DistInfoMetadata any `json:"dist-info-metadata,omitempty"`
}

// Metadata reports whether the index advertises a PEP 658 metadata file for
// this distribution and, when it does, the hashes it declared for it (nil for a
// bare "true").
func (f *FileInfo) Metadata() (hashes map[string]string, ok bool) {
	v := f.CoreMetadata
	if v == nil {
		v = f.DistInfoMetadata
	}
	switch m := v.(type) {
	case bool:
		return nil, m
	case map[string]any:
		hashes = make(map[string]string, len(m))
		for k, h := range m {
			if s, ok := h.(string); ok {
				hashes[k] = s
			}
		}
		return hashes, true
	case map[string]string:
		return m, true
	}
	return nil, false
}

// IsYanked returns true if the file is yanked
func (f *FileInfo) IsYanked() bool {
	if f.Yanked == nil {
		return false
	}
	switch v := f.Yanked.(type) {
	case bool:
		return v
	case string:
		return v != ""
	default:
		return false
	}
}

// GetYankedReason returns the yanked reason if available
func (f *FileInfo) GetYankedReason() string {
	if f.YankedReason != "" {
		return f.YankedReason
	}
	if s, ok := f.Yanked.(string); ok && s != "" {
		return s
	}
	return ""
}

type PyPISimpleResponse struct {
	Meta struct {
		APIVersion string `json:"api-version"`
	} `json:"meta"`
	Name  string     `json:"name,omitempty"`
	Files []FileInfo `json:"files,omitempty"`
}

// maxPackageIndexBytes caps one per-package index body, JSON or HTML. The body
// is held in memory to parse, so an upstream answering with an endless body
// would otherwise exhaust the process. A var only so a test can lower it.
// Documented in docs/api-endpoints.md beside the root cap.
var maxPackageIndexBytes int64 = 64 << 20

// readCapped reads r whole, failing rather than truncating past limit bytes.
// Reading one byte past the cap is what tells the two apart: a body of exactly
// the cap is returned, the first byte over is an error.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("body exceeds the %d byte limit", limit)
	}
	return body, nil
}

// newTransport builds the one upstream transport every client shares: the
// per-package index, the root proxy and file downloads. It is where
// GROXPI_DISABLE_INDEX_SSL_VERIFICATION applies, so the setting reaches all
// three.
func newTransport(cfg *config.Config) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.DisableSSLVerification, // #nosec G402 -- explicit operator opt-in (GROXPI_DISABLE_INDEX_SSL_VERIFICATION)
		},
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// newClient builds the per-package index client over the shared transport.
func newClient(cfg *config.Config, transport http.RoundTripper) *Client {
	httpClient := &http.Client{Transport: transport, Timeout: 60 * time.Second}

	// http.Client.Timeout is one budget for the whole request - connect, headers
	// and body - so the two configured phases are summed rather than applied
	// separately: an index that takes ConnectTimeout to connect is still allowed
	// its ReadTimeout to answer. Both default to 0, which leaves the 60s above.
	if budget := cfg.ConnectTimeout + cfg.ReadTimeout; budget > 0 {
		httpClient.Timeout = budget
	}
	return &Client{httpClient: httpClient}
}

// GetPackageFiles fetches one package's file list from one index. The index is a
// parameter rather than client state because resolution asks several of them for
// the same package; the client itself is stateless about which.
func (c *Client) GetPackageFiles(ctx context.Context, index config.Index, packageName string) ([]FileInfo, error) {
	indexURL := strings.TrimSuffix(index.URL, "/") + "/" + packageName + "/"

	// Try JSON first
	resp, err := c.makeRequest(ctx, indexURL, "application/vnd.pypi.simple.v1+json")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch package files for %s from %s: %w", packageName, index.Redacted(), err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			// Log error but don't fail the operation
			_ = err
		}
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s on %s", ErrNotFound, packageName, index.Redacted())
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s for package %s", resp.StatusCode, index.Redacted(), packageName)
	}

	// Check if response is JSON
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "json") {
		// PEP 691 requires absolute URLs in the JSON payload, so no resolution is needed.
		return c.parseJSONPackageFiles(resp.Body)
	}

	// Fall back to HTML parsing. Resolve hrefs against the URL actually served
	// (which may differ from indexURL after a redirect).
	baseURL := indexURL
	if resp.Request != nil && resp.Request.URL != nil {
		baseURL = resp.Request.URL.String()
	}
	// A base we cannot parse means we cannot resolve anything; fall back to
	// emitting hrefs verbatim rather than dropping the whole index.
	base, err := url.Parse(baseURL)
	if err != nil {
		slog.WarnContext(ctx, "Cannot parse index URL, leaving package file hrefs unresolved",
			"error", err, "base_url", config.RedactURL(baseURL))
		base = nil
	}
	files, err := parseSimpleHTML(resp.Body, base)
	if err != nil {
		return nil, fmt.Errorf("failed to parse HTML page for %s from %s: %w", packageName, index.Redacted(), err)
	}
	// Still an authoritative answer, but a page this parser cannot read would
	// look exactly like this, so say so.
	if len(files) == 0 {
		slog.WarnContext(ctx, "HTML index page lists no files",
			"package", packageName, "index", index.Redacted())
	}
	return files, nil
}

func (c *Client) makeRequest(ctx context.Context, target, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, config.RedactURLError(err)
	}

	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "groxpi/1.0.0")

	resp, err := c.httpClient.Do(req)
	return resp, config.RedactURLError(err)
}

func (c *Client) parseJSONPackageFiles(body io.Reader) ([]FileInfo, error) {
	buf, err := readCapped(body, maxPackageIndexBytes)
	if err != nil {
		return nil, err
	}
	var response PyPISimpleResponse
	if err := json.Unmarshal(buf, &response); err != nil {
		return nil, fmt.Errorf("failed to parse JSON response: %w", err)
	}
	return response.Files, nil
}
