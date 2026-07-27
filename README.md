# groxpi

A caching PyPI proxy written in Go, reimplemented from the Python
[proxpi](https://github.com/EpicWink/proxpi) project.

[![Go Version](https://img.shields.io/badge/go-1.26+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

- PyPI Simple API, PEP 503 and PEP 691 — a drop-in index URL for pip, uv, Poetry and pipenv
- Downloads stream to the client and into the cache in one pass, with every cached file verified
  against the hash or length the index declared
- Multiple upstream indexes with per-index TTLs, extras taking priority over the primary
- Local filesystem, S3-compatible, or hybrid local-over-S3 storage
- OpenTelemetry traces, metrics and logs over OTLP; inert when no collector is configured
- Optional password-protected cache admin page with prefetch and eviction

> No performance figures are published here. Everything the repository once quoted predates the current
> architecture and was deleted rather than restated. See [docs/benchmarking.md](docs/benchmarking.md) to
> measure it yourself.

## Quick start

```bash
docker compose up -d
pip install --index-url http://localhost:5005/simple/ numpy
```

Or from source:

```bash
go run ./cmd/groxpi
```

groxpi listens on port `5000` by default (the bundled Compose file publishes it on `5005`).

## Configure a client

```bash
pip install --index-url http://localhost:5000/simple/ numpy
UV_INDEX_URL=http://localhost:5000/simple/ uv pip install numpy
```

```ini
# pip.conf / pip.ini
[global]
index-url = http://localhost:5000/simple/
```

```toml
# pyproject.toml, Poetry
[[tool.poetry.source]]
name = "groxpi"
url = "http://localhost:5000/simple/"
priority = "primary"
```

```toml
# pyproject.toml, uv
[[tool.uv.index]]
name = "groxpi"
url = "http://localhost:5000/simple/"
default = true
```

```toml
# Pipfile
[[source]]
name = "groxpi"
url = "http://localhost:5000/simple/"
verify_ssl = false
```

## Common settings

| Variable | Default | Meaning |
|---|---|---|
| `GROXPI_INDEX_URL` | `https://pypi.org/simple/` | Upstream index |
| `GROXPI_INDEX_TTL` | `1800` | Index cache TTL, seconds |
| `GROXPI_CACHE_DIR` | OS temp dir | Where cached files live |
| `GROXPI_CACHE_SIZE` | `5368709120` | File cache byte budget |
| `GROXPI_STORAGE_TYPE` | `local` | `local`, `s3`, `hybrid` |
| `PORT` | `5000` | Listen port |

[docs/configuration.md](docs/configuration.md) is the full reference — every variable groxpi reads, and
nothing it does not.

## Migrating from Python proxpi

The URLs are the same, so clients need no changes. Environment variables keep their names where the
setting still exists; `PROXPI_`-prefixed spellings are not read. Two things changed behaviour:

- **Extra indexes now participate in resolution**, queried before the primary, first hit winning whole.
  A package that used to resolve to PyPI may now resolve to an extra index.
- **`DELETE /cache/list` and `DELETE /cache/<package>` require credentials** and answer `404` when the
  admin surface is not configured. They were open before.

Both are covered in [docs/configuration.md](docs/configuration.md) and
[docs/api-endpoints.md](docs/api-endpoints.md).

## Development

```bash
go build -o groxpi ./cmd/groxpi
go test ./...            # full suite
go test -short ./...     # skips tests needing a live S3
gofmt -l . && go vet ./... && golangci-lint run
```

## Documentation

- [Configuration](docs/configuration.md) — every environment variable
- [API](docs/api-endpoints.md) — every route, content negotiation, error codes
- [Architecture](docs/architecture.md) — modules, caches, storage seam, request flows, telemetry
- [Deployment](docs/deployment.md) — Docker, Kubernetes, TLS, observability wiring
- [Benchmarking](docs/benchmarking.md) — how to measure it
- [Decision records](docs/adr/) — why index resolution and telemetry work the way they do

## License

MIT — see [LICENSE](LICENSE). Original [proxpi](https://github.com/EpicWink/proxpi) by EpicWink.
