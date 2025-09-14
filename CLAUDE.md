# groxpi - Go PyPI Proxy

A high-performance PyPI caching proxy server written in Go, reimplemented from the Python-based proxpi project using Fiber framework and Sonic JSON.

## Project Goals

1. **Maximum Performance**: Leverage Go's concurrency, Fiber framework (2x faster), and Sonic JSON (3x faster) for minimal CPU/memory usage
2. **Feature Parity**: Maintain all features from the original proxpi implementation, extend the power to use S3 as cache backend
3. **Production Ready**: Built for reliability, observability, and ease of deployment

## Architecture Overview

### Technology Stack
- **Language**: Go 1.24+
- **Web Framework**: [Fiber v2](https://gofiber.io/) - Express-inspired, ultra-fast HTTP framework
- **JSON Processing**: [ByteDance Sonic](https://github.com/bytedance/sonic) - Blazingly fast JSON serialization
- **Templates**: Go HTML templates with Fiber integration
- **Cache**: In-memory TTL cache + LRU file cache
- **Storage**: Local filesystem + S3-compatible storage (MinIO/AWS S3)
- **Logging**: [phuslu/log](https://github.com/phuslu/log) - High-performance structured logging
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
- Apply Zero-copy optimization to remove Memory overhead and avoid GC
- Use byte pools for frequent allocations
- Leverage Fiber's built-in optimizations

### Code Organization
```
groxpi/
├── cmd/groxpi/          # Main application entry point
├── internal/            # Private application code
│   ├── cache/          # Caching implementations (TTL + LRU + Response)
│   ├── config/         # Configuration management
│   ├── logger/         # Structured logging with phuslu/log
│   ├── pypi/           # PyPI client with Sonic JSON
│   ├── server/         # Fiber HTTP server and handlers
│   ├── storage/        # Storage backend abstraction (local/S3)
│   └── streaming/      # Zero-copy streaming (broadcast, downloader, zerocopy)
├── docs/               # Detailed documentation
├── benchmarks/         # Performance benchmarking suite
├── monitoring/         # Monitoring and observability
│   ├── grafana/        # Grafana dashboards and datasources
│   └── prometheus.yml  # Prometheus configuration
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
- Zero-copy streaming with broadcast capabilities
- Multi-backend storage (local/S3)
- High-performance optimizations

### Configuration
Full compatibility with original proxpi configuration through environment variables.

**📖 See [docs/configuration.md](docs/configuration.md)** for complete configuration reference including:
- Core configuration options
- Storage backends (local/S3)
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
16,000x performance improvement over original Python proxpi with sub-millisecond response times.

**📊 See [docs/performance.md](docs/performance.md)** for detailed benchmarks including:
- API performance metrics
- Zero-copy optimizations
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
- Follow Fiber and Sonic best practices

## License

MIT License - Same as original proxpi project

---
