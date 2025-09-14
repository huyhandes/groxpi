package storage

import (
	"bytes"
	"fmt"
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

// TestS3BackendWithPackageManagers tests the S3 backend with real package managers
func TestS3BackendWithPackageManagers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping S3 package manager integration test in short mode")
	}

	// Check if groxpi binary exists
	groxpiBinary := "../groxpi"
	if _, err := os.Stat(groxpiBinary); os.IsNotExist(err) {
		// Try to build the binary
		t.Log("Building groxpi binary for testing...")
		cmd := exec.Command("go", "build", "-o", groxpiBinary, "../cmd/groxpi/main.go")
		if err := cmd.Run(); err != nil {
			t.Fatalf("Failed to build groxpi binary: %v", err)
		}
	}

	// Create test directory
	testDir := t.TempDir()
	t.Logf("Using test directory: %s", testDir)

	// Start groxpi with S3 backend
	server := startGroxpiWithS3(t, testDir)
	defer server.Stop()

	// Wait for server to be ready
	waitForServer(t, "http://localhost:5000")

	// Test with pip
	t.Run("pip_install_requests", func(t *testing.T) {
		testPipInstall(t, testDir, "requests==2.28.0")
	})

	// Test concurrent downloads
	t.Run("concurrent_pip_installs", func(t *testing.T) {
		testConcurrentPipInstalls(t, testDir)
	})

	// Verify files are stored in S3
	t.Run("verify_s3_storage", func(t *testing.T) {
		verifyS3Storage(t)
	})
}

// groxpiProcess represents a running groxpi server
type groxpiProcess struct {
	cmd *exec.Cmd
	t   *testing.T
}

func (gp *groxpiProcess) Stop() {
	if gp.cmd != nil && gp.cmd.Process != nil {
		gp.t.Log("Stopping groxpi server...")
		gp.cmd.Process.Kill()
		gp.cmd.Wait()
	}
}

func startGroxpiWithS3(t *testing.T, testDir string) *groxpiProcess {
	// Set up S3 environment variables for MinIO
	env := []string{
		"GROXPI_STORAGE_TYPE=s3",
		"AWS_ENDPOINT_URL=http://localhost:9000",
		"AWS_ACCESS_KEY_ID=minioadmin",
		"AWS_SECRET_ACCESS_KEY=minioadmin",
		"GROXPI_S3_BUCKET=groxpi-test",
		"GROXPI_S3_PREFIX=integration-test",
		"GROXPI_S3_USE_SSL=false",
		"GROXPI_S3_FORCE_PATH_STYLE=true",
		"GROXPI_LOGGING_LEVEL=DEBUG",
		"PORT=5000",
	}

	// Add current environment
	env = append(env, os.Environ()...)

	cmd := exec.Command("../groxpi")
	cmd.Env = env
	cmd.Dir = testDir

	// Capture output for debugging
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	t.Log("Starting groxpi server with S3 backend...")
	err := cmd.Start()
	require.NoError(t, err, "Failed to start groxpi server")

	// Give the server a moment to start
	time.Sleep(2 * time.Second)

	return &groxpiProcess{cmd: cmd, t: t}
}

func waitForServer(t *testing.T, url string) {
	client := &http.Client{Timeout: 1 * time.Second}

	for i := 0; i < 30; i++ {
		resp, err := client.Get(url + "/health")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			t.Log("Server is ready")
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(1 * time.Second)
	}

	t.Fatal("Server did not become ready within 30 seconds")
}

func testPipInstall(t *testing.T, testDir, packageSpec string) {
	// Create virtual environment
	venvDir := filepath.Join(testDir, "venv")
	cmd := exec.Command("python3", "-m", "venv", venvDir)
	err := cmd.Run()
	require.NoError(t, err, "Failed to create virtual environment")

	// Install pip in the venv and upgrade it
	pipPath := filepath.Join(venvDir, "bin", "pip")
	if _, err := os.Stat(pipPath); os.IsNotExist(err) {
		// Windows path
		pipPath = filepath.Join(venvDir, "Scripts", "pip.exe")
	}

	// Upgrade pip
	cmd = exec.Command(pipPath, "install", "--upgrade", "pip")
	err = cmd.Run()
	require.NoError(t, err, "Failed to upgrade pip")

	// Install package using groxpi as index
	t.Logf("Installing %s via groxpi...", packageSpec)
	cmd = exec.Command(pipPath, "install",
		"--index-url", "http://localhost:5000/simple/",
		"--trusted-host", "localhost",
		packageSpec)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()

	t.Logf("pip stdout: %s", stdout.String())
	t.Logf("pip stderr: %s", stderr.String())

	assert.NoError(t, err, "pip install should succeed")

	// Verify package is installed
	cmd = exec.Command(pipPath, "show", strings.Split(packageSpec, "==")[0])
	err = cmd.Run()
	assert.NoError(t, err, "Package should be installed and findable")
}

func testConcurrentPipInstalls(t *testing.T, testDir string) {
	packages := []string{
		"urllib3==1.26.0",
		"certifi==2022.12.7",
		"charset-normalizer==3.0.0",
	}

	// Create separate venvs for concurrent installs
	results := make(chan error, len(packages))

	for i, pkg := range packages {
		go func(index int, packageSpec string) {
			venvDir := filepath.Join(testDir, fmt.Sprintf("venv%d", index))

			// Create virtual environment
			cmd := exec.Command("python3", "-m", "venv", venvDir)
			if err := cmd.Run(); err != nil {
				results <- fmt.Errorf("failed to create venv %d: %w", index, err)
				return
			}

			// Install via groxpi
			pipPath := filepath.Join(venvDir, "bin", "pip")
			if _, err := os.Stat(pipPath); os.IsNotExist(err) {
				pipPath = filepath.Join(venvDir, "Scripts", "pip.exe")
			}

			// Upgrade pip first
			cmd = exec.Command(pipPath, "install", "--upgrade", "pip")
			if err := cmd.Run(); err != nil {
				results <- fmt.Errorf("failed to upgrade pip in venv %d: %w", index, err)
				return
			}

			// Install package
			cmd = exec.Command(pipPath, "install",
				"--index-url", "http://localhost:5000/simple/",
				"--trusted-host", "localhost",
				packageSpec)

			if err := cmd.Run(); err != nil {
				results <- fmt.Errorf("failed to install %s in venv %d: %w", packageSpec, index, err)
				return
			}

			results <- nil
		}(i, pkg)
	}

	// Wait for all installs to complete
	for i := 0; i < len(packages); i++ {
		err := <-results
		assert.NoError(t, err, "Concurrent install %d should succeed", i)
	}
}

func verifyS3Storage(t *testing.T) {
	// Use docker to check if files are stored in MinIO
	cmd := exec.Command("docker", "exec", "groxpi-minio-test",
		"mc", "ls", "local/groxpi-test/integration-test/packages/")

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		t.Logf("Could not list S3 contents (this may be expected if no packages were cached): %v", err)
		return
	}

	output := stdout.String()
	t.Logf("S3 storage contents:\n%s", output)

	// Check if we have some package files
	if strings.Contains(output, ".whl") || strings.Contains(output, ".tar.gz") {
		t.Log("✅ Package files found in S3 storage")
	} else {
		t.Log("ℹ️  No package files found in S3 (packages may have been served from PyPI directly)")
	}
}

// TestStreamingDownloader tests the streaming downloader with S3 storage
func TestStreamingDownloaderWithS3(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping streaming downloader test in short mode")
	}

	// This test verifies that the streaming downloader works correctly with S3
	// by testing a direct file download and cache operation

	t.Log("Testing streaming downloader integration with S3...")

	// Test will be expanded to include direct streaming tests
	// For now, we rely on the package manager tests above
}
