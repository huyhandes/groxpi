package pypi

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/phuslu/log"
)

// Client fetches index pages from an upstream PyPI-compatible index. Concurrent
// requests for the same package are coalesced by the package-file service, not
// here.
type Client struct {
	config     *config.Config
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
	Projects []struct {
		Name string `json:"name"`
	} `json:"projects,omitempty"`
	Name  string     `json:"name,omitempty"`
	Files []FileInfo `json:"files,omitempty"`
}

// Buffer pool for reducing allocations
var bufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// Copy buffer pool for zero-copy optimizations
var copyBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024) // 32KB copy buffers
		return &buf
	},
}

// Helper functions to reduce duplication (DRY principle)

// copyToBuffer copies from reader to buffer using pooled copy buffer for zero-copy optimization
func copyToBuffer(dst *bytes.Buffer, src io.Reader) error {
	copyBufPtr := copyBufferPool.Get().(*[]byte)
	defer copyBufferPool.Put(copyBufPtr)
	copyBuf := *copyBufPtr

	_, err := io.CopyBuffer(dst, src, copyBuf)
	return err
}

// withBuffers provides pooled buffers for parsing operations, following DRY principle
func withBuffers(fn func(*bytes.Buffer) error) error {
	buf := bufferPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufferPool.Put(buf)
	}()

	return fn(buf)
}

func NewClient(cfg *config.Config) *Client {
	// Optimized transport with better connection pooling
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.DisableSSLVerification,
		},
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,  // Enable HTTP/2 for better multiplexing
		DisableCompression:    false, // Let transport handle compression
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second, // Increased for large responses
	}

	if cfg.ConnectTimeout > 0 || cfg.ReadTimeout > 0 {
		timeout := cfg.ConnectTimeout + cfg.ReadTimeout
		if timeout > 0 {
			httpClient.Timeout = timeout
		}
	}

	return &Client{
		config:     cfg,
		httpClient: httpClient,
	}
}

func (c *Client) GetPackageList() ([]string, error) {
	url := strings.TrimSuffix(c.config.IndexURL, "/")

	// Try JSON first
	resp, err := c.makeRequest(url, "application/vnd.pypi.simple.v1+json")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch package list: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			// Log error but don't fail the operation
			_ = err
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	// Check if response is JSON
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "json") {
		return c.parseJSONPackageList(resp.Body)
	}

	// Fall back to HTML parsing
	return c.parseHTMLPackageList(resp.Body)
}

func (c *Client) GetPackageFiles(packageName string) ([]FileInfo, error) {
	indexURL := strings.TrimSuffix(c.config.IndexURL, "/") + "/" + packageName + "/"

	// Try JSON first
	resp, err := c.makeRequest(indexURL, "application/vnd.pypi.simple.v1+json")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch package files for %s: %w", packageName, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			// Log error but don't fail the operation
			_ = err
		}
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("package %s not found", packageName)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, indexURL)
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
	return c.parseHTMLPackageFiles(resp.Body, baseURL)
}

func (c *Client) makeRequest(url, accept string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "groxpi/1.0.0")

	return c.httpClient.Do(req)
}

func (c *Client) parseJSONPackageList(body io.Reader) ([]string, error) {
	var packages []string

	err := withBuffers(func(buf *bytes.Buffer) error {
		if err := copyToBuffer(buf, body); err != nil {
			return err
		}

		var response PyPISimpleResponse
		if err := json.Unmarshal(buf.Bytes(), &response); err != nil {
			return fmt.Errorf("failed to parse JSON response: %w", err)
		}

		packages = make([]string, len(response.Projects))
		for i, project := range response.Projects {
			packages[i] = project.Name
		}

		return nil
	})

	return packages, err
}

func (c *Client) parseJSONPackageFiles(body io.Reader) ([]FileInfo, error) {
	var files []FileInfo

	err := withBuffers(func(buf *bytes.Buffer) error {
		// Use buffered reader for better performance
		bufReader := bufio.NewReaderSize(body, 64*1024)

		if err := copyToBuffer(buf, bufReader); err != nil {
			return err
		}

		var response PyPISimpleResponse
		if err := json.Unmarshal(buf.Bytes(), &response); err != nil {
			return fmt.Errorf("failed to parse JSON response: %w", err)
		}

		files = response.Files
		return nil
	})

	return files, err
}

func (c *Client) parseHTMLPackageList(body io.Reader) ([]string, error) {
	var packages []string

	err := withBuffers(func(buf *bytes.Buffer) error {
		if err := copyToBuffer(buf, body); err != nil {
			return err
		}

		html := buf.String()
		packages = make([]string, 0, 1000)

		// Simple HTML parsing for package list
		lines := strings.SplitSeq(html, "\n")
		for line := range lines {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "<a ") {
				continue
			}

			// Extract package name from anchor text
			textStart := strings.Index(line, ">")
			textEnd := strings.Index(line, "</a>")
			if textStart == -1 || textEnd == -1 || textStart >= textEnd {
				continue
			}
			packageName := line[textStart+1 : textEnd]
			packages = append(packages, packageName)
		}

		return nil
	})

	return packages, err
}

// parseHTMLPackageFiles parses a PEP 503 HTML index page. baseURL is the URL the
// page was served from; relative and protocol-relative hrefs are resolved against
// it so that FileInfo.URL is always usable on its own.
func (c *Client) parseHTMLPackageFiles(body io.Reader, baseURL string) ([]FileInfo, error) {
	var files []FileInfo

	// A base we cannot parse means we cannot resolve anything; fall back to
	// emitting hrefs verbatim rather than dropping the whole index.
	base, baseErr := url.Parse(baseURL)
	if baseErr != nil {
		log.Warn().Err(baseErr).Str("base_url", baseURL).
			Msg("Cannot parse index URL, leaving package file hrefs unresolved")
		base = nil
	}

	err := withBuffers(func(buf *bytes.Buffer) error {
		if err := copyToBuffer(buf, body); err != nil {
			return err
		}

		html := buf.String()
		files = make([]FileInfo, 0, 50)

		// Simple HTML parsing for package files
		lines := strings.SplitSeq(html, "\n")
		for line := range lines {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "<a ") {
				continue
			}

			// Extract href
			hrefStart := strings.Index(line, `href="`)
			if hrefStart == -1 {
				continue
			}
			hrefStart += 6
			hrefEnd := strings.Index(line[hrefStart:], `"`)
			if hrefEnd == -1 {
				continue
			}
			href := line[hrefStart : hrefStart+hrefEnd]

			// Resolve the href against the index page URL. ResolveReference
			// correctly handles absolute, protocol-relative, root-relative and
			// path-relative hrefs, and carries the #sha256=... fragment through.
			fileURL := href
			if base != nil {
				ref, err := url.Parse(href)
				if err != nil {
					log.Warn().Err(err).Str("href", href).Str("base_url", baseURL).
						Msg("Skipping package file with unparseable href")
					continue
				}
				fileURL = base.ResolveReference(ref).String()
			}

			// Extract filename from anchor text
			textStart := strings.Index(line, ">")
			textEnd := strings.Index(line, "</a>")
			if textStart == -1 || textEnd == -1 || textStart >= textEnd {
				continue
			}
			filename := line[textStart+1 : textEnd]

			// Extract data-requires-python if present
			var requiresPython string
			if rpStart := strings.Index(line, `data-requires-python="`); rpStart != -1 {
				rpStart += 22
				if rpEnd := strings.Index(line[rpStart:], `"`); rpEnd != -1 {
					requiresPython = line[rpStart : rpStart+rpEnd]
				}
			}

			// Extract data-yanked if present
			var yanked any
			if yankStart := strings.Index(line, `data-yanked="`); yankStart != -1 {
				yankStart += 13
				if yankEnd := strings.Index(line[yankStart:], `"`); yankEnd != -1 {
					yankedStr := line[yankStart : yankStart+yankEnd]
					if yankedStr == "" {
						yanked = true
					} else {
						yanked = yankedStr
					}
				}
			}

			// PEP 503 carries the digest in the href fragment as
			// #sha256=<hex>; lift it into the same hash map shape the JSON
			// index produces so downstream code needs no format branch.
			var hashes map[string]string
			if _, fragment, found := strings.Cut(href, "#"); found {
				if sum, ok := strings.CutPrefix(fragment, "sha256="); ok && sum != "" {
					hashes = map[string]string{"sha256": sum}
				}
			}

			files = append(files, FileInfo{
				Name:           filename,
				URL:            fileURL,
				Hashes:         hashes,
				RequiresPython: requiresPython,
				Yanked:         yanked,
			})
		}

		return nil
	})

	return files, err
}
