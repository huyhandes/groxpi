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
- **S3 Buffer Pools**: Tests zero-copy optimizations and buffer reuse
- **Singleflight Patterns**: Tests request deduplication logic
- **Configuration**: Tests various S3 configuration scenarios

### Integration Tests
- **S3 Basic Operations**: Put, Get, Delete, Exists, Stat operations with real S3
- **S3 Advanced Features**: Multipart uploads, presigned URLs, range requests
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

- **Overall Storage Package**: 81.6% achieved
- **Local Storage**: 90%+ coverage
- **S3 Implementation**: 80%+ coverage with real integration testing
- **Edge Cases**: Comprehensive boundary condition testing
- **Error Scenarios**: Full error path validation