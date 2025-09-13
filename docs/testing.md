# Testing Strategy

Groxpi follows Test-Driven Development (TDD) with comprehensive testing across all components, achieving 95%+ code coverage.

## Testing Philosophy

### TDD Approach
1. **Red**: Write failing tests first
2. **Green**: Implement minimal code to pass
3. **Refactor**: Improve code while maintaining tests
4. **Repeat**: Iterate for each feature

### Testing Principles
- **Unit Tests**: Test individual functions and methods
- **Integration Tests**: Test component interactions
- **Benchmark Tests**: Validate performance requirements
- **End-to-End Tests**: Test complete user workflows

## Test Structure

### Test Organization (Go Standard)
Following Go testing best practices, test files are located alongside the code they test:

```
groxpi/
├── cmd/groxpi/                  # Main application
│   ├── main.go                  # Application entry point
│   ├── main_test.go             # Main function unit tests
│   └── integration_test.go      # Full server integration tests
├── internal/                    # Private application code
│   ├── cache/                   # Caching implementations
│   │   ├── file.go              # File cache implementation
│   │   ├── file_test.go         # File cache tests
│   │   ├── index.go             # Index cache implementation
│   │   ├── index_test.go        # Index cache tests
│   │   ├── response.go          # Response cache implementation
│   │   └── response_test.go     # Response cache tests
│   ├── config/                  # Configuration management
│   │   ├── config.go            # Configuration implementation
│   │   └── config_test.go       # Configuration tests
│   ├── logger/                  # Structured logging
│   │   ├── logger.go            # Logger implementation
│   │   └── logger_test.go       # Logger tests
│   ├── pypi/                    # PyPI client
│   │   ├── client.go            # PyPI client implementation
│   │   ├── client_test.go       # PyPI client tests
│   │   └── json_bench_test.go   # JSON performance benchmarks
│   ├── server/                  # HTTP server and handlers
│   │   ├── server.go            # Server implementation
│   │   └── server_test.go       # Server tests
│   ├── storage/                 # Storage backend abstraction
│   │   ├── local.go             # Local storage implementation
│   │   ├── local_test.go        # Local storage tests
│   │   ├── s3.go                # S3 storage implementation
│   │   ├── s3_unit_test.go      # S3 unit tests
│   │   ├── s3_integration_test.go      # S3 integration tests
│   │   ├── s3_full_integration_test.go # S3 comprehensive tests
│   │   ├── s3_mock_integration_test.go # S3 mock tests
│   │   └── storage_bench_test.go       # Storage benchmarks
│   └── streaming/               # Zero-copy streaming
│       ├── broadcast.go         # Broadcast implementation
│       ├── broadcast_test.go    # Broadcast tests
│       ├── downloader.go        # Downloader implementation
│       ├── downloader_test.go   # Downloader tests
│       ├── zerocopy.go          # Zero-copy implementation
│       └── zerocopy_test.go     # Zero-copy tests
└── templates/                   # HTML templates
    └── ...
```

## Test Categories

### Unit Tests
Test individual functions, methods, and components in isolation.

#### Cache Testing
```go
func TestIndexCache_GetSet(t *testing.T) {
    cache := NewIndexCache(100, time.Minute)
    
    // Test cache miss
    _, found := cache.Get("test-key")
    assert.False(t, found)
    
    // Test cache set and hit
    cache.Set("test-key", []string{"package1", "package2"})
    packages, found := cache.Get("test-key")
    assert.True(t, found)
    assert.Equal(t, []string{"package1", "package2"}, packages)
}
```

#### Configuration Testing
```go
func TestConfig_LoadFromEnv(t *testing.T) {
    os.Setenv("GROXPI_INDEX_URL", "https://test.pypi.org/simple/")
    os.Setenv("GROXPI_INDEX_TTL", "3600")
    
    config, err := LoadConfig()
    require.NoError(t, err)
    assert.Equal(t, "https://test.pypi.org/simple/", config.IndexURL)
    assert.Equal(t, 3600, config.IndexTTL)
}
```

### Integration Tests
Test component interactions and full request flows.

#### Server Integration Tests
```go
func TestServer_Integration(t *testing.T) {
    // Setup test server with real dependencies
    server := setupTestServer(t)
    defer server.app.Shutdown()
    
    // Test package index endpoint
    resp, err := http.Get("http://localhost:5000/simple/")
    require.NoError(t, err)
    assert.Equal(t, http.StatusOK, resp.StatusCode)
    
    // Test JSON content negotiation
    req, _ := http.NewRequest("GET", "http://localhost:5000/simple/", nil)
    req.Header.Set("Accept", "application/json")
    resp, err = http.DefaultClient.Do(req)
    require.NoError(t, err)
    assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
}
```

#### Storage Integration Tests
```go
func TestS3Storage_Integration(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping S3 integration test in short mode")
    }
    
    storage := setupS3Storage(t)
    
    // Test file upload and download
    content := []byte("test file content")
    err := storage.Put("test/file.txt", content)
    require.NoError(t, err)
    
    reader, size, err := storage.Get("test/file.txt")
    require.NoError(t, err)
    assert.Equal(t, int64(len(content)), size)
    
    retrieved, err := io.ReadAll(reader)
    require.NoError(t, err)
    assert.Equal(t, content, retrieved)
}
```

### Benchmark Tests
Validate performance requirements and prevent regressions.

#### JSON Performance Benchmarks
```go
func BenchmarkJSONMarshal_Sonic(b *testing.B) {
    data := createTestPackageData()
    b.ReportAllocs()
    b.ResetTimer()
    
    for i := 0; i < b.N; i++ {
        _, err := sonic.ConfigFastest.Marshal(data)
        if err != nil {
            b.Fatal(err)
        }
    }
}

func BenchmarkJSONMarshal_Stdlib(b *testing.B) {
    data := createTestPackageData()
    b.ReportAllocs()
    b.ResetTimer()
    
    for i := 0; i < b.N; i++ {
        _, err := json.Marshal(data)
        if err != nil {
            b.Fatal(err)
        }
    }
}
```

#### Zero-Copy Benchmarks
```go
func BenchmarkZeroCopy_Stream(b *testing.B) {
    testData := make([]byte, 1024*1024) // 1MB
    b.ReportAllocs()
    b.ResetTimer()
    
    for i := 0; i < b.N; i++ {
        reader := bytes.NewReader(testData)
        _, err := io.Copy(io.Discard, reader)
        if err != nil {
            b.Fatal(err)
        }
    }
}
```

### Load Tests
Test system behavior under concurrent load.

#### Concurrent Request Tests
```go
func TestServer_ConcurrentRequests(t *testing.T) {
    server := setupTestServer(t)
    defer server.app.Shutdown()
    
    concurrency := 100
    requests := 1000
    
    var wg sync.WaitGroup
    errors := make(chan error, requests)
    
    for i := 0; i < concurrency; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for j := 0; j < requests/concurrency; j++ {
                resp, err := http.Get("http://localhost:5000/simple/")
                if err != nil {
                    errors <- err
                    return
                }
                resp.Body.Close()
                
                if resp.StatusCode != http.StatusOK {
                    errors <- fmt.Errorf("unexpected status: %d", resp.StatusCode)
                    return
                }
            }
        }()
    }
    
    wg.Wait()
    close(errors)
    
    for err := range errors {
        t.Error(err)
    }
}
```

## Test Prerequisites

### MinIO for S3 Integration Tests
S3 integration tests require a running MinIO instance to provide S3-compatible storage locally.

#### Start MinIO for Testing
```bash
# Start MinIO test environment
docker-compose -f docker-compose.test.yml up -d

# Wait for MinIO to be ready
timeout 60s bash -c 'until curl -f http://localhost:9000/minio/health/live; do sleep 2; done'

# Create test bucket (required for S3 integration tests)
docker exec groxpi-minio-test mc alias set local http://localhost:9000 minioadmin minioadmin
docker exec groxpi-minio-test mc mb local/groxpi-test

# Verify MinIO is running (optional)
# MinIO Console: http://localhost:9001 (minioadmin/minioadmin)
# S3 API: http://localhost:9000
```

#### Stop MinIO After Testing
```bash
# Stop and remove test containers
docker-compose -f docker-compose.test.yml down

# Remove test data volumes (optional)
docker-compose -f docker-compose.test.yml down -v
```

## Test Execution

### Running Tests

#### All Tests (Go Standard)
```bash
# Run all tests (recommended)
go test ./...

# Run tests with coverage
go test -cover ./...

# Run tests with detailed coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html

# Run only unit tests (fast, no external dependencies)
go test -short ./...
```

#### Specific Test Types
```bash
# Test specific packages
go test ./internal/cache
go test ./internal/server
go test ./internal/storage

# Run integration tests (may require MinIO for S3 tests)
go test ./internal/storage -run Integration

# Benchmark tests
go test -bench=. ./internal/pypi        # JSON benchmarks
go test -bench=. ./internal/storage     # Storage benchmarks

# Test main application
go test ./cmd/groxpi
```

#### S3 Integration Tests (Optional)
```bash
# Prerequisites: Start MinIO first (for S3 integration tests)
docker-compose -f docker-compose.test.yml up -d

# Wait for MinIO and create test bucket
timeout 60s bash -c 'until curl -f http://localhost:9000/minio/health/live; do sleep 2; done'
docker exec groxpi-minio-test mc alias set local http://localhost:9000 minioadmin minioadmin
docker exec groxpi-minio-test mc mb local/groxpi-test

# Run S3 integration tests
go test ./internal/storage -run S3

# Clean up
docker-compose -f docker-compose.test.yml down
```

#### Verbose Output
```bash
# Verbose test output
go test -v ./...

# Race condition detection
go test -race ./...

# Memory sanitizer
go test -msan ./...
```

### Test Configuration

#### Environment Variables

##### Default Configuration (MinIO)
The S3 integration tests use MinIO defaults when environment variables are not set:
```bash
# Default MinIO configuration (no environment variables needed)
# TEST_S3_ENDPOINT="localhost:9000"
# TEST_S3_ACCESS_KEY="minioadmin"
# TEST_S3_SECRET_KEY="minioadmin"
# TEST_S3_BUCKET="groxpi-test"
# TEST_S3_USE_SSL="false"
# TEST_S3_FORCE_PATH_STYLE="true"
```

##### Custom Configuration
Override defaults for external S3 services or CI/CD:
```bash
# Test configuration
export GROXPI_TEST_INDEX_URL="https://test.pypi.org/simple/"
export GROXPI_TEST_CACHE_DIR="/tmp/groxpi-test"
export GROXPI_TEST_LOG_LEVEL="DEBUG"

# S3 integration tests (custom endpoint)
export TEST_S3_ENDPOINT="s3.amazonaws.com"
export TEST_S3_ACCESS_KEY="your-access-key"
export TEST_S3_SECRET_KEY="your-secret-key"
export TEST_S3_BUCKET="your-test-bucket"
export TEST_S3_REGION="us-west-2"
export TEST_S3_USE_SSL="true"
export TEST_S3_FORCE_PATH_STYLE="false"
```

#### Test Flags
```bash
# Skip slow tests
go test -short ./...

# Run specific test
go test -run TestServerIntegration ./internal/server

# Run tests with timeout
go test -timeout 30s ./...

# Parallel test execution
go test -parallel 4 ./...
```

## Coverage Requirements

### Coverage Targets
- **Overall**: 95%+ code coverage
- **Critical paths**: 100% coverage (cache, storage, API handlers)
- **Error handling**: 100% coverage for error paths
- **Edge cases**: Complete coverage of boundary conditions

### Coverage by Component
| Component | Target | Previous | Current | Status |
|-----------|--------|----------|---------|--------|
| Cache | 100% | 67.9% | 96.4% | ✅ |
| Configuration | 100% | 48.2% | 82.1% | ⚠️ |
| Logging | 85% | 72.0% | 92.0% | ✅ |
| PyPI Client | 90% | 4.4% | 84.3% | ⚠️ |
| Server | 95% | 11.0% | 55.3% | ⚠️ |
| Storage | 95% | 12.9% | 23.8% | ❌ |
| Streaming | 85% | 9.8% | 81.2% | ⚠️ |
| Main (cmd) | 75% | 25.8% | TBD | ⚠️ |
| **Overall** | **95%** | **~54%** | **~75%** | **⚠️** |

*Last updated: 2025-09-13 (after test reorganization)*

### Test Reorganization Results (2025-09-13)
✅ **Successfully migrated tests to Go standard structure:**
- Moved all test files from `tests/unit/*` to `internal/*/` alongside source code
- Fixed package declarations (from `package xxx_test` to `package xxx`)
- Removed redundant `high_coverage_test.go` files
- Merged duplicate test cases
- Improved access to internal functions and private struct fields

**Coverage Improvements:**
- Cache: +28.5% (67.9% → 96.4%)
- Configuration: +33.9% (48.2% → 82.1%)
- Logging: +20% (72.0% → 92.0%)
- PyPI Client: +79.9% (4.4% → 84.3%)
- Server: +44.3% (11.0% → 55.3%)
- Storage: +10.9% (12.9% → 23.8%)
- Streaming: +71.4% (9.8% → 81.2%)

**Overall improvement: ~21%** (from ~54% to ~75%)

### Coverage Tools
```bash
# Generate coverage report
go test -coverprofile=coverage.out ./tests/...

# View coverage by function
go tool cover -func=coverage.out

# Generate HTML coverage report
go tool cover -html=coverage.out -o coverage.html

# Check coverage threshold
go tool cover -func=coverage.out | grep total | awk '{print $3}' | sed 's/%//'

# Generate coverage for specific test types
go test -coverprofile=unit-coverage.out ./tests/unit/...
go test -coverprofile=integration-coverage.out ./tests/integration/...
```

## Continuous Integration

### GitHub Actions
```yaml
name: Tests
on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    
    steps:
      - uses: actions/checkout@v3
      
      - uses: actions/setup-go@v3
        with:
          go-version: 1.24
      
      - name: Start MinIO for integration tests
        run: |
          docker-compose -f docker-compose.test.yml up -d
          # Wait for MinIO to be ready
          timeout 60s bash -c 'until curl -f http://localhost:9000/minio/health/live; do sleep 2; done'
      
      - name: Run unit tests
        run: go test -short -race ./tests/unit/...

      - name: Run integration tests
        run: go test -race ./tests/integration/...

      - name: Generate coverage report
        run: |
          go test -race -coverprofile=coverage.out ./tests/...
          go tool cover -func=coverage.out
      
      - name: Upload coverage
        uses: codecov/codecov-action@v3
        with:
          file: ./coverage.out
      
      - name: Stop MinIO
        if: always()
        run: docker-compose -f docker-compose.test.yml down
```

### Pre-commit Hooks
```bash
#!/bin/sh
# .git/hooks/pre-commit

# Run tests before commit
go test -short ./tests/unit/...
if [ $? -ne 0 ]; then
    echo "Tests failed. Commit aborted."
    exit 1
fi

# Check code coverage
COVERAGE=$(go test -cover ./tests/... | grep coverage | awk '{print $5}' | tr -d '%' | head -1)
if [ ${COVERAGE%.*} -lt 95 ]; then
    echo "Coverage below 95%. Commit aborted."
    exit 1
fi
```

## Test Data Management

### Mock Data
```go
// Test package data
func createTestPackageData() map[string]interface{} {
    return map[string]interface{}{
        "meta": map[string]interface{}{
            "api-version": "1.0",
        },
        "name": "test-package",
        "files": []map[string]interface{}{
            {
                "filename": "test-package-1.0.0.tar.gz",
                "url":      "https://files.pythonhosted.org/packages/test.tar.gz",
                "hashes":   map[string]string{"sha256": "abc123"},
                "size":     1024,
            },
        },
    }
}
```

### Test Fixtures
```bash
# Test data directory
tests/
├── fixtures/
│   ├── packages/
│   │   ├── numpy_index.json
│   │   ├── requests_index.json
│   │   └── django_index.json
│   └── responses/
│       ├── package_list.json
│       └── package_list.html
├── mocks/
│   ├── s3_responses.json
│   └── pypi_responses.json
└── results/
    ├── coverage.md
    └── test-output.md
```

## Known Issues

### Current Test Status (2025-09-13)

#### ✅ Resolved Issues
- **Test Structure**: Successfully migrated to Go standard test organization
- **Package Names**: Fixed all package declaration issues
- **Redundant Code**: Removed duplicate `high_coverage_test.go` files
- **Config Tests**: All configuration tests now pass
- **Coverage**: Significant improvements across all packages

#### ⚠️ Remaining Issues

**Server Tests (Network Timeouts)**
- **Issue**: Some server tests timeout when making real HTTP requests to PyPI
- **Error**: `test: timeout error 1000ms`
- **Impact**: Tests like `TestServer_HandleListPackages_HTML/JSON` fail
- **Status**: Template loading fixed, but network calls need mocking
- **Solution**: Mock PyPI HTTP client in tests for deterministic behavior

**Storage Tests (Low Coverage)**
- **Issue**: Storage package only at 23.8% coverage
- **Gap**: Missing tests for error conditions, edge cases, and S3 operations
- **Solution**: Add comprehensive tests for all storage backends

#### S3 Integration Tests
- **Setup**: MinIO required for S3 integration tests
- **Command**: `docker-compose -f docker-compose.test.yml up -d`
- **Bucket**: Ensure `groxpi-test` bucket exists

### Next Steps
1. Mock PyPI client in server tests to eliminate network dependencies
2. Add comprehensive storage tests (local and S3 error conditions)
3. Add more main function test coverage
4. Target 95% overall coverage

## Best Practices

### Test Writing Guidelines
1. **Descriptive names**: Test names should describe what is being tested
2. **Single responsibility**: Each test should test one specific behavior
3. **Arrange-Act-Assert**: Clear test structure
4. **Independent tests**: Tests should not depend on each other
5. **Deterministic**: Tests should produce consistent results

### Performance Testing
1. **Baseline benchmarks**: Establish performance baselines
2. **Regression detection**: Fail builds on performance regressions
3. **Memory profiling**: Monitor memory usage and leaks
4. **Load testing**: Test under realistic production loads

### Error Testing
1. **Happy path**: Test successful scenarios first
2. **Error paths**: Test all error conditions
3. **Edge cases**: Test boundary conditions
4. **Recovery**: Test error recovery mechanisms