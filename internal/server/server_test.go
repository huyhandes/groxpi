package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huybui/groxpi/internal/config"
)

func TestNew(t *testing.T) {
	cfg := &config.Config{
		IndexURL:    "https://pypi.org/simple/",
		CacheSize:   1024 * 1024 * 1024, // 1GB
		CacheDir:    "/tmp/test-cache",
		IndexTTL:    30 * time.Minute,
		LogLevel:    "INFO",
	}
	
	server := New(cfg)
	
	if server == nil {
		t.Fatal("New() returned nil")
	}
	
	if server.config != cfg {
		t.Error("Server config not set correctly")
	}
	
	if server.app == nil {
		t.Error("Fiber app not initialized")
	}
	
	if server.indexCache == nil {
		t.Error("Index cache not initialized")
	}
	
	if server.fileCache == nil {
		t.Error("File cache not initialized")
	}
	
	if server.pypiClient == nil {
		t.Error("PyPI client not initialized")
	}
}

func TestServer_HandleHome(t *testing.T) {
	cfg := &config.Config{
		IndexURL:    "https://pypi.org/simple/",
		CacheSize:   1024 * 1024 * 1024,
		CacheDir:    "/tmp/test-cache",
		IndexTTL:    30 * time.Minute,
		LogLevel:    "INFO",
	}
	
	server := New(cfg)
	app := server.App()
	
	req := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
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
		IndexURL:    "https://pypi.org/simple/",
		CacheDir:    "/tmp/test-cache",
	}
	
	server := New(cfg)
	app := server.App()
	
	req := httptest.NewRequest("GET", "/health", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		t.Errorf("Expected JSON content type, got %s", contentType)
	}
	
	var response map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}
	
	if response["status"] != "success" {
		t.Errorf("Expected status 'success', got %v", response["status"])
	}
	
	data, ok := response["data"].(map[string]interface{})
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
	cfg := &config.Config{
		IndexURL: "https://pypi.org/simple/",
		CacheDir: "/tmp/test-cache",
	}
	
	server := New(cfg)
	app := server.App()
	
	req := httptest.NewRequest("GET", "/index/", nil)
	req.Header.Set("Accept", "text/html")
	
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
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
	if !strings.Contains(bodyStr, "Simple index") {
		t.Error("Response should contain 'Simple index'")
	}
}

func TestServer_HandleListPackages_JSON(t *testing.T) {
	cfg := &config.Config{
		IndexURL: "https://pypi.org/simple/",
		CacheDir: "/tmp/test-cache",
	}
	
	server := New(cfg)
	app := server.App()
	
	req := httptest.NewRequest("GET", "/index/", nil)
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
	
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/vnd.pypi.simple.v1+json") {
		t.Errorf("Expected PyPI JSON content type, got %s", contentType)
	}
	
	var response map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}
	
	meta, ok := response["meta"].(map[string]interface{})
	if !ok {
		t.Error("Expected meta to be an object")
	}
	
	if meta["api-version"] != "1.0" {
		t.Errorf("Expected api-version '1.0', got %v", meta["api-version"])
	}
	
	projects, ok := response["projects"].([]interface{})
	if !ok {
		t.Error("Expected projects to be an array")
	}
	
	// Should be empty since we haven't populated the cache
	if len(projects) != 0 {
		t.Errorf("Expected empty projects array, got %d items", len(projects))
	}
}

func TestServer_HandleListFiles(t *testing.T) {
	cfg := &config.Config{
		IndexURL: "https://pypi.org/simple/",
		CacheDir: "/tmp/test-cache",
	}
	
	server := New(cfg)
	app := server.App()
	
	req := httptest.NewRequest("GET", "/index/nonexistent-test-package-xyz", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	// Should return 404 since package doesn't exist
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}
}

func TestServer_HandleDownloadFile(t *testing.T) {
	cfg := &config.Config{
		IndexURL: "https://pypi.org/simple/",
		CacheDir: "/tmp/test-cache",
	}
	
	server := New(cfg)
	app := server.App()
	
	req := httptest.NewRequest("GET", "/index/numpy/numpy-1.21.0-py3-none-any.whl", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	// Should return 404 since file is not cached
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}
}

func TestServer_HandleCacheList(t *testing.T) {
	cfg := &config.Config{
		IndexURL: "https://pypi.org/simple/",
		CacheDir: "/tmp/test-cache",
	}
	
	server := New(cfg)
	app := server.App()
	
	// Test DELETE method
	req := httptest.NewRequest("DELETE", "/cache/list", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	
	var response map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}
	
	if response["status"] != "success" {
		t.Errorf("Expected status 'success', got %v", response["status"])
	}
	
	// Test wrong method
	req = httptest.NewRequest("GET", "/cache/list", nil)
	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", resp.StatusCode)
	}
}

func TestServer_HandleCachePackage(t *testing.T) {
	cfg := &config.Config{
		IndexURL: "https://pypi.org/simple/",
		CacheDir: "/tmp/test-cache",
	}
	
	server := New(cfg)
	app := server.App()
	
	// Test DELETE method with package name
	req := httptest.NewRequest("DELETE", "/cache/numpy", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	
	var response map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}
	
	if response["status"] != "success" {
		t.Errorf("Expected status 'success', got %v", response["status"])
	}
}

func TestServer_Handle404(t *testing.T) {
	cfg := &config.Config{
		IndexURL: "https://pypi.org/simple/",
		CacheDir: "/tmp/test-cache",
	}
	
	server := New(cfg)
	app := server.App()
	
	req := httptest.NewRequest("GET", "/non-existent-path", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}
}

// Removed problematic tests that access unexported functions

func TestServer_ContentNegotiation(t *testing.T) {
	cfg := &config.Config{
		IndexURL: "https://pypi.org/simple/",
		CacheDir: "/tmp/test-cache",
	}
	
	server := New(cfg)
	app := server.App()
	
	t.Run("JSON request returns JSON", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index/", nil)
		req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
		
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()
		
		contentType := resp.Header.Get("Content-Type")
		if !strings.Contains(contentType, "json") {
			t.Errorf("Expected JSON content type, got %s", contentType)
		}
	})
	
	t.Run("HTML request returns HTML", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index/", nil)
		req.Header.Set("Accept", "text/html")
		
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()
		
		contentType := resp.Header.Get("Content-Type")
		if !strings.Contains(contentType, "html") {
			t.Errorf("Expected HTML content type, got %s", contentType)
		}
	})
}