package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestS3WithMinIO tests basic S3 operations with MinIO
func TestS3WithMinIO(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping S3 integration test in short mode")
	}

	storage := createTestS3Storage(t)
	defer func() { _ = storage.Close() }()

	ctx := context.Background()

	t.Run("basic_operations", func(t *testing.T) {
		key := fmt.Sprintf("test/%d.txt", time.Now().UnixNano())
		content := []byte("Hello S3 integration test")
		contentType := "text/plain"

		// Test Put
		err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), contentType)
		require.NoError(t, err, "Failed to put object")

		// Test Get
		reader, getInfo, err := storage.Get(ctx, key)
		require.NoError(t, err, "Failed to get object")
		defer func() { _ = reader.Close() }()

		assert.Equal(t, int64(len(content)), getInfo.Size)

		data, err := io.ReadAll(reader)
		require.NoError(t, err, "Failed to read object data")
		assert.Equal(t, content, data)

		// Test Delete
		_, err = storage.DeletePrefix(ctx, key)
		assert.NoError(t, err, "Failed to delete test object")

		// Verify deletion
		_, _, err = storage.Get(ctx, key)
		assert.ErrorIs(t, err, ErrNotFound, "Object should not exist after deletion")
	})
}

// TestS3WithMinIO_RejectsChecksumTrailers puts an object through a proxy that
// answers 400 to any request carrying an aws-chunked checksum trailer, the way
// older MinIO releases do. It is the only assertion that catches the SDK's
// newer default checksum behaviour, and it needs a real server behind it.
func TestS3WithMinIO_RejectsChecksumTrailers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping S3 integration test in short mode")
	}

	target, err := url.Parse("http://" + minioEndpoint(t))
	require.NoError(t, err)

	var rejected atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(target)
	strict := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sendsChecksum(r.Header) {
			rejected.Add(1)
			http.Error(w, "checksums are not supported", http.StatusBadRequest)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer strict.Close()

	storage := newIntegrationS3Storage(t, strict.URL)
	defer func() { _ = storage.Close() }()

	ctx := context.Background()
	key := fmt.Sprintf("test/no-trailers-%d.txt", time.Now().UnixNano())
	content := []byte("no checksum trailers here")

	err = storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain")
	require.NoError(t, err, "put must succeed against a server that rejects checksum trailers")

	assert.Zero(t, rejected.Load(), "the client sent a checksum trailer")

	reader, _, err := storage.Get(ctx, key)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, content, data)

	_, err = storage.DeletePrefix(ctx, key)
	require.NoError(t, err)
}

// sendsChecksum reports whether a request carries the checksum trailer or
// header the SDK now emits by default and older MinIO releases reject.
func sendsChecksum(h http.Header) bool {
	if h.Get("X-Amz-Trailer") != "" ||
		strings.Contains(strings.ToLower(h.Get("Content-Encoding")), "aws-chunked") {
		return true
	}
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-checksum-") ||
			strings.EqualFold(name, "X-Amz-Sdk-Checksum-Algorithm") {
			return true
		}
	}
	return false
}

// TestS3WithRealClients tests that real package managers work with S3 backend
func TestS3WithRealClients(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping S3 client integration test in short mode")
	}

	// Only run in CI where MinIO service is available
	if os.Getenv("TEST_S3_ENDPOINT") == "" {
		t.Skip("Skipping S3 client integration test: TEST_S3_ENDPOINT not set")
	}

	// Check if groxpi binary exists or build it
	groxpiBinary := buildGroxpiBinary(t)

	// Create test directory
	testDir := t.TempDir()

	// Start groxpi server with S3 backend
	server := startGroxpiServer(t, groxpiBinary)
	defer server.Stop()

	// Wait for server to be ready
	waitForServer(t, "http://127.0.0.1:5000")

	t.Run("pip_install", func(t *testing.T) {
		testPipInstall(t, testDir)
	})

	t.Run("uv_add", func(t *testing.T) {
		testUVAdd(t, testDir)
	})

}

// createTestS3Storage creates an S3 storage instance pointed at MinIO.
func createTestS3Storage(t *testing.T) *S3Storage {
	return newIntegrationS3Storage(t, minioEndpoint(t))
}

// minioEndpoint returns the MinIO endpoint under test, skipping the test when
// nothing is listening there rather than failing it.
func minioEndpoint(t *testing.T) string {
	t.Helper()

	endpoint := strings.TrimPrefix(getEnvOrDefault("TEST_S3_ENDPOINT", "localhost:9000"), "http://")
	conn, err := net.DialTimeout("tcp", endpoint, 2*time.Second)
	if err != nil {
		t.Skipf("MinIO not available at %s, skipping S3 integration test: %v", endpoint, err)
	}
	_ = conn.Close()

	return endpoint
}

// newIntegrationS3Storage builds a backend against endpoint. Path-style
// addressing is on: MinIO cannot resolve virtual-hosted bucket names, so a round
// trip here is itself the proof that path-style addressing is in use.
func newIntegrationS3Storage(t *testing.T, endpoint string) *S3Storage {
	t.Helper()

	storage, err := NewS3Storage(&S3Config{
		Endpoint:        endpoint,
		AccessKeyID:     getEnvOrDefault("TEST_S3_ACCESS_KEY", "minioadmin"),
		SecretAccessKey: getEnvOrDefault("TEST_S3_SECRET_KEY", "minioadmin"),
		Region:          "us-east-1",
		Bucket:          getEnvOrDefault("TEST_S3_BUCKET", "groxpi-test"),
		Prefix:          "client-test",
		UseSSL:          false,
		ForcePathStyle:  true,
		ConnectTimeout:  30 * time.Second,
		RequestTimeout:  5 * time.Minute,
	})
	require.NoError(t, err, "Failed to create S3 storage")

	return storage
}

// buildGroxpiBinary builds the groxpi binary if it doesn't exist
func buildGroxpiBinary(t *testing.T) string {
	groxpiBinary := filepath.Join("..", "..", "groxpi")
	absGroxpiBinary, err := filepath.Abs(groxpiBinary)
	require.NoError(t, err)

	if _, err := os.Stat(absGroxpiBinary); os.IsNotExist(err) {
		t.Log("Building groxpi binary for testing...")
		cmd := exec.Command("go", "build", "-o", absGroxpiBinary,
			filepath.Join("..", "..", "cmd", "groxpi", "main.go"))
		require.NoError(t, cmd.Run(), "Failed to build groxpi binary")
	}

	return absGroxpiBinary
}

// groxpiServer represents a running groxpi server process
type groxpiServer struct {
	cmd *exec.Cmd
	t   *testing.T
}

func (gs *groxpiServer) Stop() {
	if gs.cmd != nil && gs.cmd.Process != nil {
		gs.t.Log("Stopping groxpi server...")
		_ = gs.cmd.Process.Kill()
		_ = gs.cmd.Wait()
	}
}

// startGroxpiServer starts groxpi with S3 backend configuration
func startGroxpiServer(t *testing.T, groxpiBinary string) *groxpiServer {
	cmd := exec.Command(groxpiBinary)
	cmd.Env = append(os.Environ(),
		"GROXPI_STORAGE_TYPE=s3",
		"AWS_ENDPOINT_URL=http://127.0.0.1:9000",
		"AWS_ACCESS_KEY_ID=minioadmin",
		"AWS_SECRET_ACCESS_KEY=minioadmin",
		"GROXPI_S3_BUCKET=groxpi-test",
		"GROXPI_S3_PREFIX=client-test",
		"GROXPI_S3_USE_SSL=false",
		"GROXPI_S3_FORCE_PATH_STYLE=true",
		"GROXPI_LOGGING_LEVEL=INFO",
		"PORT=5000",
	)

	err := cmd.Start()
	require.NoError(t, err, "Failed to start groxpi server")

	return &groxpiServer{cmd: cmd, t: t}
}

// waitForServer waits for the server to be ready
func waitForServer(t *testing.T, url string) {
	client := &http.Client{Timeout: 5 * time.Second}

	for range 30 {
		if resp, err := client.Get(url + "/health"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				t.Log("Groxpi server is ready")
				return
			}
		}
		time.Sleep(1 * time.Second)
	}

	t.Fatal("Groxpi server did not become ready within 30 seconds")
}

// testPipInstall tests pip package installation through groxpi
func testPipInstall(t *testing.T, testDir string) {
	venvDir := filepath.Join(testDir, "pip-venv")

	// Create virtual environment
	cmd := exec.Command("python3", "-m", "venv", venvDir)
	require.NoError(t, cmd.Run(), "Failed to create virtual environment")

	// Install package using pip with groxpi index
	pipBin := filepath.Join(venvDir, "bin", "pip")
	if _, err := os.Stat(pipBin); os.IsNotExist(err) {
		// Windows
		pipBin = filepath.Join(venvDir, "Scripts", "pip.exe")
	}

	cmd = exec.Command(pipBin, "install", "requests==2.28.0",
		"--index-url", "http://127.0.0.1:5000/simple/", "--trusted-host", "127.0.0.1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "Failed to install requests with pip: %s", string(output))

	t.Log("Successfully installed requests package with pip")
}

// testUVAdd tests uv package installation through groxpi
func testUVAdd(t *testing.T, testDir string) {
	// Check if uv is available
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not available, skipping uv test")
	}

	projectDir := filepath.Join(testDir, "uv-project")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Initialize uv project
	cmd := exec.Command("uv", "init", "--no-readme", "--name", "test-project")
	cmd.Dir = projectDir
	require.NoError(t, cmd.Run(), "Failed to initialize uv project")

	// Add package using uv with groxpi index
	cmd = exec.Command("uv", "add", "click", "--default-index", "http://127.0.0.1:5000/simple/")
	cmd.Dir = projectDir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "Failed to add click with uv: %s", string(output))

	// Verify package was added to pyproject.toml
	pyprojectPath := filepath.Join(projectDir, "pyproject.toml")
	content, err := os.ReadFile(pyprojectPath)
	require.NoError(t, err, "Failed to read pyproject.toml")
	assert.Contains(t, string(content), "click", "click package not found in pyproject.toml")

	t.Log("Successfully added click package with uv")
}

// getEnvOrDefault returns environment variable value or default
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
