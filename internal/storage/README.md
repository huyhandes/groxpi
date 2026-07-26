# Storage Package Testing

This package includes comprehensive tests for both local and S3 storage implementations.

## Running Tests

### Unit Tests Only (Fast)
```bash
go test -v -short ./internal/storage/
```
This runs only unit tests and skips integration tests that require external services.

### Full Test Suite (Including Integration Tests)
```bash
# Set required environment variables
export TEST_S3_ENDPOINT="your-s3-endpoint"
export TEST_S3_ACCESS_KEY="your-access-key"  
export TEST_S3_SECRET_KEY="your-secret-key"
export TEST_S3_BUCKET="your-test-bucket"

# Optional environment variables
export TEST_S3_REGION="us-east-1"           # Default: us-east-1
export TEST_S3_USE_SSL="true"               # Default: true
export TEST_S3_FORCE_PATH_STYLE="false"     # Default: false

# Run all tests including integration
go test -v ./internal/storage/
```

## Test Coverage
```bash
go test -cover ./internal/storage/
```

## Benchmarks
```bash
go test -bench=. ./internal/storage/
```

## Test Structure

### Unit Tests
- **Local Storage**: Tests the filesystem-based storage implementation
- **LRU Eviction** (`lru_test.go`): a frequently-read file survives eviction pressure; eviction goes through the `objectDeleter` seam; size accounting matches disk after overwrites
- **Tiering** (`tiered_test.go`): L1 back-fill lands and survives request cancellation; a real L1 error propagates instead of being masked as a miss; misses are the `ErrNotFound` sentinel
- **Worker Pool** (`workerpool_test.go`): every submitted job runs, concurrency never exceeds the worker count, `Submit` drops when full, `Close` is idempotent and waits for in-flight jobs
- **S3 Buffer Pools**: Tests buffer reuse for small-object writes
- **Singleflight Patterns**: Tests request deduplication logic
- **Configuration**: Tests various S3 configuration scenarios

### Integration Tests
- **S3 Basic Operations**: Put, Get, Delete, Exists, Stat operations with real S3
- **S3 Advanced Features**: Multipart uploads (handled internally by the SDK via part sizing) and presigned URLs
- **S3 Concurrency**: Concurrent operations and singleflight deduplication
- **S3 Error Handling**: Network failures, timeouts, invalid requests
- **S3 Edge Cases**: Empty files, large files, Unicode content, special characters

### Performance Tests
- **Buffer Pool Efficiency**: Memory allocation benchmarks
- **Singleflight Effectiveness**: Request deduplication measurements  
- **Real S3 Operations**: Network operation benchmarks
- **Concurrent Access**: Multi-goroutine performance testing

## Security Notes

- **No Hardcoded Credentials**: All S3 credentials are loaded from environment variables
- **Safe Defaults**: SSL enabled by default, secure configuration options
- **Test Isolation**: Each test uses unique keys to avoid conflicts
- **Cleanup**: All test objects are cleaned up after test completion

## Coverage Targets

- **Overall Storage Package**: 81.6% — *this figure predates the 2026-07 architecture refactor and is unverified. Measured at `6a11326` with `go test -short -coverprofile`, the package is **53.1%**; the gap is the S3 network paths, which only the skipped integration tests reach.*
- **Local Storage**: 90%+ coverage *(unverified since the refactor)*
- **S3 Implementation**: 80%+ coverage with real integration testing *(requires the `TEST_S3_*` environment above; skipped by default)*
- **Edge Cases**: Comprehensive boundary condition testing
- **Error Scenarios**: Full error path validation, including `errors.Is(err, ErrNotFound)` on every adapter