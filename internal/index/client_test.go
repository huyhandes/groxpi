package index

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huyhandes/groxpi/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_MakeRequest(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request headers
		if r.Header.Get("Accept") != "application/vnd.pypi.simple.v1+json" {
			t.Errorf("Expected Accept header to be 'application/vnd.pypi.simple.v1+json', got '%s'", r.Header.Get("Accept"))
		}

		if r.Header.Get("User-Agent") != config.UserAgent {
			t.Errorf("Expected User-Agent %q, got %q", config.UserAgent, r.Header.Get("User-Agent"))
		}

		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"test": "response"}`))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := newClient(cfg, newTransport(cfg))

	resp, err := client.makeRequest(context.Background(), server.URL, "application/vnd.pypi.simple.v1+json")
	if err != nil {
		t.Fatalf("makeRequest failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/vnd.pypi.simple.v1+json" {
		t.Errorf("Expected content type 'application/vnd.pypi.simple.v1+json', got '%s'", contentType)
	}
}

func TestClient_ParseJSONPackageFiles(t *testing.T) {
	cfg := &config.Config{}
	client := newClient(cfg, newTransport(cfg))

	jsonResponse := `{
		"meta": {
			"api-version": "1.0"
		},
		"name": "numpy",
		"files": [
			{
				"filename": "numpy-1.21.0-py3-none-any.whl",
				"url": "https://files.pythonhosted.org/packages/.../numpy-1.21.0-py3-none-any.whl",
				"requires-python": ">=3.7",
				"yanked": false
			},
			{
				"filename": "numpy-1.21.0.tar.gz",
				"url": "https://files.pythonhosted.org/packages/.../numpy-1.21.0.tar.gz",
				"requires-python": ">=3.7",
				"yanked": false
			}
		]
	}`

	reader := strings.NewReader(jsonResponse)
	files, err := client.parseJSONPackageFiles(reader, nil)
	if err != nil {
		t.Fatalf("parseJSONPackageFiles failed: %v", err)
	}

	if len(files) != 2 {
		t.Errorf("Expected 2 files, got %d", len(files))
	}

	// Check first file
	if files[0].Name != "numpy-1.21.0-py3-none-any.whl" {
		t.Errorf("Expected first file name to be 'numpy-1.21.0-py3-none-any.whl', got '%s'", files[0].Name)
	}

	if files[0].RequiresPython != ">=3.7" {
		t.Errorf("Expected first file RequiresPython to be '>=3.7', got '%s'", files[0].RequiresPython)
	}

	if files[0].Yanked != false {
		t.Errorf("Expected first file Yanked to be false, got %v", files[0].Yanked)
	}

	// Check second file
	if files[1].Name != "numpy-1.21.0.tar.gz" {
		t.Errorf("Expected second file name to be 'numpy-1.21.0.tar.gz', got '%s'", files[1].Name)
	}
}

func TestClient_GetPackageFiles(t *testing.T) {
	packageName := "requests"

	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expectedPath := "/" + packageName + "/"
		if r.URL.Path != expectedPath {
			t.Errorf("Expected path '%s', got '%s'", expectedPath, r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		w.WriteHeader(http.StatusOK)
		response := `{
			"meta": {"api-version": "1.0"},
			"name": "requests",
			"files": [
				{
					"filename": "requests-2.28.0-py3-none-any.whl",
					"url": "https://files.pythonhosted.org/packages/.../requests-2.28.0-py3-none-any.whl",
					"requires-python": ">=3.7"
				}
			]
		}`
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := newClient(cfg, newTransport(cfg))

	files, err := client.GetPackageFiles(context.Background(), config.Index{URL: server.URL}, packageName)
	if err != nil {
		t.Fatalf("GetPackageFiles failed: %v", err)
	}

	if len(files) != 1 {
		t.Errorf("Expected 1 file, got %d", len(files))
	}

	if files[0].Name != "requests-2.28.0-py3-none-any.whl" {
		t.Errorf("Expected file name 'requests-2.28.0-py3-none-any.whl', got '%s'", files[0].Name)
	}
}

func TestClient_GetPackageFiles_NotFound(t *testing.T) {
	// Create test server that returns 404
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not Found"))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := newClient(cfg, newTransport(cfg))

	_, err := client.GetPackageFiles(context.Background(), config.Index{URL: server.URL}, "non-existent-package")
	if err == nil {
		t.Error("Expected error for non-existent package")
	}

	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Expected ErrNotFound, got: %v", err)
	}
}

// TestFileInfo_IsYanked tests the IsYanked method with different yanked values
func TestFileInfo_IsYanked(t *testing.T) {
	testCases := []struct {
		name     string
		yanked   any
		expected bool
	}{
		{"nil yanked", nil, false},
		{"false bool yanked", false, false},
		{"true bool yanked", true, true},
		{"empty string yanked", "", false},
		{"non-empty string yanked", "security issue", true},
		{"invalid type yanked", 123, false},
		{"float type yanked", 1.23, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fileInfo := &FileInfo{Yanked: tc.yanked}
			result := fileInfo.IsYanked()
			if result != tc.expected {
				t.Errorf("IsYanked() = %v, expected %v for yanked value %v", result, tc.expected, tc.yanked)
			}
		})
	}
}

// TestFileInfo_GetYankedReason tests the GetYankedReason method
func TestFileInfo_GetYankedReason(t *testing.T) {
	testCases := []struct {
		name     string
		yanked   any
		expected string
	}{
		{"reason from string yanked", "implicit reason", "implicit reason"},
		{"not yanked", false, ""},
		{"nil yanked", nil, ""},
		{"bool true yanked no reason", true, ""},
		{"empty string yanked", "", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fileInfo := &FileInfo{Yanked: tc.yanked}
			result := fileInfo.GetYankedReason()
			if result != tc.expected {
				t.Errorf("GetYankedReason() = %q, expected %q", result, tc.expected)
			}
		})
	}
}

// TestParseSimpleHTML_ResolvesRelativeHrefs verifies that hrefs on a
// PEP 503 HTML index page are resolved against the URL of the index page they came
// from (relative, same-directory, absolute and protocol-relative forms), and that
// the #sha256=... fragment survives resolution.
func TestParseSimpleHTML_ResolvesRelativeHrefs(t *testing.T) {
	const baseURL = "https://org/simple/foo/"

	testCases := []struct {
		name        string
		html        string
		expectedURL string
	}{
		{
			name:        "parent-relative href",
			html:        `<a href="../../packages/ab/cd/foo-1.0.tar.gz">foo-1.0.tar.gz</a>`,
			expectedURL: "https://org/packages/ab/cd/foo-1.0.tar.gz",
		},
		{
			name:        "same-directory relative href",
			html:        `<a href="foo-1.0.whl">foo-1.0.whl</a>`,
			expectedURL: "https://org/simple/foo/foo-1.0.whl",
		},
		{
			name:        "absolute href passes through unchanged",
			html:        `<a href="https://files.pythonhosted.org/packages/ab/cd/foo-1.0.tar.gz">foo-1.0.tar.gz</a>`,
			expectedURL: "https://files.pythonhosted.org/packages/ab/cd/foo-1.0.tar.gz",
		},
		{
			name:        "protocol-relative href inherits base scheme",
			html:        `<a href="//files.pythonhosted.org/packages/ab/cd/foo-1.0.tar.gz">foo-1.0.tar.gz</a>`,
			expectedURL: "https://files.pythonhosted.org/packages/ab/cd/foo-1.0.tar.gz",
		},
		{
			name:        "relative href keeps sha256 fragment",
			html:        `<a href="../../packages/ab/cd/foo-1.0.tar.gz#sha256=abc123">foo-1.0.tar.gz</a>`,
			expectedURL: "https://org/packages/ab/cd/foo-1.0.tar.gz#sha256=abc123",
		},
		{
			name:        "root-relative href",
			html:        `<a href="/packages/ab/cd/foo-1.0.tar.gz#sha256=abc123">foo-1.0.tar.gz</a>`,
			expectedURL: "https://org/packages/ab/cd/foo-1.0.tar.gz#sha256=abc123",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := parseSimpleHTML(strings.NewReader(tc.html), mustURL(t, baseURL))
			require.NoError(t, err)
			require.Len(t, files, 1)
			assert.Equal(t, tc.expectedURL, files[0].URL)
		})
	}
}

// TestParseSimpleHTML_SHA256Fragment verifies the #sha256= fragment is
// read into the hash map in the same shape the JSON index produces, so downstream
// code never branches on index format.
func TestParseSimpleHTML_SHA256Fragment(t *testing.T) {
	const sum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	testCases := []struct {
		name     string
		html     string
		expected map[string]string
	}{
		{
			name:     "sha256 fragment becomes a hash entry",
			html:     `<a href="../../packages/ab/cd/foo-1.0.tar.gz#sha256=` + sum + `">foo-1.0.tar.gz</a>`,
			expected: map[string]string{"sha256": sum},
		},
		{
			name:     "fragment without a hash yields no hashes",
			html:     `<a href="../../packages/ab/cd/foo-1.0.tar.gz#egg=foo">foo-1.0.tar.gz</a>`,
			expected: nil,
		},
		{
			name:     "no fragment yields no hashes",
			html:     `<a href="../../packages/ab/cd/foo-1.0.tar.gz">foo-1.0.tar.gz</a>`,
			expected: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := parseSimpleHTML(strings.NewReader(tc.html), mustURL(t, "https://org/simple/foo/"))
			require.NoError(t, err)
			require.Len(t, files, 1)
			assert.Equal(t, tc.expected, files[0].Hashes)
		})
	}
}

// TestParseSimpleHTML_PreservesAttributes ensures resolution did not
// disturb the other data pulled off the anchor tag, and that HTML entities are
// decoded. A PEP 503 index must escape ">" in data-requires-python; carrying the
// escape through into the PEP 691 body we serve makes pip raise InvalidSpecifier
// on "&gt;=3.8".
func TestParseSimpleHTML_PreservesAttributes(t *testing.T) {
	html := `<a href="../../packages/ab/cd/foo-1.0.tar.gz?token=a&amp;v=2#sha256=abc123" data-requires-python="&gt;=3.8,&lt;4" data-yanked="broken &amp; unusable">foo-1.0&amp;bar.tar.gz</a>`

	files, err := parseSimpleHTML(strings.NewReader(html), mustURL(t, "https://org/simple/foo/"))
	require.NoError(t, err)
	require.Len(t, files, 1)

	assert.Equal(t, "foo-1.0&bar.tar.gz", files[0].Name)
	assert.Equal(t, "https://org/packages/ab/cd/foo-1.0.tar.gz?token=a&v=2#sha256=abc123", files[0].URL)
	assert.Equal(t, ">=3.8,<4", files[0].RequiresPython)
	assert.Equal(t, "broken & unusable", files[0].Yanked)
}

// TestParseSimpleHTML_MalformedHref verifies an unparseable href is
// skipped rather than aborting the whole index.
func TestParseSimpleHTML_MalformedHref(t *testing.T) {
	html := `<a href="http://[::1]bad">bad.tar.gz</a>
<a href="foo-1.0.whl">foo-1.0.whl</a>`

	files, err := parseSimpleHTML(strings.NewReader(html), mustURL(t, "https://org/simple/foo/"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "https://org/simple/foo/foo-1.0.whl", files[0].URL)
}

// TestParseSimpleHTML_UnparseableBase: a nil base (the caller could not parse
// the index URL) leaves hrefs as-is rather than dropping every file.
func TestParseSimpleHTML_UnparseableBase(t *testing.T) {
	html := `<a href="foo-1.0.whl">foo-1.0.whl</a>`

	files, err := parseSimpleHTML(strings.NewReader(html), nil)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "foo-1.0.whl", files[0].URL)
}

// TestClient_GetPackageFiles_ResolvesJSONURLs verifies PEP 691 file URLs, which
// may be relative, are resolved against the URL the JSON page was served from,
// exactly as hrefs are on the HTML path, and keep their #sha256= fragment.
func TestClient_GetPackageFiles_ResolvesJSONURLs(t *testing.T) {
	testCases := []struct {
		name string
		url  string
		want string // with "{base}" standing for the upstream server URL
	}{
		{"absolute URL unchanged", "https://files.pythonhosted.org/packages/ab/cd/foo-1.0.tar.gz", "https://files.pythonhosted.org/packages/ab/cd/foo-1.0.tar.gz"},
		{"parent-relative URL", "../../packages/x.whl", "{base}/packages/x.whl"},
		{"same-directory URL", "x.whl", "{base}/simple/foo/x.whl"},
		{"root-relative URL keeps fragment", "/packages/x.whl#sha256=abc123", "{base}/packages/x.whl#sha256=abc123"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				_, _ = w.Write([]byte(`{"meta":{"api-version":"1.0"},"name":"foo","files":[` +
					`{"filename":"x.whl","url":"` + tc.url + `","hashes":{"sha256":"abc123"}}]}`))
			}))
			defer upstream.Close()

			cfg := &config.Config{}
			client := newClient(cfg, newTransport(cfg))
			files, err := client.GetPackageFiles(context.Background(), config.Index{URL: upstream.URL + "/simple"}, "foo")
			require.NoError(t, err)
			require.Len(t, files, 1)
			assert.Equal(t, strings.ReplaceAll(tc.want, "{base}", upstream.URL), files[0].URL)
			assert.Equal(t, map[string]string{"sha256": "abc123"}, files[0].Hashes)
		})
	}
}

// TestParseSimpleHTML tests HTML parsing for package files
func TestParseSimpleHTML(t *testing.T) {

	testCases := []struct {
		name     string
		html     string
		expected []FileInfo
	}{
		{
			name: "simple package files",
			html: `<!DOCTYPE html>
<html>
<body>
	<a href="numpy-1.21.0.tar.gz">numpy-1.21.0.tar.gz</a><br/>
	<a href="numpy-1.21.0-py3-none-any.whl">numpy-1.21.0-py3-none-any.whl</a><br/>
</body>
</html>`,
			expected: []FileInfo{
				{Name: "numpy-1.21.0.tar.gz", URL: "https://org/simple/numpy/numpy-1.21.0.tar.gz"},
				{Name: "numpy-1.21.0-py3-none-any.whl", URL: "https://org/simple/numpy/numpy-1.21.0-py3-none-any.whl"},
			},
		},
		{
			name: "files with hashes in URL",
			html: `<html>
<body>
<a href="package-1.0.tar.gz#sha256=abc123">package-1.0.tar.gz</a>
<a href="package-1.0.whl#md5=def456">package-1.0.whl</a>
</body>
</html>`,
			expected: []FileInfo{
				{
					Name: "package-1.0.tar.gz",
					URL:  "https://org/simple/numpy/package-1.0.tar.gz#sha256=abc123",
				},
				{
					Name: "package-1.0.whl",
					URL:  "https://org/simple/numpy/package-1.0.whl#md5=def456",
				},
			},
		},
		{
			name:     "empty package page",
			html:     `<html><body><h1>No files found</h1></body></html>`,
			expected: []FileInfo{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseSimpleHTML(strings.NewReader(tc.html), mustURL(t, "https://org/simple/numpy/"))
			if err != nil {
				t.Fatalf("parseSimpleHTML failed: %v", err)
			}

			if len(result) != len(tc.expected) {
				t.Errorf("Expected %d files, got %d", len(tc.expected), len(result))
				return
			}

			for i, file := range result {
				expected := tc.expected[i]
				if file.Name != expected.Name {
					t.Errorf("File %d name: expected %q, got %q", i, expected.Name, file.Name)
				}
				if file.URL != expected.URL {
					t.Errorf("File %d URL: expected %q, got %q", i, expected.URL, file.URL)
				}
			}
		})
	}
}

// TestClient_GetPackageFiles_StrictHTMLIndex pins the Accept header: an index
// that serves only HTML and 406s any request not offering an HTML type must
// still resolve, through the HTML parser.
func TestClient_GetPackageFiles_StrictHTMLIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><a href="/files/pkg-1.0.tar.gz">pkg-1.0.tar.gz</a></body></html>`))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := newClient(cfg, newTransport(cfg))

	files, err := client.GetPackageFiles(context.Background(), config.Index{URL: server.URL}, "pkg")
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "pkg-1.0.tar.gz", files[0].Name)
	assert.Equal(t, server.URL+"/files/pkg-1.0.tar.gz", files[0].URL)
}
