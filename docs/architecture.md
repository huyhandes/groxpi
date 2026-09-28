# Architecture

groxpi is a caching PyPI proxy built on the standard library's `net/http`: one `server` module owning
every route on one `http.ServeMux`, over plain Go services and a read-through storage cache. There is no
web framework. See [adr/0003-stdlib-mux-and-module-boundaries.md](adr/0003-stdlib-mux-and-module-boundaries.md)
and [adr/0004-read-through-storage-and-server-owned-http.md](adr/0004-read-through-storage-and-server-owned-http.md).

## Module map

`server` owns every route and handler. Every other module is deep: a few plain Go methods over
everything it needs to own, with no HTTP handler code. Modules talk to each other through those
methods, never through each other's internals.

```
cmd/groxpi/            main: config, telemetry, logger, server, graceful shutdown
internal/
  server/    server.go       composition root: builds storage, index, download, admin; the mux;
                             serves GET /simple/{package}/{file} (and its /index/ alias);
                             wraps the mux in panic recovery and request tracing; shutdown
             index.go        GET /simple/, GET /simple/{package}/ and the /index/ aliases
             admin.go        basic auth, cross-site check, admin templates, page, rows, prefetch,
                             cache eviction routes
  index/     service.go      Resolve / Invalidate / ProxyRoot: extras-first resolution, singleflight
             cache.go        bounded index cache (LRU by last access + background TTL sweep)
             client.go       upstream index client (JSON first, HTML fallback)
             normalize.go    PEP 503 name normalisation
  download/  service.go      Resolve / Fetch: find a file in the index (PEP 658 included), open it upstream
  admin/     admin.go        List / Evict / InFlight: the cache listing and eviction
             prefetch.go     Prefetch and newest-release selection, built on storage.Open
  storage/   store.go        read-through Open: coalesced, verified, spooled downloads; the cache
                             and/or the durable store; InFlight
             local.go        the local cache; evicts by atime (the filesystem is the LRU index)
             atime_*.go      per-OS atime read
             s3.go           the S3 durable store (AWS SDK v2)
  config/    config.go       every environment variable, URL redaction, index resolution order
  logger/    logger.go       slog setup: stdout handler plus the OpenTelemetry log bridge
  telemetry/ telemetry.go    OTLP providers for traces, metrics and logs
             instruments.go  the metrics groxpi records
templates/                   admin.html, rows.html, htmx.min.js — embedded with go:embed
monitoring/prometheus.yml    scrapes an OpenTelemetry collector, never groxpi
benchmarks/                  benchmark harness (see benchmarking.md)
docs/adr/                    architecture decision records
```

Dependencies point one way: `server → admin → storage → download → index`. `download` needs one thing
from `index` — a resolved file list — and takes the concrete `*index.Service`; its tests fake upstream
over HTTP. `storage` needs one thing from `download` — an open upstream body — and asks for it through
a one-method interface it defines itself (`storage.Fetcher`), the one interface a module depends on
another through, so its tests run against a fake fetcher. Everywhere else a module is used as the
concrete type it is: an interface with one implementation is a liability, not a boundary.

Routing is the standard library's `http.ServeMux` with method-and-pattern routes (`GET
/simple/{package}/{$}`). The mux supplies the trailing-slash redirect, the `405` with `Allow`, and the
matched pattern (`r.Pattern`) that names each request span. JSON is the standard library's
`encoding/json`.

## The index cache

One cache, one byte budget. Each entry holds, for one package: the parsed file list, the JSON response
body, and the gzipped JSON response body. All three are accounted against `GROXPI_INDEX_CACHE_SIZE`.
Entries leave by TTL — a background sweep, so an expired entry's memory is actually reclaimed rather
than waiting for the next lookup — or by LRU on last access when the budget is exceeded.

Producing the serialised bodies at fill time is what removes the separate response cache: the request
path never marshals and never compresses. A client that accepts gzip is handed bytes that were
compressed once, when the entry was built.

## The cache and the durable store

groxpi keeps package files in up to two places, with different jobs:

- **The cache** is the local filesystem: `GROXPI_CACHE_DIR` bounded by `GROXPI_CACHE_SIZE` in
  `local` mode, `GROXPI_LOCAL_CACHE_DIR` bounded by `GROXPI_LOCAL_CACHE_SIZE` in `hybrid` mode, where
  `GROXPI_LOCAL_CACHE_TTL` can also expire files (there is no TTL in `local` mode). It evicts by
  least-recent atime. It is disposable: losing it costs re-fetches, nothing else. Only the cache evicts.
- **The durable store** is an S3 bucket. Every verified download lands there and stays; groxpi never
  expires or evicts an object from it. Retention is the operator's job, through bucket lifecycle rules.

`GROXPI_STORAGE_TYPE` picks which exist: `local` is the cache only, `s3` is the durable store only, and
`hybrid` is the cache in front of the durable store. There is no backend interface: the `Store` holds a
concrete local cache and/or a concrete S3 store, and `hybrid` is "cache, then S3" logic in the `Store`.

Storage keys are `packages/<normalised-name>/<filename>`, in the cache and in the bucket alike. The
package prefix is what makes `DELETE /cache/<package>` a prefix delete rather than a second index.
Eviction deletes the package from the cache and from the durable store; otherwise the next request
would restore it from S3. The admin listing shows the cache only.

Serving a stored file hands the open object to `http.ServeContent`,
so `net/http` does range and conditional-request handling. The tracing middleware's response-writer
wrapper forwards `io.ReaderFrom` and `Unwrap`, so the standard writer's own copy path is preserved.
Whether that ever avoids a user-space copy depends on the platform and is neither claimed nor measured
here: the win groxpi relies on is delegated correctness.

### The durable store (`s3`)

The official AWS SDK for Go v2. Every upload is a finished, verified file of known size, so it is a
single `PutObject`; an object is therefore limited to S3's single-PUT maximum of 5 GiB, which no PyPI
file approaches. Only a genuine `404`/`NoSuchKey` is reported as a miss — denied, throttled and
transport failures propagate as errors, so a permissions problem never looks like an empty store.

A pure `s3` hit issues its `GetObject` before any response header is sent. If that request fails, the
file is treated as a miss — fetched upstream, or redirected in redirect mode — rather than answered
with a `200` that is cut off mid-body. `Range` and conditional requests still go through
`http.ServeContent`; a ranged read fetches bounded windows, starting at 1 MiB and doubling up to a cap,
rather than an open-ended `bytes=<off>-`. An S3 hit carries no `ETag`.

### `hybrid`

The write path of a download: spool under the cache directory, verify (SHA-256 and length), rename
into the cache. At that point every reader is done or can finish from the cache, and the next request
is a cache hit. The same download goroutine then uploads the finished file to S3, synchronously —
there is no queue and nothing is dropped. A failed upload is logged; the file is still served and
cached. Shutdown waits, within the shutdown timeout, for downloads and their uploads to finish.

A read that misses the cache and hits S3 is a promotion: the `Store` opens the S3 body and streams it
through the same flight, spool and tail as an upstream download, so it lands in the cache checked
against the S3 object's length and every concurrent reader tails it. It runs detached, so it survives
the request that triggered it. A promoted file is not uploaded back — it is already in S3.

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
relative hrefs against the URL actually served, lifting `#sha256=` fragments into the file's hashes and
`data-core-metadata` / `data-dist-info-metadata` attributes into the PEP 658 marker.

### Download — `GET /simple/<package>/<file>`

The server hands every package-file request to `storage.Open`. A hit in the cache (or, for pure `s3`,
in the durable store) comes back as a seekable object and goes to `http.ServeContent`. A pure `s3` hit
opens its `GetObject` before response headers are sent and reads ranges in bounded windows (see
[the durable store](#the-durable-store-s3)). In `hybrid`, a cache miss with an S3 hit is promoted into
the cache through the same spool path as a download, with no upstream fetch.

On a miss, `storage` keeps one flight per storage key. The first request starts it: a detached leader
calls `download.Fetch`, which resolves the file through the index (a `<file>.metadata` name resolves
from `<file>`'s entry, PEP 658) and opens it upstream. The leader copies the body into a `.tmp-*` spool
file (next to its final path locally, under `GROXPI_CACHE_DIR/spool` for `s3`) while hashing it, through
one pooled copy buffer. Every request for that key, the first included, tails the growing spool; Range
is ignored while in flight. Readers are held one byte short of the expected length (or of the bytes
written, when no length is known) until verification: against the SHA-256 the index declared, and
against the expected length (the declared size, else the upstream `Content-Length`) when one is known;
a file with neither is cached unverified with a warning. A verified spool is
renamed into the cache (`local`, `hybrid`; in `hybrid` the same goroutine then uploads it to S3) or
uploaded and removed (`s3`). A spool that fails verification is deleted and every reader's body read fails, so
the server aborts each response rather than end it cleanly; only a fetch that fails before upstream
answers `200` (or whose spool cannot be created) is redirected upstream.

The flight is visible: `storage.InFlight` lists each running download with the bytes written so far,
the size the index declared, when it started and how many requests are coalesced onto it. The admin
page renders it as "Downloading now" rows and `groxpi.download.inflight` reports its size.

With `GROXPI_DOWNLOAD_TIMEOUT=0`, a miss is not fetched: the server resolves the file and answers `302`
to the upstream URL. A fetch that fails before upstream answers `200` also ends in a `302`, counted as
`fetch_failed`. A file the index does not list is a `404`.

The time-to-first-byte budget is a timer that cancels the request context and is then stopped, so it can
only fire while groxpi is still waiting for upstream response headers. A body already streaming is never
cut off by it.

The leader runs on a context detached from its client's request: it fills the cache for every reader,
so it finishes even after all of them have left. Shutdown waits for it.

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
| `groxpi.redirects` | counter | `redirect.reason` = `caching_disabled`, `fetch_failed` |
| `groxpi.verification.failures` | counter | — |
| `groxpi.download.inflight` | gauge | — |

`groxpi.redirects` is the counter that distinguishes a working cache from a time-to-first-byte budget
set too tight: every increment is a request that was not served from cache.

An index is always identified by its redacted URL, never the configured one — a metric attribute lives
as long as the collector keeps the series.

### Spans

```
{METHOD} {pattern}           e.g. GET /simple/{package}/{file}
├─ storage.exists
├─ index.resolve
│  └─ index.query          (one per index consulted)
└─ upstream.fetch
   └─ storage.put
```

## Embedded assets

The admin page's templates and its interaction library are compiled into the binary with `go:embed`.
Nothing is read from disk at runtime and there is no asset build step: building groxpi stays a single
`go build`, and the page works in an air-gapped network with no CDN reachable.

The cost is binary size, and the Dockerfile strips with `-ldflags="-w -s"`. What dominates that size is
the OpenTelemetry and AWS SDKs, not the templates — `html/template` and the embedded assets are a
rounding error against them, so deleting the templates would reclaim close to nothing. Do not
"optimize" them away on a size argument. Measure before quoting a number here.

## Decisions

- [adr/0001-extras-first-index-resolution.md](adr/0001-extras-first-index-resolution.md) — why extra
  indexes are queried first and file lists are never merged.
- [adr/0002-otlp-over-prometheus-scrape.md](adr/0002-otlp-over-prometheus-scrape.md) — why there is no
  scrape endpoint.
- [adr/0003-stdlib-mux-and-module-boundaries.md](adr/0003-stdlib-mux-and-module-boundaries.md) — why
  there is no web framework (its module layout is superseded by 0004).
- [adr/0004-read-through-storage-and-server-owned-http.md](adr/0004-read-through-storage-and-server-owned-http.md)
  — why the server owns all HTTP and storage is a read-through cache.

## Known gaps

Open limits, each with a proposed fix and check, are in [backlog.md](backlog.md).
