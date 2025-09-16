package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		info, err := storage.Put(ctx, key, bytes.NewReader(content), int64(len(content)), contentType)
		require.NoError(t, err, "Failed to put object")
		assert.Equal(t, key, info.Key)
		assert.Equal(t, int64(len(content)), info.Size)
		assert.Equal(t, contentType, info.ContentType)
		assert.NotEmpty(t, info.ETag)

		// Test Get
		reader, getInfo, err := storage.Get(ctx, key)
		require.NoError(t, err, "Failed to get object")
		defer func() { _ = reader.Close() }()

		assert.Equal(t, key, getInfo.Key)
		assert.Equal(t, int64(len(content)), getInfo.Size)
		assert.NotEmpty(t, getInfo.ETag)

		data, err := io.ReadAll(reader)
		require.NoError(t, err, "Failed to read object data")
		assert.Equal(t, content, data)

		// Test Delete
		err = storage.Delete(ctx, key)
		assert.NoError(t, err, "Failed to delete test object")

		// Verify deletion
		exists, err := storage.Exists(ctx, key)
		require.NoError(t, err, "Failed to check existence after delete")
		assert.False(t, exists, "Object should not exist after deletion")
	})
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

	t.Run("verify_s3_storage", func(t *testing.T) {
		verifyS3Storage(t)
	})
}

// createTestS3Storage creates an S3 storage instance for testing with MinIO defaults
func createTestS3Storage(t *testing.T) *S3Storage {
	endpoint := getEnvOrDefault("TEST_S3_ENDPOINT", "localhost:9000")
	accessKey := getEnvOrDefault("TEST_S3_ACCESS_KEY", "minioadmin")
	secretKey := getEnvOrDefault("TEST_S3_SECRET_KEY", "minioadmin")
	bucket := getEnvOrDefault("TEST_S3_BUCKET", "groxpi-test")

	cfg := &S3Config{
		Endpoint:        endpoint,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		Region:          "us-east-1",
		Bucket:          bucket,
		Prefix:          "client-test",
		UseSSL:          false,
		ForcePathStyle:  true,
		PartSize:        5 * 1024 * 1024,
		MaxConnections:  10,
		ConnectTimeout:  30 * time.Second,
		RequestTimeout:  5 * time.Minute,
	}

	storage, err := NewS3Storage(cfg)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			t.Skipf("MinIO not available, skipping S3 integration test: %v", err)
		}
		require.NoError(t, err, "Failed to create S3 storage")
	}

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

	for i := 0; i < 30; i++ {
		if resp, err := client.Get(url + "/health"); err == nil {
			resp.Body.Close()
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

// verifyS3Storage verifies that packages are actually stored in S3
func verifyS3Storage(t *testing.T) {
	storage := createTestS3Storage(t)
	defer func() { _ = storage.Close() }()

	ctx := context.Background()

	// List objects to verify files were stored
	objects, err := storage.List(ctx, ListOptions{
		Prefix:  "client-test",
		MaxKeys: 100,
	})
	require.NoError(t, err, "Failed to list S3 objects")

	// Should have at least some cached files
	assert.NotEmpty(t, objects, "Expected some files to be cached in S3")

	t.Logf("Found %d objects in S3 storage", len(objects))
	for _, obj := range objects {
		t.Logf("  - %s (size: %d)", obj.Key, obj.Size)
	}
}

// getEnvOrDefault returns environment variable value or default
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
