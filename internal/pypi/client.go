package pypi

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/huybui/groxpi/internal/config"
)

type Client struct {
	config     *config.Config
	httpClient *http.Client
}

type FileInfo struct {
	Name           string                 `json:"filename"`
	URL            string                 `json:"url"`
	Hashes         map[string]string      `json:"hashes,omitempty"`
	RequiresPython string                 `json:"requires-python,omitempty"`
	Size           int64                  `json:"size,omitempty"`
	UploadTime     string                 `json:"upload-time,omitempty"`
	Yanked         interface{}            `json:"yanked,omitempty"` // Can be bool or string
	YankedReason   string                 `json:"yanked-reason,omitempty"`
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
	New: func() interface{} {
		return new(bytes.Buffer)
	},
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
	defer resp.Body.Close()
	
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
	url := strings.TrimSuffix(c.config.IndexURL, "/") + "/" + packageName + "/"
	
	// Try JSON first
	resp, err := c.makeRequest(url, "application/vnd.pypi.simple.v1+json")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch package files for %s: %w", packageName, err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("package %s not found", packageName)
	}
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	
	// Check if response is JSON
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "json") {
		return c.parseJSONPackageFiles(resp.Body)
	}
	
	// Fall back to HTML parsing
	return c.parseHTMLPackageFiles(resp.Body)
}

func (c *Client) DownloadFile(url string, dest string) error {
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download %s: %w", url, err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	
	// TODO: Implement actual file download to dest
	// For now, this is a placeholder
	return nil
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
	// Use buffer pool to reduce allocations
	buf := bufferPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufferPool.Put(buf)
	}()
	
	_, err := io.Copy(buf, body)
	if err != nil {
		return nil, err
	}
	
	var response PyPISimpleResponse
	if err := sonic.ConfigFastest.Unmarshal(buf.Bytes(), &response); err != nil {
		return nil, fmt.Errorf("failed to parse JSON response: %w", err)
	}
	
	packages := make([]string, len(response.Projects))
	for i, project := range response.Projects {
		packages[i] = project.Name
	}
	
	return packages, nil
}

func (c *Client) parseJSONPackageFiles(body io.Reader) ([]FileInfo, error) {
	// Use buffer pool to reduce allocations
	buf := bufferPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufferPool.Put(buf)
	}()
	
	// Use buffered reader for better performance
	bufReader := bufio.NewReaderSize(body, 64*1024)
	
	// Read response into buffer
	_, err := io.Copy(buf, bufReader)
	if err != nil {
		return nil, err
	}
	
	// Use sonic's ConfigFastest for maximum performance
	var response PyPISimpleResponse
	if err := sonic.ConfigFastest.Unmarshal(buf.Bytes(), &response); err != nil {
		return nil, fmt.Errorf("failed to parse JSON response: %w", err)
	}
	
	return response.Files, nil
}

func (c *Client) parseHTMLPackageList(body io.Reader) ([]string, error) {
	buf := bufferPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufferPool.Put(buf)
	}()
	
	_, err := io.Copy(buf, body)
	if err != nil {
		return nil, err
	}
	
	html := buf.String()
	packages := make([]string, 0, 1000)
	
	// Simple HTML parsing for package list
	lines := strings.Split(html, "\n")
	for _, line := range lines {
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
	
	return packages, nil
}

func (c *Client) parseHTMLPackageFiles(body io.Reader) ([]FileInfo, error) {
	buf := bufferPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufferPool.Put(buf)
	}()
	
	_, err := io.Copy(buf, body)
	if err != nil {
		return nil, err
	}
	
	html := buf.String()
	files := make([]FileInfo, 0, 50)
	
	// Simple HTML parsing for package files
	lines := strings.Split(html, "\n")
	for _, line := range lines {
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
		url := line[hrefStart : hrefStart+hrefEnd]
		
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
		var yanked interface{}
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
		
		files = append(files, FileInfo{
			Name:           filename,
			URL:            url,
			RequiresPython: requiresPython,
			Yanked:         yanked,
		})
	}
	
	return files, nil
}