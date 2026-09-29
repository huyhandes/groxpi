# groxpi — Go PyPI Proxy

A PyPI caching proxy in Go, reimplemented from the Python `proxpi` on the standard library's `net/http`. Local filesystem, S3 (AWS/MinIO), or hybrid (local cache in front of an S3 durable store) storage.

## Commands

```bash
go run ./cmd/groxpi                              # run locally
go test -short -race ./...                       # tests (CI flags)
go test -short -race -coverprofile=coverage.out -covermode=atomic ./...   # with coverage
go build -o groxpi ./cmd/groxpi                  # build binary
docker compose up -d                             # production
gofmt -s -l .                                    # format check (must print nothing)
go vet ./...                                     # static analysis
staticcheck ./...                                # CI test job runs this
golangci-lint run --timeout=10m                  # CI lint job (golangci-lint v2.12.2)
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

- **"Zero-copy" is delegated correctness, not a measured saving.** A cache hit from `storage.Store.Open` is a seekable `Object` (an `*os.File` locally; for s3 a `GetObject` issued before response headers, read in bounded ranged windows) that `server.handleFile` hands to `http.ServeContent`, so `net/http` handles range and conditional requests. The tracing middleware's `statusWriter` must keep forwarding `io.ReaderFrom` and `Unwrap` (there is a test) so the stdlib writer's own copy path survives wrapping. **Never add code or docs claiming a kernel-level zero copy** — nothing here measures one.
- **Server owns all HTTP** (ADR 0004): every route, handler, template and redirect decision lives in `server` (`server.go` with `handleFile`, `index.go`, `admin.go`). `index`, `download`, `storage` and `admin` are plain Go services with no handler code.
- **Storage is the cache, read-through.** `Store.Open` serves a hit, or runs one detached flight per key: `Fetcher.Fetch` → spool file (hashed while written) → every client tails it, last byte held back until verified → rename into the cache, then (hybrid) the same goroutine uploads it to S3 inline, or (s3) upload only. The local cache is bounded and disposable and the only thing that evicts; S3 is the durable store, never expired by groxpi (bucket lifecycle rules own retention). Hybrid cache miss + S3 hit is promoted through the same spool and not re-uploaded. The filesystem is the local LRU index (atime, throttled touches, evict to 90%).
- **Modules are deep and one-directional:** `server → admin → storage → download → index`, with `server` also calling `storage` and `index`, and `admin` also calling `index`. Surfaces: `index.Resolve/Invalidate/ProxyRoot`, `download.Resolve/Fetch` (stateless), `storage.Store.Open/InFlight/Snapshot/DeletePrefix`, `admin.List/Evict/Prefetch/InFlight`. The one cross-module dependency interface is `storage.Fetcher` (consumer-defined, implemented by `download`, faked in tests); the `Store` holds a concrete local cache and/or S3 store, with no backend interface. Don't add interfaces with one implementation.
- **`tasks/` is gitignored** — task notes are local, never committed. Plan into `tasks/<name>.md` if the user asks for a plan.
- **`context7`** is an MCP tool available in this workspace for searching package/framework docs. Use it when you need upstream library facts.
- **A documented setting must exist in `internal/config/config.go`; a documented endpoint must exist in the router.** Add nothing aspirational — if you document it, implement it.

## Code organization

```
cmd/groxpi/        main.go (entry point, --health-check flag)
internal/
  server/          server.go (composition root, mux, handleFile, recover + trace middleware, shutdown),
                   index.go (simple index routes), admin.go (auth, cross-site, page, rows, prefetch, eviction)
  index/           service.go (Resolve/Invalidate, extras-first, singleflight, root proxy),
                   cache.go (bounded index cache: files + JSON + gzip per entry),
                   client.go (PyPI client, JSON first, HTML fallback), normalize.go
  download/        service.go (stateless Resolve/Fetch, PEP 658 metadata siblings)
  admin/           admin.go (List/Evict/InFlight), prefetch.go — pure service, no HTTP
  storage/         store.go (read-through Store: flight, spool, tail, ranged S3 reader),
                   local.go (the cache: atime LRU), atime_*.go, s3.go (the durable store)
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
- `docs/architecture.md` — modules, caches, cache and durable store, request flows, telemetry spans
- `docs/deployment.md` — Docker, Kubernetes, TLS, observability wiring
- `docs/benchmarking.md` — how to run the suite
- `docs/backlog.md` — known limits, each with a proposed fix and check; delete an entry when it ships
- `docs/adr/` — decision records: extras-first index resolution, OTLP over Prometheus scrape, stdlib mux and module boundaries, read-through storage and server-owned HTTP
- `CONTEXT.md` — glossary of the storage vocabulary (Store, flight, spool, tail, promotion, cache, durable store)

**No performance figures in documentation** until the benchmark suite is re-run against current code. Delete a stale number; never update one by guess.

## Conventions that differ from the default

- Keep changes small and impact-localized; prefer stdlib over new dependencies.
- A seam with one production caller is a liability — don't add speculative machinery.
- Goroutines for concurrent operations (e.g. querying multiple indexes); singleflight to dedupe IO.
- Stream large files; never load them fully into memory.

## License

MIT, same as original proxpi.
