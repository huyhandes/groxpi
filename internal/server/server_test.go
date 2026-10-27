package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

// deadIndexURL is a loopback index nothing listens on, for tests that never
// ask upstream.
const deadIndexURL = "http://127.0.0.1:1/simple/"

// testRequest performs an HTTP request against the router and returns the response
func testRequest(router http.Handler, req *http.Request) *http.Response {
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

func TestNew(t *testing.T) {
	cfg := &config.Config{
		IndexURL:  deadIndexURL,
		CacheSize: 1024 * 1024 * 1024, // 1GB
		CacheDir:  t.TempDir(),
		IndexTTL:  30 * time.Minute,
		LogLevel:  "INFO",
	}

	srv := newTestServer(t, cfg)

	if srv == nil {
		t.Fatal("New() returned nil")
	}

	// Test that server has required components initialized
	if srv.Router() == nil {
		t.Error("Gin router not initialized")
	}

	// Test server interface - we can't access private fields
	// Note: We'll test the server functionality through HTTP requests
}

func TestServer_HandleHome(t *testing.T) {
	cfg := &config.Config{
		IndexURL:  deadIndexURL,
		CacheSize: 1024 * 1024 * 1024,
		CacheDir:  t.TempDir(),
		IndexTTL:  30 * time.Minute,
		LogLevel:  "INFO",
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	req := httptest.NewRequest("GET", "/", nil)
	resp := testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Errorf("Expected HTML content type, got %s", contentType)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, "groxpi - PyPI Cache") {
		t.Error("Response should contain title 'groxpi - PyPI Cache'")
	}

	if !strings.Contains(bodyStr, cfg.IndexURL) {
		t.Error("Response should contain index URL")
	}
}

func TestServer_HandleHealth(t *testing.T) {
	cfg := &config.Config{
		IndexURL: deadIndexURL,
		CacheDir: t.TempDir(),
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	req := httptest.NewRequest("GET", "/health", nil)
	resp := testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		t.Errorf("Expected JSON content type, got %s", contentType)
	}

	var response map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	if response["status"] != "success" {
		t.Errorf("Expected status 'success', got %v", response["status"])
	}

	data, ok := response["data"].(map[string]any)
	if !ok {
		t.Error("Expected data to be an object")
	}

	if data["index_url"] != cfg.IndexURL {
		t.Errorf("Expected index_url '%s', got %v", cfg.IndexURL, data["index_url"])
	}

	if data["cache_dir"] != cfg.CacheDir {
		t.Errorf("Expected cache_dir '%s', got %v", cfg.CacheDir, data["cache_dir"])
	}
}

func TestServer_HandleListPackages_HTML(t *testing.T) {
	up := NewFakePyPI(t)
	router := newProxyServer(t, up.URL)

	req := httptest.NewRequest("GET", "/index/", nil)
	req.Header.Set("Accept", "text/html")

	resp := testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Errorf("Expected HTML content type, got %s", contentType)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	if !strings.Contains(string(body), "demo") {
		t.Error("Response should contain the upstream's package list")
	}
}

func TestServer_HandleListPackages_JSON(t *testing.T) {
	up := NewFakePyPI(t)
	router := newProxyServer(t, up.URL)

	req := httptest.NewRequest("GET", "/index/", nil)
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

	resp := testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/vnd.pypi.simple.v1+json") {
		t.Errorf("Expected PyPI JSON content type, got %s", contentType)
	}

	var response map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	meta, ok := response["meta"].(map[string]any)
	if !ok {
		t.Error("Expected meta to be an object")
	}

	if meta["api-version"] != "1.0" {
		t.Errorf("Expected api-version '1.0', got %v", meta["api-version"])
	}

	projects, ok := response["projects"].([]any)
	if !ok || len(projects) != 1 {
		t.Fatalf("Expected the upstream's one project, got %v", response["projects"])
	}
	if name := projects[0].(map[string]any)["name"]; name != "demo" {
		t.Errorf("Expected project 'demo', got %v", name)
	}
}

func TestServer_HandleListFiles(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	cfg := &config.Config{
		IndexURL: upstream.URL + "/simple/",
		CacheDir: t.TempDir(),
		LogLevel: "INFO",
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	req := httptest.NewRequest("GET", "/index/nonexistent-test-package-xyz", nil)
	resp := testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	// Should return 404 since package doesn't exist
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}
}

func TestServer_HandleDownloadFile(t *testing.T) {
	up := NewFakePyPI(t)
	cfg := &config.Config{
		IndexURL: up.URL,
		CacheDir: t.TempDir(),
		LogLevel: "INFO",
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	req := httptest.NewRequest("GET", "/index/numpy/numpy-1.21.0-py3-none-any.whl", nil)
	resp := testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	// The upstream does not know the package, so the file is not found.
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}
	assert.Equal(t, 1, up.IndexHits("numpy"), "the file is looked up in the package index")
}

func TestServer_HandleCacheList(t *testing.T) {
	cfg := &config.Config{
		IndexURL:      deadIndexURL,
		CacheDir:      t.TempDir(),
		AdminUsername: adminUser,
		AdminPassword: adminPass,
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	// The cache routes are part of the authenticated administrative group.
	req := httptest.NewRequest("DELETE", "/cache/list", nil)
	req.SetBasicAuth(adminUser, adminPass)
	resp := testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var response map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	if response["status"] != "success" {
		t.Errorf("Expected status 'success', got %v", response["status"])
	}

	// Test wrong method
	req = httptest.NewRequest("GET", "/cache/list", nil)
	resp = testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", resp.StatusCode)
	}
}

func TestServer_HandleCachePackage(t *testing.T) {
	cfg := &config.Config{
		IndexURL:      deadIndexURL,
		CacheDir:      t.TempDir(),
		AdminUsername: adminUser,
		AdminPassword: adminPass,
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	// Test DELETE method with package name
	req := httptest.NewRequest("DELETE", "/cache/numpy", nil)
	req.SetBasicAuth(adminUser, adminPass)
	resp := testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var response map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	if response["status"] != "success" {
		t.Errorf("Expected status 'success', got %v", response["status"])
	}
}

func TestServer_Handle404(t *testing.T) {
	cfg := &config.Config{
		IndexURL: deadIndexURL,
		CacheDir: t.TempDir(),
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	req := httptest.NewRequest("GET", "/non-existent-path", nil)
	resp := testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}
}

// Removed problematic tests that access unexported functions

func TestServer_ContentNegotiation(t *testing.T) {
	up := NewFakePyPI(t)
	router := newProxyServer(t, up.URL)

	t.Run("JSON request returns JSON", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index/", nil)
		req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

		resp := testRequest(router, req)
		defer func() { _ = resp.Body.Close() }()

		contentType := resp.Header.Get("Content-Type")
		if !strings.Contains(contentType, "json") {
			t.Errorf("Expected JSON content type, got %s", contentType)
		}
	})

	t.Run("HTML request returns HTML", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index/", nil)
		req.Header.Set("Accept", "text/html")

		resp := testRequest(router, req)
		defer func() { _ = resp.Body.Close() }()

		contentType := resp.Header.Get("Content-Type")
		if !strings.Contains(contentType, "html") {
			t.Errorf("Expected HTML content type, got %s", contentType)
		}
	})
}

// Additional tests for better coverage
func TestServer_InitStorage_EdgeCases(t *testing.T) {
	t.Run("Invalid storage type", func(t *testing.T) {
		cfg := &config.Config{
			IndexURL:    deadIndexURL,
			StorageType: "invalid",
			CacheDir:    t.TempDir(),
		}

		// A typo must not silently run local storage without the durable store.
		requireLoopback(t, cfg.IndexURL)
		srv, err := NewServer(cfg)
		require.Error(t, err)
		assert.Nil(t, srv)
	})

	t.Run("Local storage configuration", func(t *testing.T) {
		cfg := &config.Config{
			IndexURL:    deadIndexURL,
			StorageType: "local",
			CacheDir:    t.TempDir(),
		}

		srv := newTestServer(t, cfg)

		// Test that server was created successfully with local storage
		if srv == nil {
			t.Error("Server should not be nil with local storage config")
			return
		}

		// Now we can test internal fields since we're in the same package
		if srv.store == nil {
			t.Error("Server storage should not be nil with local config")
		}
	})
}

func TestServer_HandleDownloadFile_EdgeCases(t *testing.T) {
	up := NewFakePyPI(t)
	cfg := &config.Config{
		IndexURL:        up.URL,
		CacheDir:        t.TempDir(),
		DownloadTimeout: 5 * time.Second, // fetch mode; the fake answers at once
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	t.Run("Missing file returns 404", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index/nonexistent/nonexistent-1.0.0.tar.gz", nil)
		resp := testRequest(router, req)
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("Expected status 404, got %d", resp.StatusCode)
		}
	})

	t.Run("Invalid package name", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index/../etc/passwd", nil)
		resp := testRequest(router, req)
		defer func() { _ = resp.Body.Close() }()

		// The mux cleans the path and redirects to the cleaned form, which
		// is outside every route; nothing under the cache directory is read.
		if resp.StatusCode != http.StatusTemporaryRedirect {
			t.Errorf("Expected 307 for path traversal, got %d", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/etc/passwd" {
			t.Errorf("redirect must go to the cleaned path, got %q", loc)
		}
	})
}

func TestServer_HandleListFiles_EdgeCases(t *testing.T) {
	up := NewFakePyPI(t).Add("test-package-123", "test_package_123-1.0.tar.gz", FakeFile{})
	cfg := &config.Config{
		IndexURL: up.URL,
		CacheDir: t.TempDir(),
		IndexTTL: 5 * time.Minute,
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	t.Run("Package with special characters", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index/test-package_123", nil)
		req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

		resp := testRequest(router, req)
		defer func() { _ = resp.Body.Close() }()

		// The name is normalised before it is asked for upstream.
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}
		assert.Equal(t, 1, up.IndexHits("test-package-123"))
	})

	t.Run("Empty package name", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index//", nil)
		resp := testRequest(router, req)
		defer func() { _ = resp.Body.Close() }()

		// The mux cleans the empty segment away and redirects to the root.
		if resp.StatusCode != http.StatusTemporaryRedirect {
			t.Errorf("Expected status 307, got %d", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/index/" {
			t.Errorf("Expected redirect to /index/, got %q", loc)
		}
	})
}

func TestServer_WantsJSON(t *testing.T) {
	// The root index is proxied byte for byte, so the upstream decides the
	// representation from the Accept groxpi forwards, exactly as pypi.org does.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "json") {
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_, _ = w.Write([]byte(`{"meta":{"api-version":"1.0"},"projects":[{"name":"flask"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<a href="flask/">flask</a>`))
	}))
	defer upstream.Close()
	cfg := &config.Config{
		IndexURL: upstream.URL + "/simple/",
		CacheDir: t.TempDir(),
		IndexTTL: time.Hour,
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	tests := []struct {
		name        string
		accept      string
		expectsJSON bool
	}{
		{"JSON Accept header", "application/vnd.pypi.simple.v1+json", true},
		{"JSON with quality", "application/vnd.pypi.simple.v1+json;q=0.8", true},
		{"HTML Accept header", "text/html", false},
		{"Browser accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", false},
		{"Empty accept", "", false},
		{"Wildcard", "*/*", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/index/", nil)
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}

			resp := testRequest(router, req)
			defer func() { _ = resp.Body.Close() }()

			contentType := resp.Header.Get("Content-Type")
			isJSON := strings.Contains(contentType, "json")

			if isJSON != tt.expectsJSON {
				t.Errorf("Expected JSON=%v, got Content-Type=%s", tt.expectsJSON, contentType)
			}
		})
	}
}

func TestServer_NormalizePackageName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Django", "django"},
		{"Flask-Login", "flask-login"},
		{"requests_oauthlib", "requests-oauthlib"},
		{"Pillow", "pillow"},
		{"PyYAML", "pyyaml"},
		{"setuptools_scm", "setuptools-scm"},
		{"SOME_PACKAGE", "some-package"},
		{"", ""},
	}

	up := NewFakePyPI(t)
	for _, tt := range tests {
		if tt.expected != "" {
			up.Add(tt.expected, tt.expected+"-1.0.tar.gz", FakeFile{})
		}
	}
	cfg := &config.Config{IndexURL: up.URL, CacheDir: t.TempDir(), IndexTTL: time.Hour}
	srv := newTestServer(t, cfg)
	router := srv.Router()

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			// The upstream only lists the normalised name, so a 200 proves the
			// request was normalised; "" is the root listing.
			req := httptest.NewRequest("GET", "/index/"+tt.input, nil)
			resp := testRequest(router, req)
			_ = resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode, "package %q", tt.input)
			if tt.expected != "" {
				assert.Equal(t, 1, up.IndexHits(tt.expected), "package %q", tt.input)
			}
		})
	}

	// A mixed-case file URL is served from the normalised cache key, without
	// asking upstream.
	t.Run("mixed-case file URL hits the normalised key", func(t *testing.T) {
		var hits atomic.Int64
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(up.Close)
		cfg := routesConfig(t, up.URL)
		path := filepath.Join(cfg.CacheDir, "packages", "foo-bar", "Foo_Bar-1.0.tar.gz")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte("cached"), 0o600))
		srv := newTestServer(t, cfg)

		resp := do(srv.Router(), httptest.NewRequest("GET", "/simple/Foo_Bar/Foo_Bar-1.0.tar.gz", nil))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "cached", string(body))
		assert.Zero(t, hits.Load(), "a cached file is not fetched again")
	})
}

func TestServer_HandleCacheEdgeCases(t *testing.T) {
	cfg := &config.Config{
		IndexURL:      deadIndexURL,
		CacheDir:      t.TempDir(),
		AdminUsername: adminUser,
		AdminPassword: adminPass,
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	t.Run("Cache list with invalid method", func(t *testing.T) {
		methods := []string{"POST", "PUT", "PATCH"}
		for _, method := range methods {
			req := httptest.NewRequest(method, "/cache/list", nil)
			resp := testRequest(router, req)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("Method %s should return 405, got %d", method, resp.StatusCode)
			}
		}
	})

	t.Run("Cache package with special characters", func(t *testing.T) {
		packages := []string{"test-package", "test_package", "Test.Package"}
		for _, pkg := range packages {
			req := httptest.NewRequest("DELETE", "/cache/"+pkg, nil)
			req.SetBasicAuth(adminUser, adminPass)
			resp := testRequest(router, req)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("DELETE /cache/%s should return 200, got %d", pkg, resp.StatusCode)
			}
		}
	})
}

func TestServer_HealthEndpointDetails(t *testing.T) {
	cfg := &config.Config{
		IndexURL:    deadIndexURL,
		CacheDir:    t.TempDir(),
		CacheSize:   1024 * 1024 * 1024,
		IndexTTL:    30 * time.Minute,
		StorageType: "local",
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	req := httptest.NewRequest("GET", "/health", nil)
	resp := testRequest(router, req)
	defer func() { _ = resp.Body.Close() }()

	var response map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	// Verify all expected fields are present
	expectedFields := []string{"status", "data", "timestamp"}
	for _, field := range expectedFields {
		if _, exists := response[field]; !exists {
			t.Errorf("Health response missing field: %s", field)
		}
	}

	data := response["data"].(map[string]any)
	expectedDataFields := []string{"index_url", "cache_dir", "cache_size", "index_ttl_seconds", "storage_type"}
	for _, field := range expectedDataFields {
		if _, exists := data[field]; !exists {
			t.Errorf("Health data missing field: %s", field)
		}
	}
}

func TestServer_ErrorHandling(t *testing.T) {
	// A closed server: its loopback port refuses every connection.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	cfg := &config.Config{
		IndexURL: closed.URL,
		CacheDir: t.TempDir(),
		IndexTTL: 1 * time.Second,
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	t.Run("Network error handling", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index/", nil)
		req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

		resp := testRequest(router, req)
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("Expected status 502 for an unreachable upstream, got %d", resp.StatusCode)
		}
	})
}

// Singleflight tests for server handlers following TDD principles
func TestServer_SingleflightListPackages(t *testing.T) {
	// Delay so the concurrent requests overlap the one upstream fetch.
	mockPyPI := NewFakePyPI(t).SetDelay(20 * time.Millisecond)

	cfg := &config.Config{
		IndexURL: mockPyPI.URL,
		CacheDir: t.TempDir(),
		IndexTTL: 1 * time.Hour, // Long TTL to avoid cache expiry during test
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	const numConcurrentRequests = 10
	var wg sync.WaitGroup
	responses := make([]*http.Response, numConcurrentRequests)

	// Launch concurrent requests to the same endpoint
	for i := range numConcurrentRequests {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/index/", nil)
			req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

			resp := testRequest(router, req)
			responses[idx] = resp
		}(i)
	}

	wg.Wait()

	// Verify only one HTTP request was made to PyPI due to singleflight
	if hits := mockPyPI.RootHits(); hits != 1 {
		t.Errorf("Expected exactly 1 HTTP request to PyPI due to singleflight, got %d", hits)
	}

	// Verify all server responses are successful
	for i := range numConcurrentRequests {

		if responses[i].StatusCode != http.StatusOK {
			t.Errorf("Request %d got status %d, expected 200", i, responses[i].StatusCode)
			continue
		}

		body, err := io.ReadAll(responses[i].Body)
		_ = responses[i].Body.Close()
		if err != nil {
			t.Errorf("Failed to read response body for request %d: %v", i, err)
			continue
		}

		// Verify response contains expected content (all should have same data due to singleflight)
		var response map[string]any
		if err := json.Unmarshal(body, &response); err != nil {
			t.Errorf("Failed to parse JSON response for request %d: %v", i, err)
			continue
		}

		projects, ok := response["projects"].([]any)
		if !ok {
			t.Errorf("Request %d: expected projects array", i)
			continue
		}

		if len(projects) != 1 {
			t.Errorf("Request %d: expected the upstream's 1 project, got %d", i, len(projects))
		}

		// Verify it contains the expected project names
		projectNames := make(map[string]bool)
		for _, proj := range projects {
			if projMap, ok := proj.(map[string]any); ok {
				if name, ok := projMap["name"].(string); ok {
					projectNames[name] = true
				}
			}
		}

		if !projectNames["demo"] {
			t.Errorf("Request %d: expected the upstream's project demo, got %v", i, projectNames)
		}
	}
}

func TestServer_SingleflightListFiles(t *testing.T) {
	packageName := "test-package"

	// Delay so the concurrent requests overlap the one upstream fetch.
	mockPyPI := NewFakePyPI(t).SetDelay(20*time.Millisecond).
		Add(packageName, packageName+"-1.0.0.tar.gz", FakeFile{}).
		Add(packageName, packageName+"-1.0.0-py3-none-any.whl", FakeFile{})

	cfg := &config.Config{
		IndexURL: mockPyPI.URL,
		CacheDir: t.TempDir(),
		IndexTTL: 1 * time.Hour, // Long TTL to avoid cache expiry during test
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	const numConcurrentRequests = 8
	var wg sync.WaitGroup
	responses := make([]*http.Response, numConcurrentRequests)

	// Launch concurrent requests for the same package
	for i := range numConcurrentRequests {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/index/"+packageName, nil)
			req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

			resp := testRequest(router, req)
			responses[idx] = resp
		}(i)
	}

	wg.Wait()

	// Verify only one HTTP request was made to PyPI due to singleflight
	if hits := mockPyPI.IndexHits(packageName); hits != 1 {
		t.Errorf("Expected exactly 1 HTTP request to PyPI due to singleflight, got %d", hits)
	}

	// Verify all server responses are successful
	for i := range numConcurrentRequests {

		if responses[i].StatusCode != http.StatusOK {
			t.Errorf("Request %d got status %d, expected 200", i, responses[i].StatusCode)
			continue
		}

		body, err := io.ReadAll(responses[i].Body)
		_ = responses[i].Body.Close()
		if err != nil {
			t.Errorf("Failed to read response body for request %d: %v", i, err)
			continue
		}

		// Verify response contains expected content (all should have same data due to singleflight)
		var response map[string]any
		if err := json.Unmarshal(body, &response); err != nil {
			t.Errorf("Failed to parse JSON response for request %d: %v", i, err)
			continue
		}

		files, ok := response["files"].([]any)
		if !ok {
			t.Errorf("Request %d: expected files array", i)
			continue
		}

		if len(files) != 2 {
			t.Errorf("Request %d: expected 2 files, got %d", i, len(files))
		}

		if response["name"] != packageName {
			t.Errorf("Request %d: expected name '%s', got %v", i, packageName, response["name"])
		}

		// Verify it contains the expected file names
		fileNames := make(map[string]bool)
		for _, file := range files {
			if fileMap, ok := file.(map[string]any); ok {
				if name, ok := fileMap["filename"].(string); ok {
					fileNames[name] = true
				}
			}
		}

		expectedFiles := []string{packageName + "-1.0.0.tar.gz", packageName + "-1.0.0-py3-none-any.whl"}
		for _, expectedFile := range expectedFiles {
			if !fileNames[expectedFile] {
				t.Errorf("Request %d: expected file %s not found, got %v", i, expectedFile, fileNames)
			}
		}
	}
}

func TestServer_SingleflightErrorPropagation(t *testing.T) {
	// Root listing answers 500, delayed so the concurrent requests overlap.
	mockPyPI := NewFakePyPI(t).SetDelay(20 * time.Millisecond).Fail("")

	cfg := &config.Config{
		IndexURL: mockPyPI.URL,
		CacheDir: t.TempDir(),
		IndexTTL: 1 * time.Hour,
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	const numConcurrentRequests = 6
	var wg sync.WaitGroup
	responses := make([]*http.Response, numConcurrentRequests)

	// Launch concurrent requests that should all fail
	for i := range numConcurrentRequests {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/index/", nil)
			req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

			resp := testRequest(router, req)
			responses[idx] = resp
		}(i)
	}

	wg.Wait()

	// Verify only one HTTP request was made to PyPI due to singleflight
	if hits := mockPyPI.RootHits(); hits != 1 {
		t.Errorf("Expected exactly 1 HTTP request to PyPI due to singleflight, got %d", hits)
	}

	// The root index is a byte-level proxy, so the upstream failure is reported
	// verbatim rather than dressed up as an empty list.
	for i := range numConcurrentRequests {
		if responses[i].StatusCode != http.StatusInternalServerError {
			t.Errorf("Request %d got status %d, expected the upstream's 500", i, responses[i].StatusCode)
		}

		body, err := io.ReadAll(responses[i].Body)
		_ = responses[i].Body.Close()
		if err != nil {
			t.Errorf("Failed to read response body for request %d: %v", i, err)
			continue
		}
		if string(body) != "boom\n" {
			t.Errorf("Request %d: expected the upstream body, got %q", i, body)
		}
	}
}

func TestServer_SingleflightDifferentPackages(t *testing.T) {
	packages := []string{"package-a", "package-b", "package-c"}
	mockPyPI := NewFakePyPI(t).SetDelay(15 * time.Millisecond)
	for _, pkg := range packages {
		mockPyPI.Add(pkg, pkg+"-1.0.0.tar.gz", FakeFile{})
	}

	cfg := &config.Config{
		IndexURL: mockPyPI.URL,
		CacheDir: t.TempDir(),
		IndexTTL: 1 * time.Hour,
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	var wg sync.WaitGroup
	responses := make([]*http.Response, len(packages))

	// Launch concurrent requests for different packages
	for i, pkg := range packages {
		wg.Add(1)
		go func(idx int, packageName string) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/index/"+packageName, nil)
			req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

			resp := testRequest(router, req)
			responses[idx] = resp
		}(i, pkg)
	}

	wg.Wait()

	// Verify separate requests for different packages (one each)
	for _, pkg := range packages {
		if hits := mockPyPI.IndexHits(pkg); hits != 1 {
			t.Errorf("Expected 1 HTTP request for %s, got %d", pkg, hits)
		}
	}

	// Verify all results are correct (collect results first to avoid race conditions)
	responseData := make(map[string]map[string]any)

	for i := range packages {

		if responses[i].StatusCode != http.StatusOK {
			t.Errorf("Request %d got status %d, expected 200", i, responses[i].StatusCode)
			continue
		}

		body, err := io.ReadAll(responses[i].Body)
		_ = responses[i].Body.Close()
		if err != nil {
			t.Errorf("Failed to read response body for request %d: %v", i, err)
			continue
		}

		var response map[string]any
		if err := json.Unmarshal(body, &response); err != nil {
			t.Errorf("Failed to parse JSON response for request %d: %v", i, err)
			continue
		}

		// Store response data by package name to avoid race condition issues
		if pkgName, ok := response["name"].(string); ok {
			responseData[pkgName] = response
		}
	}

	// Now verify each expected package was returned
	for _, pkg := range packages {
		response, ok := responseData[pkg]
		if !ok {
			t.Errorf("No response found for package %s", pkg)
			continue
		}

		files, ok := response["files"].([]any)
		if !ok || len(files) != 1 {
			t.Errorf("Expected 1 file for %s, got %v", pkg, files)
			continue
		}

		file := files[0].(map[string]any)
		expectedFilename := pkg + "-1.0.0.tar.gz"
		if file["filename"] != expectedFilename {
			t.Errorf("Expected filename '%s', got %v", expectedFilename, file["filename"])
		}
	}
}

// Benchmark tests to measure singleflight performance impact on server handlers
func BenchmarkServer_HandleListPackages_WithSingleflight(b *testing.B) {
	mockPyPI := NewFakePyPI(b)

	cfg := &config.Config{
		IndexURL: mockPyPI.URL,
		CacheDir: b.TempDir(),
		IndexTTL: 1 * time.Second, // Short TTL for benchmarking
	}

	srv := newTestServer(b, cfg)
	router := srv.Router()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("GET", "/index/", nil)
			req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

			resp := testRequest(router, req)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				b.Fatalf("Expected status 200, got %d", resp.StatusCode)
			}
		}
	})
}

func BenchmarkServer_HandleListFiles_WithSingleflight(b *testing.B) {
	mockPyPI := NewFakePyPI(b).Add("benchmark-package", "benchmark-package-1.0.0.tar.gz", FakeFile{})

	cfg := &config.Config{
		IndexURL: mockPyPI.URL,
		CacheDir: b.TempDir(),
		IndexTTL: 1 * time.Second, // Short TTL for benchmarking
	}

	srv := newTestServer(b, cfg)
	router := srv.Router()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("GET", "/index/benchmark-package", nil)
			req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

			resp := testRequest(router, req)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				b.Fatalf("Expected status 200, got %d", resp.StatusCode)
			}
		}
	})
}

// Test URL rewriting functionality to ensure packages are downloaded through proxy
func TestServer_URLRewriting(t *testing.T) {
	packageName := "test-package"

	// The fake's index lists absolute URLs on its own host.
	mockPyPI := NewFakePyPI(t).
		Add(packageName, "test-package-1.0.0.tar.gz", FakeFile{}).
		Add(packageName, "test-package-1.0.0-py3-none-any.whl", FakeFile{})

	cfg := &config.Config{
		IndexURL: mockPyPI.URL,
		CacheDir: t.TempDir(),
		IndexTTL: 1 * time.Hour,
	}

	srv := newTestServer(t, cfg)
	router := srv.Router()

	t.Run("JSON response URLs rewritten to proxy", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index/"+packageName, nil)
		req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

		resp := testRequest(router, req)
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Expected status 200, got %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response body: %v", err)
		}

		var response map[string]any
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatalf("Failed to parse JSON response: %v", err)
		}

		files, ok := response["files"].([]any)
		if !ok {
			t.Fatal("Expected files array")
		}

		if len(files) != 2 {
			t.Fatalf("Expected 2 files, got %d", len(files))
		}

		// Verify URLs are rewritten to point to proxy
		for _, file := range files {
			fileMap := file.(map[string]any)
			filename := fileMap["filename"].(string)
			url := fileMap["url"].(string)

			expectedURL := fmt.Sprintf("/simple/%s/%s", packageName, filename)
			if url != expectedURL {
				t.Errorf("Expected URL '%s', got '%s'", expectedURL, url)
			}

			// Ensure URL does not point to the upstream directly
			if strings.Contains(url, mockPyPI.URL) {
				t.Errorf("URL should not contain the upstream %s, got '%s'", mockPyPI.URL, url)
			}
		}
	})

	t.Run("HTML response URLs rewritten to proxy", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index/"+packageName, nil)
		req.Header.Set("Accept", "text/html")

		resp := testRequest(router, req)
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Expected status 200, got %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response body: %v", err)
		}

		bodyStr := string(body)

		// Verify HTML contains proxy URLs, not PyPI URLs
		expectedURLs := []string{
			fmt.Sprintf(`href="/simple/%s/test-package-1.0.0.tar.gz"`, packageName),
			fmt.Sprintf(`href="/simple/%s/test-package-1.0.0-py3-none-any.whl"`, packageName),
		}

		for _, expectedURL := range expectedURLs {
			if !strings.Contains(bodyStr, expectedURL) {
				t.Errorf("Expected HTML to contain '%s', got: %s", expectedURL, bodyStr)
			}
		}

		// Ensure HTML does not contain direct upstream URLs
		if strings.Contains(bodyStr, mockPyPI.URL) {
			t.Errorf("HTML should not contain upstream %s URLs, got: %s", mockPyPI.URL, bodyStr)
		}
	})
}
