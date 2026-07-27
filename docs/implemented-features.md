# Implemented Features

Groxpi provides a complete, production-ready implementation of a high-performance PyPI caching proxy with extensive feature coverage.

## Core API Implementation ✅

### PyPI Simple API Compliance
- **PEP 503 Compliance**: Complete implementation of PyPI Simple Repository API
- **PEP 691 Compliance**: JSON API variant with content negotiation
- **Package Listing**: `/simple/` endpoint for package discovery
- **Package Details**: `/simple/{package}/` for file listings
- **File Downloads**: `/simple/{package}/{file}` with download/redirect logic
- **Content Negotiation**: Automatic JSON/HTML response based on Accept headers
- **Hash Verification**: SHA256 hash support for package integrity
- **Metadata Support**: Requires-Python and file size information
- **ETag Normalisation**: `quoteETag` is the single place an entity-tag is quoted, so a bare index hash and an already-quoted backend ETag both emit exactly one layer of quotes — an already-quoted value is no longer double-quoted into `""abc""`

### HTTP Server Features
- **Gin Framework**: High-performance HTTP server with radix tree routing
- **Response Compression**: Automatic gzip/deflate compression
- **Request Logging**: Structured request/response logging with timing
- **Error Recovery**: Automatic panic recovery with full stack traces
- **Graceful Shutdown**: Proper connection draining and resource cleanup
- **Health Checks**: Detailed health endpoint for monitoring
- **Method-Aware Routing**: `HandleMethodNotAllowed` is enabled, so a known path reached with an unregistered method returns `405` with an `Allow` header instead of a misleading `404`. Only `GET` is registered on the index and download routes, so `HEAD` on them is currently `405` too — registering `HEAD` is an open follow-up, not intended behaviour

## Advanced Caching System ✅

### Multi-Level Caching
- **Index Cache**: In-memory TTL-based cache for package listings and parsed file indexes
- **Response Cache**: In-memory LRU cache of pre-marshaled JSON responses
- **On-Disk Object Cache**: LRU-evicted package files on local disk, S3, or both (hybrid)
- **Thread-Safe Operations**: Concurrent cache access with minimal locking
- **Cache Invalidation**: Manual cache clearing via DELETE endpoints

*A third in-process tier (`cache.FileCache`) existed with no production callers and was deleted in `6a11326`.*

### Cache Strategies
- **TTL-Based Expiration**: Configurable time-to-live for index and response caches
- **No Negative Caching**: an empty upstream index or package list is never cached, so a transient upstream fault cannot poison a package (or the whole `/simple/` listing) for the full `GROXPI_INDEX_TTL`
- **LRU Eviction**: Least Recently Used eviction for the on-disk object cache. When a TTL is configured, expired entries are swept periodically regardless of cache size, and a size-driven pass still prefers expired victims over unexpired ones
- **Size-Based Limits**: Automatic eviction when cache size limits reached (`0` means unlimited)
- **Eviction Safety**: Every read path that yields a size records an access, so a file currently being served cannot look cold to the evictor
- **Standalone TTL Sweep**: the sweep ticks at half the TTL (capped at 1 minute) on the eviction goroutine and is disabled entirely when no TTL is set. Before `97282f1`, expiry only ran as a phase of size-driven eviction, so `GROXPI_LOCAL_CACHE_TTL` did nothing on an under-quota cache
- **No Stats API**: there is no programmatic cache-stats accessor. `LRULocalStorage.GetStats()` existed with no production caller and was deleted in `ba2e0d6`; size and eviction activity are visible only in the structured logs

## Storage Backend Support ✅

### Local Filesystem Storage
- **File-Based Caching**: Local directory-based package storage
- **LRU Eviction**: Automatic eviction of least recently used files when cache is full
- **Configurable Paths**: Customizable cache directory locations
- **Atomic Operations**: Safe file write operations with temporary files
- **Directory Management**: Automatic directory creation and cleanup
- **Cache Rebuild**: Automatic rebuild of LRU cache from existing files on startup

### S3-Compatible Storage
- **AWS S3 Support**: Native AWS S3 integration
- **MinIO Compatibility**: Works with MinIO and other S3-compatible stores
- **Custom Endpoints**: Support for private S3-compatible services
- **Path-Style URLs**: Configurable URL styles for different S3 implementations
- **SSL/TLS Support**: Secure connections with configurable SSL settings
- **Prefix Support**: Bucket prefixes for organized storage
- **Synchronous Uploads**: `Put` hands the reader straight to minio-go, which sizes multipart uploads itself. The "async write" queue, its buffer pools and the `GROXPI_S3_ASYNC_WRITES`/`_WORKERS`/`_QUEUE_SIZE` variables were deleted in `ba2e0d6` — `Put` blocked on the queued result anyway, so nothing was ever asynchronous to the caller
- **Separate Connection Pools**: distinct HTTP transports for read, write and metadata operations (`GROXPI_S3_READ_POOL_SIZE` / `_WRITE_POOL_SIZE` / `_META_POOL_SIZE`)

### Hybrid/Tiered Storage
- **Multi-Tier Caching**: Local L1 cache + S3 L2 storage (`GROXPI_STORAGE_TYPE=hybrid`)
- **Automatic L1 Population**: L2 hits asynchronously populate L1 for future requests, on jobs that outlive the request that queued them
- **Concurrent Writes**: New files written to both L1 and L2 simultaneously (`io.Pipe` tee under singleflight)
- **Local-Path L1 Serving**: L1 objects are real files, so they are served by path and `net/http` handles range and conditional requests (see the note below — this is not a kernel zero copy)
- **LRU L1 Eviction**: Size-based LRU plus an independent periodic TTL sweep (`GROXPI_LOCAL_CACHE_TTL`), deleting through the storage backend so on-disk state and size accounting have one owner
- **Background Sync Workers**: Configurable `WorkerPool[T]` for L1 cache population
- **Non-Blocking L1 Sync**: L1 population doesn't block user requests; a full queue drops the back-fill rather than stalling the reader
- **Typed Misses**: `storage.ErrNotFound` means L1 falls through to L2 only on a genuine miss — a real L1 failure is reported, not silently treated as a miss
- **S3 as Primary**: L2 (S3) is authoritative source, L1 is performance layer
- **Capability-Based Tiering**: the tier that genuinely has a capability provides it — `GetFilePath` from L1, the only tier holding real files

## High-Performance File Transfer ✅

### Memory Efficiency
- **Streaming, Never Buffering**: files are piped through, never loaded whole into memory
- **Metadata Before Body**: `Storage.Get` returns `*ObjectInfo` before the first body byte, so headers are emitted once and correctly
- **Buffer Pools**: 64KB copy buffers in the tee downloader and parse buffers in the upstream index client. Nothing is pooled on the response path or the S3 write path — the S3 backend's four buffer pools were deleted in `ba2e0d6` along with the async-write queue they served
- **GC Optimization**: reduced allocation pressure from pooling and streaming

### About "zero copy"

groxpi does **not** have a kernel zero-copy path. `storage.ZeroCopyCapable` means "this backend can name a real file on the local filesystem", which lets the transport delegate range requests, `If-Modified-Since` and content-type sniffing to `net/http`. It does not avoid a copy: gin's `responseWriter` implements neither `File()` nor `io.ReaderFrom`, so `net/http`'s sendfile path cannot engage, and the gzip middleware wraps the writer regardless. Earlier versions of this document claimed OS-level sendfile; that was never true in this codebase, and the `trySendfile` helper that implied it was removed in `6243e0a`.

### Streaming Pipeline
- **Tee Download-and-Cache**: one upstream read feeds both the client and the storage backend (`io.TeeReader` + `io.Pipe`)
- **SingleFlight Deduplication**: one request per file streams; concurrent requests wait and are then served from cache
- **Download Outlives Its Trigger**: the fetch populates the cache for every waiter, so it is not cancelled when the triggering client disconnects
- **Headers Before Body**: response headers are emitted lazily, immediately before the first body byte, so nothing is dropped
- **Connection Pooling**: HTTP client connection reuse

Speculative streaming machinery — `ZeroCopyServer`, `BroadcastWriter`, `HashingWriter` and a second non-tee downloader — had no production callers and was deleted in `6243e0a`.

## Multi-Index Support ✅

`GROXPI_EXTRA_INDEX_URLS` / `GROXPI_EXTRA_INDEX_TTLS` build `config.ResolutionOrder()` — extras first in configured order, primary last — which `PackageFileService.queryIndexes` consults. See [ADR 0001](adr/0001-extras-first-index-resolution.md).

### Index Configuration
- **Main Index**: primary index, consulted last ✅
- **Extra Indices**: queried before the primary, in configured order ✅
- **Individual TTLs**: the answering index's TTL is the cache entry's TTL ✅
- **Never Merged**: the first index with the package supplies its whole file list; no union, no dedup, so dependency confusion is impossible by construction ✅
- **Fallthrough**: not-found only after every index has missed; a failing index fails the request rather than falling through ✅
- **Concurrency**: extras are queried in parallel with strictly ordered selection; the primary is never queried speculatively (it would leak internal package names) ✅
- **Credential Redaction**: user-info in an index URL never reaches a log field, an error or the `/health` payload ✅
- **Health Monitoring**: not implemented 🔄

### Timeouts
- **Connect / read / download timeouts**: configurable and applied to the single index ✅
- **Dynamic download timeout**: scaled from the advertised file size (100 KB/s floor, 2min minimum, 60min cap) ✅

## JSON Processing ✅

### Sonic JSON Integration
- **High Performance**: 3x faster than standard library JSON
- **SIMD Instructions**: Hardware-accelerated JSON processing
- **Memory Efficient**: Reduced allocations during marshal/unmarshal
- **Error Handling**: Comprehensive JSON parsing error handling

### Data Structures
- **Package Metadata**: Structured package information
- **File Information**: Detailed file metadata with hashes
- **API Responses**: Standardized response formats
- **Configuration**: JSON-based configuration support

## Security Features ✅

### SSL/TLS Support
- **HTTPS Support**: Secure connections to upstream indices
- **Certificate Validation**: Configurable SSL certificate verification
- **Custom CA Support**: Support for custom certificate authorities
- **TLS Configuration**: Configurable TLS settings

### Input Validation
- **Package Name Validation**: Proper package name normalization
- **URL Validation**: Safe URL handling and validation
- **Request Sanitization**: Input sanitization for security
- **Error Boundaries**: Controlled error handling

## Monitoring & Observability ✅

### Structured Logging
- **JSON Logging**: Machine-readable structured logs
- **Log Levels**: Configurable logging levels (DEBUG, INFO, WARN, ERROR)
- **Request Tracing**: Request ID tracking across operations
- **Performance Metrics**: Request timing and performance data

### Health Monitoring
- **Health Endpoint**: Comprehensive system health information
- **System Metrics**: Memory usage, cache statistics, uptime
- **Index Status**: Upstream index health monitoring
- **Error Tracking**: Error rate and failure mode tracking

### Container Support
- **Docker Health Checks**: Built-in container health validation
- **Resource Monitoring**: Memory and CPU usage tracking
- **Startup Probes**: Container startup validation
- **Shutdown Signals**: Graceful container shutdown handling

## Configuration Management ✅

### Environment Variables
- **Full Compatibility**: Complete proxpi environment variable support
- **Type Safety**: Proper type conversion and validation
- **Default Values**: Sensible defaults for all configuration options
- **Documentation**: Comprehensive configuration documentation

### Runtime Configuration
- **Hot Reloading**: Some configuration changes without restart
- **Validation**: Configuration validation on startup
- **Error Reporting**: Clear error messages for misconfigurations
- **Override Support**: Environment variable precedence handling

## Error Handling ✅

### Comprehensive Error Management
- **Upstream Errors**: Proper handling of index failures
- **Network Errors**: Timeout and connection error handling
- **Storage Errors**: File system and S3 error handling
- **Client Errors**: Proper HTTP error response codes

### Recovery Mechanisms
- **Redirect Fallback**: an upstream stream that fails before the first body byte falls back to a 302 to PyPI; once bytes are on the wire the request is aborted rather than corrupted ✅
- **Typed Not-Found**: `storage.ErrNotFound` keeps a genuine miss distinguishable from a backend failure, so a broken disk is reported instead of silently degrading into an L2 round trip ✅
- **Non-Fatal L1 Failures**: in hybrid mode an L1 write or delete failure is logged; L2 (authoritative) decides the outcome ✅
- **No Poisoned Index on a Transient Fault**: an empty upstream result is returned but not cached, so the next request retries instead of serving an empty index for the whole TTL ✅
- **Stringly-Typed Upstream Miss**: `handleListFiles` still distinguishes "package not found" from a real failure by matching `"not found"` in the error text — `internal/pypi` exposes no sentinel. A `pypi.ErrPackageNotFound` is an open follow-up 🔄
- **Retry Logic / Circuit Breaker**: not implemented 🔄 — no retry, backoff or breaker code exists as of `ba2e0d6`

## Client Compatibility ✅

### Package Manager Support
- **pip**: Full compatibility with all pip versions
- **poetry**: Complete poetry integration support
- **pipenv**: Full pipenv compatibility
- **uv**: High-performance uv package manager support
- **PDM**: Python Dependency Manager compatibility
- **conda/mamba**: pip fallback support

### HTTP Client Features
- **User Agent Detection**: Client identification and logging
- **Accept Header Handling**: Proper content negotiation
- **Range Requests**: supported for files served by path from a local backend (`c.File` → `net/http`); not supported on the S3 open-then-stream fallback
- **Keep-Alive**: Connection reuse for performance

## HTML Interface ✅ (inline, not templated)

Handlers generate HTML inline with a `strings.Builder`; `server.New` deliberately does not load templates. The `templates/` directory is retained but unused.

### What the HTML interface provides
- **Home page**: index URL, cache size, index TTL, version
- **Package file listings**: PEP 503-compatible anchor list with `data-requires-python` and `data-yanked`, URLs rewritten to point at the proxy
- **Go Templates / layouts / partials**: not wired up 🔄

## Development Features ✅

### Testing Infrastructure
- **Unit Tests**: Comprehensive unit test coverage
- **Integration Tests**: Full API integration testing
- **Benchmark Tests**: Performance regression testing
- **Load Tests**: Concurrent request handling validation

### Development Tools
- **Hot Reload**: Development server with auto-restart
- **Debug Mode**: Enhanced logging for development
- **Profile Support**: CPU and memory profiling
- **Metrics Export**: Development metrics and statistics

## Deployment Features ✅

### Container Support
- **Multi-Stage Builds**: Optimized Docker images
- **Multi-Architecture**: ARM64 and AMD64 support
- **Health Checks**: Container orchestration integration
- **Security**: Non-root container execution

### Production Features
- **Graceful Shutdown**: Zero-downtime deployments
- **Resource Limits**: Configurable resource constraints
- **Monitoring Integration**: Prometheus metrics (planned)
- **Load Balancing**: Stateless design for horizontal scaling

## Performance Optimizations ✅

### Memory Management
- **Buffer Pools**: reused buffers on the download and index-parse paths only (see the note under Memory Efficiency)
- **Package Name Normalization**: lowercase + `_`→`-` so equivalent spellings collapse to one cache key (key normalization, not string interning)
- **GC Tuning**: Optimized garbage collection settings
- **Memory Profiling**: Built-in memory usage monitoring

### CPU Optimizations
- **SIMD Instructions**: Hardware-accelerated operations
- **Goroutine Pools**: Bounded concurrency management
- **Lock-Free Operations**: Concurrent data structure access
- **CPU Profiling**: Performance bottleneck identification

### I/O Optimizations
- **Serve-by-Path**: locally cached files handed to `net/http`, which brings range and conditional-request handling (not a kernel zero copy — see above)
- **Connection Pooling**: HTTP client connection reuse
- **Compression**: Response compression for bandwidth savings
- **Streaming**: no whole-file buffering on any path

## Comprehensive Benchmarking Suite ✅

### Performance Testing Framework
- **Master Orchestrator**: Single script to run complete benchmark suites
- **WRK Integration**: Professional HTTP load testing with detailed metrics
- **UV Package Testing**: Real-world package installation performance
- **Resource Monitoring**: Docker container CPU, memory, I/O tracking
- **DuckDB Analysis**: Advanced SQL-based results analysis

### Benchmark Types
- **API Load Testing**: High-concurrency HTTP performance measurement
- **Package Installation**: UV-based real package installation timing
- **Cache Performance**: Cold vs warm cache scenario testing
- **Resource Usage**: Container efficiency and resource consumption
- **Comparative Analysis**: Side-by-side groxpi vs proxpi benchmarks

### Results and Analysis
- **Timestamped Results**: All results saved with consistent timestamps
- **CSV Export**: DuckDB-compatible CSV files for analysis
- **Markdown Reports**: Human-readable consolidated reports
- **Performance Metrics**: RPS, latency percentiles, resource usage
- **Historical Tracking**: Results saved for performance regression testing

### Performance Results as of December 2024 — not re-measured since

> ⚠️ These figures were recorded in commit `ca9c979` (2025-12-29, labelled "December 2024") and have **not** been re-measured after the architecture refactor in `d20f16c..ba2e0d6` (2026-07-26). Treat them as unverified against current code; re-run `./benchmarks/benchmark.sh` before quoting them.

- **12.8x Higher Throughput**: 52,880 vs 4,139 requests/sec for package index
- **27x Better Latency**: 0.85ms vs 23.04ms P50 response times
- **High Load Stability**: Groxpi maintains stable responses while proxpi fails under high concurrency
- **Production Validated**: Tested with popular packages (numpy, pandas, polars, pyspark, fastapi)

## Standards Compliance ✅

### HTTP Standards
- **HTTP/1.1**: Full HTTP/1.1 specification compliance
- **Content Encoding**: Proper compression header handling
- **Cache Control**: HTTP caching header support

### PyPI Standards
- **PEP 503**: Simple Repository API compliance
- **PEP 691**: JSON-based Simple API variant
- **Package Naming**: Proper package name normalization
- **Version Handling**: Semantic version parsing and comparison

## Future Enhancements 🔄

### Planned Features
- **Prometheus Metrics**: Application-level metrics export
- **Distributed Tracing**: OpenTelemetry integration
- **Advanced Templates**: Enhanced web interface
- **CI/CD Pipeline**: Automated testing and releases

### Under Consideration
- **Cache Warming**: Proactive cache population
- **Load Balancing**: Multi-instance deployment patterns
- **Rate Limiting**: Request rate limiting capabilities
- **Authentication**: Optional authentication mechanisms

---

**Status**: Core features are production-ready and covered by unit, integration and benchmark tests (`go test ./...` green at `ba2e0d6`: 378 tests across 8 packages). The system provides API compatibility with the original proxpi. Performance figures quoted above predate the 2026-07 architecture refactor and have not been re-measured; items marked 🔄 are configured but not implemented.