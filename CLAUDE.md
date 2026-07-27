# groxpi - Go PyPI Proxy

A PyPI caching proxy server written in Go, reimplemented from the Python-based proxpi project using the
Gin framework.

## Project Goals

1. **Efficiency**: Leverage Go's concurrency and the standard library for minimal CPU/memory usage
2. **Feature Parity**: Maintain the features of the original proxpi implementation, extended with S3 as a
   cache backend
3. **Production Ready**: Built for reliability, observability, and ease of deployment

## Architecture Overview

### Technology Stack
- **Language**: Go 1.26+
- **Web Framework**: [Gin v1.11](https://gin-gonic.com/)
- **JSON Processing**: standard library `encoding/json`. Sonic is *not* used; it appears in `go.mod` only
  as an indirect dependency of gin
- **Templates**: `html/template`, embedded with `go:embed` (admin page only)
- **Cache**: one bounded in-memory index cache (LRU by last access + background TTL sweep); LRU eviction
  over the on-disk object store
- **Storage**: Local filesystem, S3-compatible via the AWS SDK for Go v2 (AWS S3/MinIO), or hybrid
  (local L1 + S3 L2)
- **Logging**: stdlib `log/slog`, bridged to OpenTelemetry via
  [otelslog](https://pkg.go.dev/go.opentelemetry.io/contrib/bridges/otelslog)
- **Telemetry**: OpenTelemetry SDK, all three signals over OTLP/HTTP; inert with no endpoint configured
- **Middleware**: Recovery, structured logging, request tracing. **No compression middleware** — index
  bodies carry their gzipped form in the cache entry and package files are already-compressed archives

## Architecture Principles

### Simplicity First
- Every change should impact as little code as possible
- Prefer simple, readable solutions over complex optimizations
- Use Go's standard library where possible before adding dependencies

### Performance Guidelines
- Use goroutines for concurrent operations (querying multiple indexes)
- Implement efficient caching with minimal lock contention
- Stream large files instead of loading into memory
- Follow the singleflight pattern to reduce IO overhead
- Serve locally-cached files by path (`storage.ZeroCopyCapable` → `c.File`) so `net/http` handles range
  and conditional requests. This is **not** a kernel zero copy: gin's response writer implements neither
  `File()` nor `io.ReaderFrom`, so the bytes are still copied through user space. Do not add code or docs
  claiming otherwise
- Use byte pools for frequent allocations
- Do not add speculative machinery. A seam with one production caller is a liability
- **No performance figures in documentation** until the benchmark suite is re-run against current code.
  Delete a stale number; never update it by guess

### Code Organization
```
groxpi/
├── cmd/groxpi/          # Main application entry point
├── internal/            # Private application code
│   ├── cache/          # index.go: bounded index cache (files + JSON + gzip per entry)
│   ├── config/         # Configuration, URL redaction, index resolution order
│   ├── logger/         # slog setup: stdout handler + OpenTelemetry log bridge
│   ├── telemetry/      # OTel provider setup (traces, metrics, logs) over OTLP
│   ├── pypi/           # PyPI client (JSON first, HTML fallback) + PEP 503 normalisation
│   ├── server/         # server.go (Gin transport), packagefile.go (PackageFileService),
│   │                   #   admin.go + prefetch.go (admin surface)
│   ├── storage/        # Storage seam: storage.go (Storage + capability interfaces),
│   │                   #   local.go, lru.go, s3.go, tiered.go, workerpool.go
│   └── streaming/      # interfaces.go + downloader.go (tee download-and-cache)
├── docs/               # Six documents plus adr/ — see below
├── benchmarks/         # Benchmark harness (publishes no figures)
├── monitoring/         # prometheus.yml (scrapes an OTel collector, not groxpi)
├── templates/          # admin.html, rows.html, htmx.min.js, templates.go (go:embed)
├── Dockerfile          # Multi-stage build onto a scratch image
├── docker-compose.yml  # Production Docker Compose setup
└── docker-compose.minio.yml # MinIO S3 storage setup
```

`tasks/` is gitignored: task notes are local, not committed.

## Development Workflow

1. **Planning**: Before any task, look for related documentation in `docs/`, document the approach in
   `tasks/<task_name>.md`, use `context7` to search package/framework documents, and ask the user to
   verify the plan
2. **Implementation**: Keep changes small and focused; follow DRY and YAGNI
3. **Testing**: Write tests alongside implementation, following TDD
4. **Review**: Self-review for simplicity and performance; update `tasks/<task_name>.md` before finishing
5. **Finish**: Run the tests, then bring the documents in line with the code
6. **Commit**: Run `gofmt`, `go vet` and `golangci-lint` before committing

## Documentation

Six documents, no duplication between them:

- **[README.md](README.md)** — what groxpi is, quick start, client configuration, migration notes
- **[docs/configuration.md](docs/configuration.md)** — every environment variable
- **[docs/api-endpoints.md](docs/api-endpoints.md)** — every route, content negotiation, error codes
- **[docs/architecture.md](docs/architecture.md)** — modules, caches, storage seam, request flows,
  telemetry signals and spans
- **[docs/deployment.md](docs/deployment.md)** — Docker, Kubernetes, TLS, observability wiring
- **[docs/benchmarking.md](docs/benchmarking.md)** — how to run the suite
- **[docs/adr/](docs/adr/)** — decision records: extras-first index resolution, OTLP over Prometheus scrape

A documented setting must exist in `internal/config/config.go`; a documented endpoint must exist in the
router. Add nothing aspirational.

## Quick Start

```bash
go run ./cmd/groxpi        # run locally
go test ./...              # run tests
go build -o groxpi ./cmd/groxpi
docker compose up -d       # production
```

## Code Quality Standards

- Use `gofmt` for formatting
- Run `go vet` for static analysis
- Keep functions small and focused
- Document exported functions
- Handle errors explicitly
- No panics in production code

## License

MIT License - Same as original proxpi project
