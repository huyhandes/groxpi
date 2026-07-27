# groxpi - Go PyPI Proxy

A high-performance PyPI caching proxy server written in Go, reimplemented from the Python-based proxpi project using Gin framework and Sonic JSON.

## Project Goals

1. **Maximum Performance**: Leverage Go's concurrency, Gin framework, and Sonic JSON (3x faster) for minimal CPU/memory usage
2. **Feature Parity**: Maintain all features from the original proxpi implementation, extend the power to use S3 as cache backend
3. **Production Ready**: Built for reliability, observability, and ease of deployment

## Architecture Overview

### Technology Stack
- **Language**: Go 1.26+
- **Web Framework**: [Gin v1.11](https://gin-gonic.com/) - High-performance HTTP web framework
- **JSON Processing**: [ByteDance Sonic](https://github.com/bytedance/sonic) - Blazingly fast JSON serialization
- **Templates**: Go HTML templates with Gin integration
- **Cache**: In-memory TTL index cache + LRU response cache; LRU eviction over the on-disk object store
- **Storage**: Local filesystem, S3-compatible (MinIO/AWS S3), or hybrid (local L1 + S3 L2)
- **Logging**: stdlib `log/slog`, bridged to OpenTelemetry via [otelslog](https://pkg.go.dev/go.opentelemetry.io/contrib/bridges/otelslog)
- **Telemetry**: OpenTelemetry SDK, all three signals over OTLP/HTTP; inert with no endpoint configured
- **Middleware**: Recovery, structured logging, compression

## Architecture Principles

### Simplicity First
- Every change should impact as little code as possible
- Prefer simple, readable solutions over complex optimizations
- Use Go's standard library where possible before adding dependencies

### Performance Guidelines
- Use goroutines for concurrent operations (fetching from multiple indices)
- Implement efficient caching with minimal lock contention
- Stream large files instead of loading into memory
- Follow SingleFlight pattern to reduce IO overhead
- Serve locally-cached files by path (`storage.ZeroCopyCapable` → `c.File`) so `net/http` handles range and conditional requests. Note this is **not** a kernel zero copy: gin's response writer implements neither `File()` nor `io.ReaderFrom`, and the gzip middleware wraps it anyway, so the bytes are still copied through user space. Do not add code or docs claiming otherwise
- Use byte pools for frequent allocations
- Leverage Gin's built-in optimizations
- Do not add speculative machinery. A seam with one production caller is a liability; the A–E refactor deleted ~2,000 lines of it (see `tasks/architecture-improvement-plan.md`)

### Code Organization
```
groxpi/
├── cmd/groxpi/          # Main application entry point
├── internal/            # Private application code
│   ├── cache/          # index.go (TTL map) + response.go (LRU of marshaled JSON)
│   ├── config/         # Configuration management
│   ├── logger/         # slog setup: stdout handler + OpenTelemetry log bridge
│   ├── telemetry/      # OTel provider setup (traces, metrics, logs) over OTLP
│   ├── pypi/           # PyPI client with Sonic JSON
│   ├── server/         # server.go (Gin transport) + packagefile.go (PackageFileService)
│   ├── storage/        # Storage seam: storage.go (Storage + capability interfaces),
│   │                   #   local.go, lru.go, s3.go, tiered.go, workerpool.go
│   └── streaming/      # interfaces.go + downloader.go (tee download-and-cache)
├── docs/               # Detailed documentation
├── benchmarks/         # Performance benchmarking suite
├── monitoring/         # prometheus.yml (scrapes an OTel collector, not groxpi)
├── templates/          # HTML templates with layouts
│   ├── layouts/        # Main layout templates
│   └── partials/       # Reusable template components
├── tasks/              # Development task tracking
├── tests/              # Test results and data
├── Dockerfile          # Multi-stage production Docker build
├── docker-compose.yml  # Production Docker Compose setup
└── docker-compose.minio.yml # MinIO S3 storage setup
```

## Development Workflow

1. **Planning**: Before doing any tasks(features, fix, test,...), look for related documentation in `docs/`,document the approach in `tasks/<task_name>.md`, use `context7` to search for package/framwork documents and ask user to verify your plan
2. **Implementation**: Keep changes small and focused, keep the code follow DRY and YAGNI principals
3. **Testing**: Write tests alongside implementation, always follow TDD
4. **Review**: Self-review for simplicity and performance, update the `tasks/<task_name>.md` before done task
5. **Finish**: Make sure code functionality do not break by running test. Then make documents up-to-date with the codebase.
    6. **Commit**: Before commit, run `gofmt`, `go vet` and `golangci-lint` to ensure the code is well formarted and no linting error

## Core Features ✅

Production-ready PyPI caching proxy with enterprise-grade performance and reliability.

**📋 See [docs/implemented-features.md](docs/implemented-features.md)** for complete feature list including:
- PyPI Simple API compliance (PEP 503/691)
- Advanced multi-level caching system
- Simultaneous stream-to-client and stream-to-cache downloads
- Multi-backend storage (local/S3/hybrid)
- High-performance optimizations

**🏛️ See [docs/architecture.md](docs/architecture.md)** for the module map, seams, request flows and remaining friction.

### Configuration
Full compatibility with original proxpi configuration through environment variables.

**📖 See [docs/configuration.md](docs/configuration.md)** for complete configuration reference including:
- Core configuration options
- Storage backends (local/S3/hybrid)
- Performance tuning
- Example configurations

## API Endpoints
Fully compliant PyPI Simple API (PEP 503/691) with cache management endpoints.

**📖 See [docs/api-endpoints.md](docs/api-endpoints.md)** for complete API reference including:
- Package index endpoints
- Download/redirect behavior
- Content negotiation details
- Cache management endpoints
- Error responses and compatibility

## Performance
Substantially faster than the original Python proxpi, with sub-millisecond P50 latency on cached index responses.

> The "16,000x" figure previously quoted here is not supported by any benchmark in this repo — the measured index-throughput gain is 12.8x (`docs/performance.md`). All published figures were recorded in `ca9c979` (2025-12-29, labelled "December 2024") and have **not** been re-measured since the 2026-07 architecture refactor. Treat them as unverified against current code.

**📊 See [docs/performance.md](docs/performance.md)** for detailed benchmarks including:
- API performance metrics
- Buffer-pool and streaming optimizations
- Load testing results
- Performance tuning guide

## Quick Start

### Development
```bash
# Run locally
go run cmd/groxpi/main.go

# Run tests
go test ./...

# Build binary
go build -o groxpi cmd/groxpi/main.go
```

### Production
```bash
# Docker
docker run -p 5000:5000 groxpi:latest

# Docker Compose (recommended)
docker-compose up -d
```

## Documentation

### Core Documentation
- **📖 [Configuration](docs/configuration.md)** - Environment variables and setup
- **🔌 [API Endpoints](docs/api-endpoints.md)** - Complete API reference
- **🚀 [Deployment](docs/deployment.md)** - Development to production deployment
- **🔄 [Migration](docs/migration.md)** - Migrating from Python proxpi

### Advanced Topics
- **📊 [Performance](docs/performance.md)** - Benchmarks and optimization
- **📊 [Monitoring](docs/monitoring.md)** - Observability and health checks
- **🧪 [Testing](docs/testing.md)** - Test strategy and coverage

## Status: **🚀 PRODUCTION-READY**

## Code Quality Standards

- Use `gofmt` for formatting
- Run `go vet` for static analysis
- Keep functions small and focused
- Document exported functions
- Handle errors explicitly
- No panics in production code
- Follow Gin and Sonic best practices

## License

MIT License - Same as original proxpi project

---
