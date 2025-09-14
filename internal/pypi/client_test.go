package pypi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huyhandes/groxpi/internal/config"
)

func TestNewClient(t *testing.T) {
	cfg := &config.Config{
		IndexURL:               "https://pypi.org/simple/",
		DisableSSLVerification: false,
		ConnectTimeout:         5 * time.Second,
		ReadTimeout:            20 * time.Second,
	}

	client := NewClient(cfg)

	if client == nil {
		t.Fatal("NewClient() returned nil")
	}

	// Test that client can make basic requests (interface testing)
	// Note: We can't access private fields, so we test the public interface
}

func TestNewClient_SSLVerificationDisabled(t *testing.T) {
	cfg := &config.Config{
		IndexURL:               "https://pypi.org/simple/",
		DisableSSLVerification: true,
	}

	client := NewClient(cfg)

	// Check if SSL verification is disabled
	transport := client.httpClient.Transport.(*http.Transport)
	if !transport.TLSClientConfig.InsecureSkipVerify {
		t.Error("Expected SSL verification to be disabled")
	}
}

func TestClient_MakeRequest(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request headers
		if r.Header.Get("Accept") != "application/vnd.pypi.simple.v1+json" {
			t.Errorf("Expected Accept header to be 'application/vnd.pypi.simple.v1+json', got '%s'", r.Header.Get("Accept"))
		}

		if r.Header.Get("User-Agent") != "groxpi/1.0.0" {
			t.Errorf("Expected User-Agent to be 'groxpi/1.0.0', got '%s'", r.Header.Get("User-Agent"))
		}

		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"test": "response"}`))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := NewClient(cfg)

	resp, err := client.makeRequest(server.URL, "application/vnd.pypi.simple.v1+json")
	if err != nil {
		t.Fatalf("makeRequest failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/vnd.pypi.simple.v1+json" {
		t.Errorf("Expected content type 'application/vnd.pypi.simple.v1+json', got '%s'", contentType)
	}
}

func TestClient_ParseJSONPackageList(t *testing.T) {
	cfg := &config.Config{}
	client := NewClient(cfg)

	jsonResponse := `{
		"meta": {
			"api-version": "1.0"
		},
		"projects": [
			{"name": "numpy"},
			{"name": "scipy"},
			{"name": "pandas"}
		]
	}`

	reader := strings.NewReader(jsonResponse)
	packages, err := client.parseJSONPackageList(reader)
	if err != nil {
		t.Fatalf("parseJSONPackageList failed: %v", err)
	}

	expected := []string{"numpy", "scipy", "pandas"}
	if len(packages) != len(expected) {
		t.Errorf("Expected %d packages, got %d", len(expected), len(packages))
	}

	for i, pkg := range expected {
		if i >= len(packages) || packages[i] != pkg {
			t.Errorf("Expected package[%d] to be '%s', got '%s'", i, pkg, packages[i])
		}
	}
}

func TestClient_ParseJSONPackageFiles(t *testing.T) {
	cfg := &config.Config{}
	client := NewClient(cfg)

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
	files, err := client.parseJSONPackageFiles(reader)
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

func TestClient_GetPackageList(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		w.WriteHeader(http.StatusOK)
		response := `{
			"meta": {"api-version": "1.0"},
			"projects": [
				{"name": "requests"},
				{"name": "urllib3"}
			]
		}`
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := NewClient(cfg)

	packages, err := client.GetPackageList()
	if err != nil {
		t.Fatalf("GetPackageList failed: %v", err)
	}

	expected := []string{"requests", "urllib3"}
	if len(packages) != len(expected) {
		t.Errorf("Expected %d packages, got %d", len(expected), len(packages))
	}

	for i, pkg := range expected {
		if i >= len(packages) || packages[i] != pkg {
			t.Errorf("Expected package[%d] to be '%s', got '%s'", i, pkg, packages[i])
		}
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
	client := NewClient(cfg)

	files, err := client.GetPackageFiles(packageName)
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
		w.Write([]byte("Not Found"))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := NewClient(cfg)

	_, err := client.GetPackageFiles("non-existent-package")
	if err == nil {
		t.Error("Expected error for non-existent package")
	}

	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("Expected 'not found' in error message, got: %v", err)
	}
}

func TestClient_GetPackageList_HTTPError(t *testing.T) {
	// Create test server that returns 500
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal Server Error"))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := NewClient(cfg)

	_, err := client.GetPackageList()
	if err == nil {
		t.Error("Expected error for HTTP 500")
	}

	if !strings.Contains(err.Error(), "500") {
		t.Errorf("Expected '500' in error message, got: %v", err)
	}
}

func TestClient_ParseJSONInvalidData(t *testing.T) {
	cfg := &config.Config{}
	client := NewClient(cfg)

	t.Run("invalid JSON", func(t *testing.T) {
		reader := strings.NewReader(`{"invalid": json}`)
		_, err := client.parseJSONPackageList(reader)
		if err == nil {
			t.Error("Expected error for invalid JSON")
		}
	})

	t.Run("empty response", func(t *testing.T) {
		reader := strings.NewReader("")
		_, err := client.parseJSONPackageList(reader)
		if err == nil {
			t.Error("Expected error for empty response")
		}
	})

	t.Run("malformed structure", func(t *testing.T) {
		reader := strings.NewReader(`{"projects": "not an array"}`)
		_, err := client.parseJSONPackageList(reader)
		if err == nil {
			t.Error("Expected error for malformed structure")
		}
	})
}

func TestClient_DownloadFile(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake file content"))
	}))
	defer server.Close()

	cfg := &config.Config{}
	client := NewClient(cfg)

	err := client.DownloadFile(server.URL, "/tmp/test-file")
	if err != nil {
		t.Errorf("DownloadFile failed: %v", err)
	}
}

func TestClient_DownloadFile_HTTPError(t *testing.T) {
	// Create test server that returns 404
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := &config.Config{}
	client := NewClient(cfg)

	err := client.DownloadFile(server.URL, "/tmp/test-file")
	if err == nil {
		t.Error("Expected error for HTTP 404")
	}

	if !strings.Contains(err.Error(), "404") {
		t.Errorf("Expected '404' in error message, got: %v", err)
	}
}

// Singleflight tests following TDD principles
func TestClient_SingleflightPackageList(t *testing.T) {
	requestCount := int64(0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		// Add delay to ensure concurrent requests hit
		time.Sleep(10 * time.Millisecond)

		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		w.WriteHeader(http.StatusOK)
		response := `{
			"meta": {"api-version": "1.0"},
			"projects": [{"name": "test-package"}]
		}`
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := NewClient(cfg)

	const numGoroutines = 10
	var wg sync.WaitGroup
	results := make([][]string, numGoroutines)
	errors := make([]error, numGoroutines)

	// Launch concurrent requests
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			packages, err := client.GetPackageList()
			results[idx] = packages
			errors[idx] = err
		}(i)
	}

	wg.Wait()

	// Verify only one HTTP request was made
	finalRequestCount := atomic.LoadInt64(&requestCount)
	if finalRequestCount != 1 {
		t.Errorf("Expected exactly 1 HTTP request due to singleflight, got %d", finalRequestCount)
	}

	// Verify all results are identical and successful
	for i := 0; i < numGoroutines; i++ {
		if errors[i] != nil {
			t.Errorf("Request %d failed: %v", i, errors[i])
		}

		if len(results[i]) != 1 || results[i][0] != "test-package" {
			t.Errorf("Request %d got unexpected result: %v", i, results[i])
		}
	}
}

func TestClient_SingleflightPackageFiles(t *testing.T) {
	requestCount := int64(0)
	packageName := "test-package"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		// Add delay to ensure concurrent requests hit
		time.Sleep(10 * time.Millisecond)

		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		w.WriteHeader(http.StatusOK)
		response := `{
			"meta": {"api-version": "1.0"},
			"name": "test-package",
			"files": [
				{
					"filename": "test-package-1.0.0.tar.gz",
					"url": "https://files.pythonhosted.org/packages/.../test-package-1.0.0.tar.gz"
				}
			]
		}`
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := NewClient(cfg)

	const numGoroutines = 10
	var wg sync.WaitGroup
	results := make([][]FileInfo, numGoroutines)
	errors := make([]error, numGoroutines)

	// Launch concurrent requests for same package
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			files, err := client.GetPackageFiles(packageName)
			results[idx] = files
			errors[idx] = err
		}(i)
	}

	wg.Wait()

	// Verify only one HTTP request was made
	finalRequestCount := atomic.LoadInt64(&requestCount)
	if finalRequestCount != 1 {
		t.Errorf("Expected exactly 1 HTTP request due to singleflight, got %d", finalRequestCount)
	}

	// Verify all results are identical and successful
	for i := 0; i < numGoroutines; i++ {
		if errors[i] != nil {
			t.Errorf("Request %d failed: %v", i, errors[i])
		}

		if len(results[i]) != 1 || results[i][0].Name != "test-package-1.0.0.tar.gz" {
			t.Errorf("Request %d got unexpected result: %v", i, results[i])
		}
	}
}

func TestClient_SingleflightErrorPropagation(t *testing.T) {
	requestCount := int64(0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		// Add delay to ensure concurrent requests hit
		time.Sleep(10 * time.Millisecond)

		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Server Error"))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := NewClient(cfg)

	const numGoroutines = 5
	var wg sync.WaitGroup
	errors := make([]error, numGoroutines)

	// Launch concurrent requests that should all fail
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := client.GetPackageList()
			errors[idx] = err
		}(i)
	}

	wg.Wait()

	// Verify only one HTTP request was made
	finalRequestCount := atomic.LoadInt64(&requestCount)
	if finalRequestCount != 1 {
		t.Errorf("Expected exactly 1 HTTP request due to singleflight, got %d", finalRequestCount)
	}

	// Verify all requests got the same error
	for i := 0; i < numGoroutines; i++ {
		if errors[i] == nil {
			t.Errorf("Request %d should have failed", i)
			continue
		}

		if !strings.Contains(errors[i].Error(), "500") {
			t.Errorf("Request %d got unexpected error: %v", i, errors[i])
		}
	}
}

func TestClient_SingleflightDifferentPackages(t *testing.T) {
	requestCount := int64(0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		time.Sleep(10 * time.Millisecond)

		packageName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/")

		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		w.WriteHeader(http.StatusOK)
		response := `{
			"meta": {"api-version": "1.0"},
			"name": "` + packageName + `",
			"files": [
				{
					"filename": "` + packageName + `-1.0.0.tar.gz",
					"url": "https://files.pythonhosted.org/packages/.../` + packageName + `-1.0.0.tar.gz"
				}
			]
		}`
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := NewClient(cfg)

	packages := []string{"package-a", "package-b", "package-c"}
	var wg sync.WaitGroup
	results := make([][]FileInfo, len(packages))
	errors := make([]error, len(packages))

	// Launch concurrent requests for different packages
	for i, pkg := range packages {
		wg.Add(1)
		go func(idx int, packageName string) {
			defer wg.Done()
			files, err := client.GetPackageFiles(packageName)
			results[idx] = files
			errors[idx] = err
		}(i, pkg)
	}

	wg.Wait()

	// Verify separate requests for different packages (should be 3 requests)
	finalRequestCount := atomic.LoadInt64(&requestCount)
	if finalRequestCount != int64(len(packages)) {
		t.Errorf("Expected %d HTTP requests for different packages, got %d", len(packages), finalRequestCount)
	}

	// Verify all results are correct
	for i, pkg := range packages {
		if errors[i] != nil {
			t.Errorf("Request for %s failed: %v", pkg, errors[i])
		}

		expectedFilename := pkg + "-1.0.0.tar.gz"
		if len(results[i]) != 1 || results[i][0].Name != expectedFilename {
			t.Errorf("Request for %s got unexpected result: %v", pkg, results[i])
		}
	}
}

// Benchmark tests to measure singleflight performance impact
func BenchmarkClient_GetPackageList_WithSingleflight(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		w.WriteHeader(http.StatusOK)
		response := `{
			"meta": {"api-version": "1.0"},
			"projects": [{"name": "benchmark-package"}]
		}`
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := NewClient(cfg)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := client.GetPackageList()
			if err != nil {
				b.Fatalf("GetPackageList failed: %v", err)
			}
		}
	})
}

func BenchmarkClient_GetPackageFiles_WithSingleflight(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		w.WriteHeader(http.StatusOK)
		response := `{
			"meta": {"api-version": "1.0"},
			"name": "benchmark-package",
			"files": [
				{
					"filename": "benchmark-package-1.0.0.tar.gz",
					"url": "https://files.pythonhosted.org/packages/.../benchmark-package-1.0.0.tar.gz"
				}
			]
		}`
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	cfg := &config.Config{IndexURL: server.URL}
	client := NewClient(cfg)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := client.GetPackageFiles("benchmark-package")
			if err != nil {
				b.Fatalf("GetPackageFiles failed: %v", err)
			}
		}
	})
}

// TestFileInfo_IsYanked tests the IsYanked method with different yanked values
func TestFileInfo_IsYanked(t *testing.T) {
	testCases := []struct {
		name     string
		yanked   interface{}
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
		name         string
		yanked       interface{}
		yankedReason string
		expected     string
	}{
		{"reason from YankedReason field", false, "explicit reason", "explicit reason"},
		{"reason from string yanked", "implicit reason", "", "implicit reason"},
		{"YankedReason takes precedence", "implicit", "explicit", "explicit"},
		{"empty reason", false, "", ""},
		{"nil yanked", nil, "", ""},
		{"bool true yanked no reason", true, "", ""},
		{"empty string yanked", "", "", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fileInfo := &FileInfo{
				Yanked:       tc.yanked,
				YankedReason: tc.yankedReason,
			}
			result := fileInfo.GetYankedReason()
			if result != tc.expected {
				t.Errorf("GetYankedReason() = %q, expected %q", result, tc.expected)
			}
		})
	}
}

// TestClient_ParseHTMLPackageList tests HTML parsing fallback
func TestClient_ParseHTMLPackageList(t *testing.T) {
	client := &Client{}

	testCases := []struct {
		name     string
		html     string
		expected []string
	}{
		{
			name: "simple HTML with packages",
			html: `<!DOCTYPE html>
<html>
<body>
	<a href="numpy/">numpy</a><br/>
	<a href="scipy/">scipy</a><br/>
	<a href="matplotlib/">matplotlib</a><br/>
</body>
</html>`,
			expected: []string{"numpy", "scipy", "matplotlib"},
		},
		{
			name: "HTML with mixed case and extra attributes",
			html: `<html>
<body>
<a href="Django/" class="package">Django</a>
<a href="flask/">flask</a>
<a href="requests/" title="HTTP library">requests</a>
</body>
</html>`,
			expected: []string{"Django", "flask", "requests"},
		},
		{
			name:     "empty HTML",
			html:     `<html><body></body></html>`,
			expected: []string{},
		},
		{
			name: "HTML with non-package links",
			html: `<html>
<body>
<a href="../">Parent Directory</a>
<a href="numpy/">numpy</a>
<a href="scipy/">scipy</a>
</body>
</html>`,
			expected: []string{"Parent Directory", "numpy", "scipy"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := client.parseHTMLPackageList(strings.NewReader(tc.html))
			if err != nil {
				t.Fatalf("parseHTMLPackageList failed: %v", err)
			}

			if len(result) != len(tc.expected) {
				t.Errorf("Expected %d packages, got %d", len(tc.expected), len(result))
				return
			}

			for i, pkg := range result {
				if pkg != tc.expected[i] {
					t.Errorf("Package %d: expected %q, got %q", i, tc.expected[i], pkg)
				}
			}
		})
	}
}

// TestClient_ParseHTMLPackageFiles tests HTML parsing for package files
func TestClient_ParseHTMLPackageFiles(t *testing.T) {
	client := &Client{}

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
				{Name: "numpy-1.21.0.tar.gz", URL: "numpy-1.21.0.tar.gz"},
				{Name: "numpy-1.21.0-py3-none-any.whl", URL: "numpy-1.21.0-py3-none-any.whl"},
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
					URL:  "package-1.0.tar.gz#sha256=abc123",
				},
				{
					Name: "package-1.0.whl",
					URL:  "package-1.0.whl#md5=def456",
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
			result, err := client.parseHTMLPackageFiles(strings.NewReader(tc.html))
			if err != nil {
				t.Fatalf("parseHTMLPackageFiles failed: %v", err)
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
