package pypi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huybui/groxpi/internal/config"
)

func TestNewClient(t *testing.T) {
	cfg := &config.Config{
		IndexURL:                   "https://pypi.org/simple/",
		DisableSSLVerification:     false,
		ConnectTimeout:             5 * time.Second,
		ReadTimeout:                20 * time.Second,
	}
	
	client := NewClient(cfg)
	
	if client == nil {
		t.Fatal("NewClient() returned nil")
	}
	
	if client.config != cfg {
		t.Error("Client config not set correctly")
	}
	
	if client.httpClient == nil {
		t.Error("HTTP client not initialized")
	}
	
	expectedTimeout := cfg.ConnectTimeout + cfg.ReadTimeout
	if client.httpClient.Timeout != expectedTimeout {
		t.Errorf("Expected timeout %v, got %v", expectedTimeout, client.httpClient.Timeout)
	}
}

func TestNewClient_SSLVerificationDisabled(t *testing.T) {
	cfg := &config.Config{
		IndexURL:                   "https://pypi.org/simple/",
		DisableSSLVerification:     true,
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
		w.Write([]byte(`{"test": "response"}`))
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
		w.Write([]byte(response))
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
		w.Write([]byte(response))
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