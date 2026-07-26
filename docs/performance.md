# Performance

Groxpi delivers strong performance through its Go-native architecture, buffer pooling, streaming file transfer and multi-level caching.

> ⚠️ **All figures on this page are unverified against the current code.** They were recorded in commit `ca9c979` (2025-12-29, labelled "December 2024") and have **not** been re-measured since the architecture refactor landed in `d20f16c..ba2e0d6` (2026-07-26), which reworked the download path, the storage interface, the L1 eviction logic and the S3 write path. Re-run `./benchmarks/benchmark.sh` before quoting any number below.

## Benchmark Results

### API Performance (vs Python proxpi)

**Benchmark results as recorded December 2024 — not re-measured since; see the warning above**

| Metric | Groxpi | Proxpi | Improvement |
|--------|--------|---------|-------------|
| **Package Index RPS (Warm)** | 52,880 | 4,139* | **12.8x faster** |
| **Package Files RPS (Warm)** | 4,204 | 4,083* | Comparable |
| **P50 Latency (Index)** | 0.85ms | 23.04ms | **27x faster** |
| **P50 Latency (Files)** | 14.55ms | 23.05ms | **1.6x faster** |
| **P99 Latency (Index)** | 326ms | 31ms | Higher under load |
| **Startup Time** | <2s | ~10s | **5x faster** |

*Note: Proxpi returned non-2xx responses for all requests under high load, indicating it cannot handle the same concurrency level as groxpi.

### Detailed Performance Metrics

#### Response Times (Cached Requests)
- **Index endpoints**: 6-542μs (sub-millisecond)
- **Package files**: 15-89μs for metadata
- **File downloads**: streamed, never buffered whole in memory
- **Health checks**: <1ms

#### Memory Efficiency
- **Allocations**: 4 vs 789 (typical request)
- **GC pressure**: reduced by buffer pooling and streaming (no whole-file buffering)
- **Buffer pools**: two remain — the upstream index client (`pypi/client.go`) and the 64KB copy buffers in the tee downloader. The S3 backend has none; its four pools were deleted in `ba2e0d6`
- **Peak memory**: ~50MB under load

#### Concurrent Performance
- **Max connections**: 1000+ simultaneous
- **Download concurrency**: SingleFlight deduplication
- **Cache contention**: Lock-free read operations
- **Streaming**: Parallel file serving

## Performance Optimizations

### Serve-by-path for locally cached files

When the configured backend can name a real file on disk, the path is handed to `net/http` instead of being opened and copied by handler code:

```go
// internal/server/server.go
func (s *Server) serveFromStorage(c *gin.Context, storageKey string) error {
    ctx := c.Request.Context()

    if zeroCopy, ok := s.storage.(storage.ZeroCopyCapable); ok {
        if filePath, err := zeroCopy.GetFilePath(ctx, storageKey); err == nil {
            c.File(filePath) // net/http handles range + If-Modified-Since
            return nil
        }
    }

    // open-then-stream fallback: metadata before the first body byte
    reader, info, err := s.storage.Get(ctx, storageKey)
    // ... headers from info, then io.Copy(c.Writer, reader)
}
```

The capability check and the fallback used to be two methods (`serveFromStorageOptimized` wrapping `serveFromStorage`); they were merged into one in `1fde004`, since the wrapper never did anything the callee could not.

**This is not a kernel-level zero copy.** Gin's `responseWriter` implements neither `File()` nor `io.ReaderFrom`, so `net/http`'s sendfile fast path cannot engage, and the gzip middleware wraps the writer regardless — the bytes still travel through user space. The benefit is delegated correctness (range requests, conditional requests, content-type sniffing handled by the standard library), not a saved copy. Earlier revisions of this page and a `trySendfile` helper in `storage/local.go` claimed OS-level zero copy; both were wrong and have been removed.

The fallback path is also cheap in the way that matters: `Storage.Get` returns `(io.ReadCloser, *ObjectInfo, error)` with metadata complete *before* the first body byte, so headers are emitted once and never re-derived mid-stream.

### Buffer Pool Management

Pooling survives in exactly two places, both on read paths:

```go
// internal/streaming/downloader.go — 64KB copy buffers, reused across downloads
copyBufPool: &sync.Pool{
    New: func() any {
        buf := make([]byte, 64*1024)
        return &buf
    },
},
```

plus `bufferPool` / `copyBufferPool` in `internal/pypi/client.go` for parsing upstream index responses.

There is **no response buffer pool** and **no pooling on the S3 write path**. The four S3 buffer pools were deleted in `ba2e0d6` along with the async-write queue they fed; uploads now hand the reader straight to minio-go, which does its own buffering.

### SingleFlight Pattern
- Deduplicates concurrent requests for the same index or package file
- Prevents cache stampede scenarios
- The sole dedup mechanism on the download path (the hand-rolled `downloadCoordinator` was removed in `5cb9f18`)
- Upstream-load reduction is workload-dependent and has not been measured in this repo

### Streaming Pipeline
- **Tee download-and-cache**: `streaming.StreamingDownloader.DownloadAndStream` writes to the client and into storage from one upstream read, via `io.TeeReader` + `io.Pipe`
- **Pooled copy buffers**: 64KB buffers reused across downloads
- **Deduplicated**: concurrent requests for the same file wait on the leader, then serve from cache

### Bounded worker pools
`storage.WorkerPool[T]` has one instance left: the tiered L1 back-fill (`WorkerPool[string]`, payload = storage key). Worker count is the concurrency cap; `Submit` never blocks — a full queue drops the job rather than stalling the reader. The S3 write queue was the other instance until `ba2e0d6`: `S3Storage.Put` submitted to it and then immediately blocked on the result channel, so it bought latency and no asynchrony. S3 uploads are now direct and synchronous.

## Technology Stack Performance

### Go Runtime Benefits
- **Goroutines**: Lightweight concurrency (2KB stack)
- **GC**: Low-latency garbage collection
- **Compilation**: Native machine code execution

### Gin Framework (vs net/http)
- **High-performance** HTTP processing with radix tree routing
- **Express-like** routing with minimal overhead
- **Built-in middleware** for compression, logging, recovery

### Sonic JSON (vs encoding/json)
- **3x faster** marshaling/unmarshaling
- **SIMD instructions** for JSON processing
- **Memory efficient** with reduced allocations

### phuslu/log (vs standard log)
- **Zero allocation** structured logging
- **High throughput** logging with minimal latency
- **JSON structured** output for observability

## Caching Performance

### Index Cache (In-Memory)
- **Hit ratio**: >95% for production workloads
- **TTL-based**: Configurable expiration (default: 30min)
- **Thread-safe**: Concurrent read operations
- **Memory usage**: ~1-5MB for 50k packages

### On-Disk Object Cache (LRU)
- **Hit ratio**: >85% for repeated downloads *(unverified — no measurement in this repo)*
- **Size-based**: Configurable limits (default: 5GB)
- **Eviction**: Least Recently Used. When a TTL is configured, a periodic sweep expires stale entries regardless of how full the cache is, and a size-driven pass still prefers expired victims over unexpired ones. Before `97282f1` expiry only ran while over quota, so a TTL had no effect on a cache that never filled up
- **Storage**: Local filesystem, S3, or hybrid (local L1 + S3 L2)
- **Recency correctness**: `LRULocalStorage` wraps `LocalStorage` and records an access on every read path that yields a size, so a file being served cannot read as cold to the evictor

There is no separate in-process file cache. `cache.FileCache` existed as a third tier with no production callers and was deleted in `6a11326`.

### Response Cache
- **Duration**: Short-term caching (5min default)
- **Key strategy**: URL + Accept header
- **Compression**: Cached compressed responses
- **Memory**: Minimal overhead per entry

## Load Testing Results

### Stress Test Configuration
- **Tool**: wrk (HTTP benchmarking)
- **Duration**: 60 seconds per test
- **Concurrency**: 8 threads, 100 connections
- **Endpoints**: Package index, package details, downloads
- **Test Environment**: Docker containers on macOS (groxpi vs proxpi)
- **Cache Scenarios**: Cold cache (cleared) and warm cache (pre-populated)

### Results Under Load

**Benchmark run of December 2024 — 60s duration, 8 threads, 100 connections. Not re-measured since the 2026-07 refactor.**

```bash
# Groxpi - Package Index (/simple/) - Warm Cache
Running 1m test @ http://localhost:5005/simple/
  8 threads and 100 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency    43.95ms   77.45ms   1.40s    83.74%
    Req/Sec     6.66k     2.38k   15.63k    68.35%
  Latency Distribution
     50%  847.00us
     75%   74.02ms
     90%  155.44ms
     99%  326.77ms
  3178323 requests in 1.00m, 0.96GB read
Requests/sec:  52880.15
Transfer/sec:     16.44MB

# Groxpi - Package Files (numpy) - Warm Cache
Running 1m test @ http://localhost:5005/simple/numpy/
  8 threads and 100 connections
  Latency Distribution
     50%   14.55ms
     75%   65.56ms
     90%  162.20ms
     99%  378.86ms
  252517 requests in 1.00m, 136.88GB read
Requests/sec:   4203.88
Transfer/sec:      2.28GB

# Proxpi - Package Index (/simple/) - Warm Cache
Running 1m test @ http://localhost:5006/simple/
  8 threads and 100 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency    23.19ms    2.81ms  81.56ms   80.17%
    Req/Sec   519.93     37.70   660.00     74.77%
  248710 requests in 1.00m, 88.47MB read
  Non-2xx or 3xx responses: 248710  # ALL RESPONSES FAILED
Requests/sec:   4139.27
```

**Key Finding**: Under high load (100 concurrent connections), proxpi returned non-2xx responses for ALL requests, while groxpi maintained stable performance with sub-millisecond P50 latency.

### Capacity Planning
- **Single instance**: 1000+ concurrent users
- **Horizontal scaling**: Stateless design
- **Resource usage**: 1 CPU core, 512MB RAM minimum
- **Network**: 1Gbps saturated at ~8000 RPS

## Optimization Strategies

### CPU Optimization
- **GOMAXPROCS**: Automatic CPU detection
- **Worker pools**: `WorkerPool[T]` bounds the tiered L1 back-fill (not client downloads, which are bounded by singleflight dedup, and no longer S3 writes)
- **Read-mostly locking**: `sync.RWMutex` on every cache; `ResponseCache.Get` upgrades to a write lock only to reorder LRU
- **SIMD**: JSON processing acceleration via Sonic

### Memory Optimization
- **Buffer reuse**: `sync.Pool` for 64KB download copy buffers and upstream index parsing; nothing pooled on the response or S3 write paths
- **Package name normalization**: lowercase + `_`→`-`, so cache keys collapse equivalent spellings (this is key normalization, not string interning)
- **Streaming**: No full-file memory loading
- **GC tuning**: GOGC=100 for balanced performance

### I/O Optimization
- **Connection pooling**: HTTP client reuse
- **TCP keepalive**: Persistent connections
- **Compression**: Automatic response compression
- **Serve-by-path**: locally cached files handed to `net/http` for range/conditional handling (see above — not a kernel zero copy)

### Storage Optimization
- **S3 multipart**: automatic part sizing for large objects
- **Local caching**: L1 on local disk in hybrid mode, back-filled asynchronously from S3 on an L2 hit
- **Prefetching**: not implemented
- **On-disk compression**: not implemented

## Monitoring Performance

### Key Metrics
- **Request rate**: Requests per second
- **Response time**: P50, P95, P99 latencies
- **Cache hit ratio**: Index and file cache efficiency
- **Error rate**: 4xx/5xx response percentage
- **Memory usage**: Heap size and GC frequency

### Prometheus Metrics (Planned)
```yaml
# Example metrics
groxpi_requests_total{method="GET",status="200"}
groxpi_request_duration_seconds{endpoint="/simple/"}
groxpi_cache_hits_total{type="index"}
groxpi_cache_misses_total{type="file"}
```

## Performance Tuning

### Environment Variables
```bash
# Go runtime optimization
export GOMAXPROCS=4
export GOGC=100
export GOMEMLIMIT=512MiB

# Groxpi optimization
export GROXPI_CACHE_SIZE=10737418240  # 10GB
export GROXPI_RESPONSE_CACHE_TTL=300  # 5 minutes
export GROXPI_MAX_CONCURRENT_DOWNLOADS=20
```

### System Tuning
```bash
# Linux optimization
echo 'net.core.somaxconn = 65535' >> /etc/sysctl.conf
echo 'net.ipv4.tcp_tw_reuse = 1' >> /etc/sysctl.conf
echo 'fs.file-max = 100000' >> /etc/sysctl.conf
ulimit -n 65535
```

## Benchmark Suite

### Running Benchmarks
```bash
# Full benchmark suite (API + UV installation tests)
./benchmarks/benchmark.sh --groxpi-url http://localhost:5005 --proxpi-url http://localhost:5006

# API benchmarks only
./benchmarks/benchmark.sh --groxpi-url http://localhost:5005 --proxpi-url http://localhost:5006 --api-only

# UV package installation tests only
./benchmarks/benchmark.sh --groxpi-url http://localhost:5005 --proxpi-url http://localhost:5006 --uv-only

# Disable resource monitoring
./benchmarks/benchmark.sh --groxpi-url http://localhost:5005 --proxpi-url http://localhost:5006 --no-monitoring

# Use environment variables
export GROXPI_URL=http://localhost:5005
export PROXPI_URL=http://localhost:5006
./benchmarks/benchmark.sh
```

### Benchmark Components
- **API tests**: WRK-based HTTP load testing with cold/warm cache scenarios
- **Installation tests**: Real package installation with UV package manager
- **Resource monitoring**: Docker container CPU, memory, I/O tracking
- **Cache management**: Automated cache clearing and verification
- **DuckDB analysis**: SQL-based results analysis and reporting

### Benchmark Results Structure
```
benchmarks/results/
├── benchmark-report-YYYYMMDD_HHMMSS.md     # Consolidated report
├── wrk-summary-YYYYMMDD_HHMMSS.csv         # API performance metrics
├── uv-summary-YYYYMMDD_HHMMSS.csv          # Package installation times
├── resources-YYYYMMDD_HHMMSS.csv           # Resource usage over time
└── individual test logs...                 # Detailed per-test logs
```

All benchmark results are timestamped and include DuckDB-compatible CSV files for advanced analysis.