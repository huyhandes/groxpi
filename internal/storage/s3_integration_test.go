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

	// Opt-in: besides the MinIO named by TEST_S3_ENDPOINT this needs python3,
	// uv and network access to PyPI, which the MinIO-only tests do not.
	if os.Getenv("TEST_S3_ENDPOINT") == "" {
		t.Skip("Skipping S3 client integration test: TEST_S3_ENDPOINT not set")
	}
	endpoint := minioEndpoint(t)

	groxpiBinary := buildGroxpiBinary(t)
	testDir := t.TempDir()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	server := startGroxpiServer(t, groxpiBinary, baseURL, endpoint)
	defer server.Stop()
	server.waitReady(baseURL)

	t.Run("pip_install", func(t *testing.T) {
		testPipInstall(t, testDir, baseURL)
	})

	t.Run("uv_add", func(t *testing.T) {
		testUVAdd(t, testDir, baseURL)
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
	})
	require.NoError(t, err, "Failed to create S3 storage")

	return storage
}

// buildGroxpiBinary builds the current source into a temporary directory, so
// the test never runs a stale binary left in the repository.
func buildGroxpiBinary(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "groxpi")
	cmd := exec.Command("go", "build", "-o", binary, "github.com/huyhandes/groxpi/cmd/groxpi")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "Failed to build groxpi binary: %s", output)

	return binary
}

// freePort asks the kernel for a port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// groxpiServer is a running groxpi child process. exited closes when the
// process ends, so a crash at startup fails the test at once instead of after
// the whole readiness wait.
type groxpiServer struct {
	t      *testing.T
	cmd    *exec.Cmd
	output *bytes.Buffer
	exited chan struct{}
}

func (gs *groxpiServer) Stop() {
	_ = gs.cmd.Process.Kill()
	<-gs.exited
}

// startGroxpiServer starts groxpi listening on baseURL's port, storing into the
// MinIO the other integration tests use.
func startGroxpiServer(t *testing.T, groxpiBinary, baseURL, endpoint string) *groxpiServer {
	t.Helper()

	u, err := url.Parse(baseURL)
	require.NoError(t, err)

	cmd := exec.Command(groxpiBinary)
	cmd.Env = append(os.Environ(),
		"GROXPI_STORAGE_TYPE=s3",
		"AWS_ENDPOINT_URL=http://"+endpoint,
		"AWS_ACCESS_KEY_ID="+getEnvOrDefault("TEST_S3_ACCESS_KEY", "minioadmin"),
		"AWS_SECRET_ACCESS_KEY="+getEnvOrDefault("TEST_S3_SECRET_KEY", "minioadmin"),
		"GROXPI_S3_BUCKET="+getEnvOrDefault("TEST_S3_BUCKET", "groxpi-test"),
		"GROXPI_S3_PREFIX=client-test",
		"GROXPI_S3_USE_SSL=false",
		"GROXPI_S3_FORCE_PATH_STYLE=true",
		"GROXPI_CACHE_DIR="+t.TempDir(),
		"GROXPI_LOGGING_LEVEL=INFO",
		"PORT="+u.Port(),
	)
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output

	require.NoError(t, cmd.Start(), "Failed to start groxpi server")

	gs := &groxpiServer{t: t, cmd: cmd, output: output, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(gs.exited)
	}()
	return gs
}

// waitReady polls /health until the server answers, failing at once if the
// process exits first.
func (gs *groxpiServer) waitReady(baseURL string) {
	gs.t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	for {
		if resp, err := client.Get(baseURL + "/health"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-gs.exited:
			gs.t.Fatalf("groxpi exited before becoming ready: %v\n%s", gs.cmd.ProcessState, gs.output)
		case <-deadline:
			gs.t.Fatal("groxpi did not become ready within 30 seconds")
		case <-tick.C:
		}
	}
}

// testPipInstall tests pip package installation through groxpi
func testPipInstall(t *testing.T, testDir, baseURL string) {
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
		"--index-url", baseURL+"/simple/", "--trusted-host", "127.0.0.1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "Failed to install requests with pip: %s", string(output))

	t.Log("Successfully installed requests package with pip")
}

// testUVAdd tests uv package installation through groxpi
func testUVAdd(t *testing.T, testDir, baseURL string) {
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
	cmd = exec.Command("uv", "add", "click", "--default-index", baseURL+"/simple/")
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
