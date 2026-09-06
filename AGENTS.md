# groxpi — Go PyPI Proxy

A PyPI caching proxy in Go, reimplemented from the Python `proxpi` on the standard library's `net/http`. Local filesystem, S3 (AWS/MinIO), or hybrid (local L1 + S3 L2) storage.

## Commands

```bash
go run ./cmd/groxpi                              # run locally
go test -short -race ./...                       # tests (CI flags)
go test -short -race -coverprofile=coverage.out -covermode=atomic ./...   # with coverage
go build -o groxpi ./cmd/groxpi                  # build binary
docker compose up -d                             # production
gofmt -s -l .                                    # format check (must print nothing)
go vet ./...                                     # static analysis
staticcheck ./...                                # CI runs this, NOT golangci-lint
```

The Dockerfile builds to a `scratch` image; the health check calls `/groxpi --health-check`.

## Stack (non-obvious choices)

- **Go 1.26**, **stdlib `net/http` + `http.ServeMux`** with method-and-pattern routes (`GET /simple/{package}/{$}`). No web framework; see `docs/adr/0003-stdlib-mux-and-module-boundaries.md`.
- **JSON: stdlib `encoding/json` only.**
- **Templates: `html/template`** embedded with `go:embed` (admin page only). The scratch image ships only the binary.
- **Logging: stdlib `log/slog`** bridged to OpenTelemetry via `otelslog`.
- **Telemetry: OpenTelemetry SDK**, all three signals over OTLP/HTTP. Inert when no endpoint is configured. `monitoring/prometheus.yml` scrapes an OTel collector, not groxpi.
- **No compression middleware.** Index bodies carry their gzipped form in the cache entry; package files are already-compressed archives.

## Architecture traps

- **"Zero-copy" is delegated correctness, not a measured saving.** `storage.ZeroCopyCapable` backends (Local, Tiered) let the download handler hand the open file to `http.ServeContent`, so `net/http` handles range and conditional requests. The tracing middleware's `statusWriter` must keep forwarding `io.ReaderFrom` and `Unwrap` (there is a test) so the stdlib writer's own copy path survives wrapping. **Never add code or docs claiming a kernel-level zero copy** — nothing here measures one.
- **Modules are deep and one-directional:** `server → admin → download → index → storage`. Each exposes `Register(mux)` plus a couple of methods (`index.Resolve/Invalidate`, `download.Plan/Fetch/Warm/InFlight`). The only cross-module interface is `download.Resolver` (one method, consumer-defined, faked in tests). Don't add interfaces with one implementation.
- **`tasks/` is gitignored** — task notes are local, never committed. Plan into `tasks/<name>.md` if the user asks for a plan.
- **`context7`** is an MCP tool available in this workspace for searching package/framework docs. Use it when you need upstream library facts.
- **A documented setting must exist in `internal/config/config.go`; a documented endpoint must exist in the router.** Add nothing aspirational — if you document it, implement it.

## Code organization

```
cmd/groxpi/        main.go (entry point, --health-check flag)
internal/
  server/          composition root: storage construction, mux, recover + trace middleware, shutdown
  index/           service.go (Resolve/Invalidate, extras-first, singleflight, root proxy),
                   cache.go (bounded index cache: files + JSON + gzip per entry),
                   client.go (PyPI client, JSON first, HTML fallback), normalize.go, handler.go
  download/        service.go (Plan/Fetch/Warm), inflight.go (running-download registry),
                   downloader.go (verified tee download-and-cache), handler.go
  admin/           admin.go (auth, cross-site, page, rows, eviction), prefetch.go
  storage/         storage.go (Storage + capability interfaces), local.go, lru.go,
                   s3.go, tiered.go, workerpool.go
  config/          config, URL redaction, index resolution order
  logger/          slog setup: stdout handler + OTel log bridge
  telemetry/       OTel provider setup (traces, metrics, logs) over OTLP
docs/              six docs + adr/ (see below)
benchmarks/        benchmark harness — publishes no figures
templates/         admin.html, rows.html, htmx.min.js, templates.go (go:embed)
```

## Documentation map

No duplication between these — each owns its surface:

- `README.md` — what groxpi is, quick start, client config, migration
- `docs/configuration.md` — every environment variable
- `docs/api-endpoints.md` — every route, content negotiation, error codes
- `docs/architecture.md` — modules, caches, storage seam, request flows, telemetry spans
- `docs/deployment.md` — Docker, Kubernetes, TLS, observability wiring
- `docs/benchmarking.md` — how to run the suite
- `docs/adr/` — decision records: extras-first index resolution, OTLP over Prometheus scrape, stdlib mux and module boundaries

**No performance figures in documentation** until the benchmark suite is re-run against current code. Delete a stale number; never update one by guess.

## Conventions that differ from the default

- Keep changes small and impact-localized; prefer stdlib over new dependencies.
- A seam with one production caller is a liability — don't add speculative machinery.
- Goroutines for concurrent operations (e.g. querying multiple indexes); singleflight to dedupe IO.
- Stream large files; never load them fully into memory.

## License

MIT, same as original proxpi.
