# Architecture

groxpi is a caching PyPI proxy: a Gin HTTP transport over an index cache, an object store and one
upstream client.

## Module map

```
cmd/groxpi/            main: config, telemetry, logger, server, graceful shutdown
internal/
  cache/     index.go        bounded index cache (LRU by last access + background TTL sweep)
  config/    config.go       every environment variable, URL redaction, index resolution order
  logger/    logger.go       slog setup: stdout handler plus the OpenTelemetry log bridge
  pypi/      client.go       upstream index client (JSON first, HTML fallback)
             normalize.go    PEP 503 name normalisation
  server/    server.go       routes, handlers, storage construction
             packagefile.go  PackageFileService: index resolution, serve planning, root proxy
             admin.go        admin page view model
             prefetch.go     prefetch and newest-release selection
  storage/   storage.go      Storage interface plus capability interfaces
             local.go        plain filesystem backend
             lru.go          filesystem backend with LRU eviction
             s3.go           AWS SDK v2 backend
             tiered.go       hybrid: local L1 over S3 L2
             workerpool.go   bounded worker pool used by the tiered backend
  streaming/ interfaces.go   StreamingDownloader
             downloader.go   tee download: client and cache from one upstream read
  telemetry/ telemetry.go    OTLP providers for traces, metrics and logs
             instruments.go  the metrics groxpi records
templates/                   admin.html, rows.html, htmx.min.js — embedded with go:embed
monitoring/prometheus.yml    scrapes an OpenTelemetry collector, never groxpi
benchmarks/                  benchmark harness (see benchmarking.md)
docs/adr/                    architecture decision records
```

JSON is the standard library's `encoding/json`. groxpi does not use Sonic; it appears in `go.mod` only
as an indirect dependency of gin.

## The index cache

One cache, one byte budget. Each entry holds, for one package: the parsed file list, the JSON response
body, and the gzipped JSON response body. All three are accounted against `GROXPI_INDEX_CACHE_SIZE`.
Entries leave by TTL — a background sweep, so an expired entry's memory is actually reclaimed rather
than waiting for the next lookup — or by LRU on last access when the budget is exceeded.

Producing the serialised bodies at fill time is what removes the separate response cache: the request
path never marshals and never compresses. A client that accepts gzip is handed bytes that were
compressed once, when the entry was built.

## The storage seam

```go
type Storage interface {
    Get(ctx, key) (io.ReadCloser, *ObjectInfo, error)
    Put(ctx, key, reader, size, contentType) (*ObjectInfo, error)
    Delete(ctx, key) error
    Exists(ctx, key) (bool, error)
    Close() error
}
```

Five methods. Anything a single backend can do that the others cannot is a separate, optional
interface, asked for with a type assertion rather than answered by a boolean the backend has to lie
about:

| Capability | Method | Implemented by |
|---|---|---|
| `ZeroCopyCapable` | `GetFilePath` | the filesystem backends |
| `PrefixDeleter` | `DeletePrefix` | the filesystem backends, and the tiered backend for its L1 |
| `cacheSnapshotter` | `Snapshot` | the LRU filesystem backend (feeds the admin page) |

Storage keys are `packages/<normalised-name>/<filename>`. The package prefix is what makes
`DELETE /cache/<package>` a prefix delete rather than a second index.

Serving a cached file whose backend can name a local path hands that path to `c.File`, so `net/http`
does range and conditional-request handling. This is **not** a kernel-level zero copy: gin's response
writer exposes neither the file nor the `io.ReaderFrom` hook `net/http` needs to skip user space, so
the bytes still pass through it. The win is delegated correctness, not a saved copy.

### `s3`

The official AWS SDK for Go v2. A seekable body of known size is a single `PutObject`; a live download
of unknown length goes through the SDK's transfer manager, which is the only path that can upload a
stream whose length is not known up front. Only a genuine `404`/`NoSuchKey` is reported as a miss —
denied, throttled and transport failures propagate as errors, so a permissions problem never looks like
an empty cache.

There is no local path, so the backend deliberately does not implement `ZeroCopyCapable`. It also does
not implement `PrefixDeleter`.

### `hybrid`

L1 is authoritative for writes: the download commits to the local store and the request completes.
The L2 upload is queued on a bounded worker pool and is best-effort; the pool is drained on shutdown.
Reads that miss L1 and hit L2 queue an L1 back-fill on the same pool, and the back-fill survives
cancellation of the request that triggered it.

`DeletePrefix` on the tiered backend deletes L1 only. Evicting from the durable tier on an operator's
cache-clear would defeat the point of having one.

## Request flows

### Root index — `GET /simple/`

Byte-level pass-through of the upstream response, nothing decoded and nothing cached. Concurrent
requests for the same `Accept`/`Accept-Encoding` pair share one upstream fetch via singleflight, and the
fetch runs on a context detached from whichever client triggered it, bounded by the client's own
timeout.

### Index resolution — `GET /simple/<package>/`

```
cache hit?  ──yes──▶ serve stored body
    │no
    ▼
singleflight on the package name
    ▼
query indexes: extras concurrently in priority order, primary last
    ▼
first index that has it wins, whole file list
    ▼
build entry (files + JSON + gzip), store, serve
```

A miss from every index is `404`. An error from an index with higher priority than the winner fails the
request instead of falling through. Losing queries are cancelled once a higher-priority index has
answered, which is what `index.result=cancelled` counts.

The upstream client prefers the PEP 691 JSON representation and falls back to parsing HTML, resolving
relative hrefs against the URL actually served and lifting `#sha256=` fragments into the file's hashes.

### Download — `GET /simple/<package>/<file>`

```
storage.Exists ──hit──▶ serve from storage
    │miss
    ▼
resolve index, find the file  ──not listed──▶ 404
    ▼
download timeout 0?  ──yes──▶ 302 upstream
    ▼
singleflight on the storage key
    ├─ leader: stream to client and tee into storage
    └─ waiter: block, then re-plan onto storage (or 302)
```

The tee is one upstream read feeding two sinks: the client's socket and an `io.Pipe` the storage
backend consumes. The bytes are hashed on the way through; the pipe is closed cleanly only if the
digest and length check out, so a file that fails verification is never committed — the backend sees a
pipe error and discards its partial write.

The time-to-first-byte budget is a timer that cancels the request context and is then stopped, so it can
only fire while groxpi is still waiting for upstream response headers. A body already streaming to the
client is never cut off by it.

The leader's download runs on a context detached from its client's request: it is populating the cache
for every waiter, so it must not die with whichever request happened to trigger it.

## Observability

Logs go through the standard library's `log/slog`, fanned out to stdout and — when a collector is
configured — to OpenTelemetry through the `otelslog` bridge, carrying the trace and span identifiers of
the request that produced them. With `OTEL_EXPORTER_OTLP_ENDPOINT` unset, all three signals are inert:
no providers, no connection attempt, no startup dependency.

All three signals leave over OTLP/HTTP to one endpoint. There is **no** Prometheus scrape endpoint and
no `/metrics` route; `monitoring/prometheus.yml` scrapes a collector. See
[adr/0002-otlp-over-prometheus-scrape.md](adr/0002-otlp-over-prometheus-scrape.md).

### Metrics

| Instrument | Kind | Attributes |
|---|---|---|
| `groxpi.cache.hits` | counter | `cache.layer` = `index`, `local`, `remote` |
| `groxpi.cache.misses` | counter | `cache.layer` |
| `groxpi.cache.evictions` | counter | `cache.layer` |
| `groxpi.cache.occupancy` | gauge (bytes) | `cache.layer` |
| `groxpi.index.resolutions` | counter | `index` (redacted URL), `index.result` = `hit`, `miss`, `error`, `cancelled` |
| `groxpi.upstream.fetch.duration` | histogram (seconds) | `fetch.outcome` = `ok`, `error` |
| `groxpi.redirects` | counter | `redirect.reason` = `caching_disabled`, `fetch_failed`, `not_cached` |
| `groxpi.verification.failures` | counter | — |

`groxpi.redirects` is the counter that distinguishes a working cache from a time-to-first-byte budget
set too tight: every increment is a request that was not served from cache.

An index is always identified by its redacted URL, never the configured one — a metric attribute lives
as long as the collector keeps the series.

### Spans

```
{METHOD} {route}
├─ index.resolve
│  └─ index.query          (one per index consulted)
├─ storage.exists
└─ upstream.fetch
   └─ storage.put
```

## Embedded assets

The admin page's templates and its interaction library are compiled into the binary with `go:embed`.
Nothing is read from disk at runtime and there is no asset build step: building groxpi stays a single
`go build`, and the page works in an air-gapped network with no CDN reachable.

The cost is binary size. Measured on darwin/arm64 at this commit: about 48 MB for a plain `go build`,
about 33 MB with `-ldflags="-w -s"` (the Dockerfile strips). Most of that is the OpenTelemetry and AWS
SDKs, not the templates — `html/template` is pulled in by gin whether or not the admin page exists, so
deleting the templates would reclaim close to nothing. Do not "optimize" them away on a size argument.

## Decisions

- [adr/0001-extras-first-index-resolution.md](adr/0001-extras-first-index-resolution.md) — why extra
  indexes are queried first and file lists are never merged.
- [adr/0002-otlp-over-prometheus-scrape.md](adr/0002-otlp-over-prometheus-scrape.md) — why there is no
  scrape endpoint.

## Known gaps

- In pure `s3` mode, `DELETE /cache/<package>` drops the index entry but deletes no objects: the S3
  backend implements no prefix delete. The response still reports success.
- The landing page at `/` reports a hardcoded version string.
