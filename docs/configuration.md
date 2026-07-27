# Configuration

groxpi is configured entirely through environment variables. Every setting below exists in
`internal/config/config.go`; nothing else is read.

Durations are in **seconds**. The timeout settings accept a decimal fraction; the others are integers.

## Index

| Variable | Default | Meaning |
|---|---|---|
| `GROXPI_INDEX_URL` | `https://pypi.org/simple/` | Primary upstream index. |
| `GROXPI_INDEX_TTL` | `1800` | How long the primary index's answers stay cached. |
| `GROXPI_EXTRA_INDEX_URLS` | *(empty)* | Comma-separated additional indexes, highest priority first. |
| `GROXPI_EXTRA_INDEX_TTLS` | `180` per index | Comma-separated TTLs, positionally matched to `GROXPI_EXTRA_INDEX_URLS`. A missing or unparseable entry falls back to 180. |
| `GROXPI_DISABLE_INDEX_SSL_VERIFICATION` | `false` | Skip TLS certificate verification when talking to an index. |

### How extra indexes are resolved

Extra indexes are queried **first**, in configured order, and the primary index **last**. The first
index that has the package wins and its file list is used **whole** — file lists from different
indexes are never merged. See
[adr/0001-extras-first-index-resolution.md](adr/0001-extras-first-index-resolution.md) for why.

Queries run concurrently, but the answer is chosen by priority, not by who replied first. If a
higher-priority index *errors* — as opposed to reporting the package absent — the request fails rather
than falling through to a lower-priority index: a private index that is down must not silently resolve
to the public one.

Each index carries its own TTL, so a fast-moving private index can be refreshed more often than PyPI.

Credentials may be embedded in an index URL (`https://user:pass@host/simple/`). They are redacted to
`https://redacted@host/simple/` in every log record, error message, metric attribute and HTTP response
body. An unparseable URL is reported as `[unparseable-url]` rather than risk leaking part of it.

> **Behavioural change on upgrade.** Extra indexes previously did not participate in package
> resolution. With `GROXPI_EXTRA_INDEX_URLS` configured, packages that used to resolve to PyPI may now
> resolve to an extra index. Review the order before upgrading.

## Caches

| Variable | Default | Meaning |
|---|---|---|
| `GROXPI_CACHE_SIZE` | `5368709120` (5 GB) | Byte budget for the on-disk package file cache. LRU eviction. |
| `GROXPI_CACHE_DIR` | the OS temp directory | Where cached package files live (`local` and `hybrid` modes). |
| `GROXPI_INDEX_CACHE_SIZE` | `268435456` (256 MB) | Byte budget for the in-memory index cache. |

There is one index cache. Per package it holds the parsed file list plus the JSON and gzipped-JSON
response bodies, and accounts all of them against the byte budget. Entries leave by TTL (a background
sweep) or by LRU when the budget is exceeded. There is no separate response cache.

> The 256 MB default is **provisional**. It was chosen without measurement and cannot be tuned honestly
> until the cache metrics have been observed against real traffic. Watch `groxpi.cache.occupancy` and
> `groxpi.cache.evictions` with `cache.layer=index` before changing it.

## Storage

| Variable | Default | Meaning |
|---|---|---|
| `GROXPI_STORAGE_TYPE` | `local` | `local`, `s3`, or `hybrid`. |

### `local`

Files live under `GROXPI_CACHE_DIR`, bounded by `GROXPI_CACHE_SIZE` with LRU eviction.

### `s3`

| Variable | Default | Meaning |
|---|---|---|
| `GROXPI_S3_BUCKET` | *(none — required)* | Bucket. Startup fails if unset in `s3` or `hybrid` mode. |
| `GROXPI_S3_PREFIX` | `groxpi` | Key prefix inside the bucket. |
| `AWS_ENDPOINT_URL` | *(unset — the SDK's own endpoint for the region)* | Endpoint override, for MinIO and other S3-compatible services. An explicit scheme here overrides `GROXPI_S3_USE_SSL`. |
| `AWS_REGION` | `us-east-1` | Region. |
| `AWS_ACCESS_KEY_ID` | *(unset)* | Optional static access key. |
| `AWS_SECRET_ACCESS_KEY` | *(unset)* | Optional static secret key. |
| `GROXPI_S3_USE_SSL` | `true` | Use HTTPS when the endpoint carries no scheme. |
| `GROXPI_S3_FORCE_PATH_STYLE` | `false` | Path-style addressing. Required for MinIO. |
| `GROXPI_S3_ENABLE_HTTP2` | `true` | Allow HTTP/2 to the endpoint. |

The backend is the official AWS SDK for Go v2. **Static credentials are not required**: with none
configured the SDK's default credential chain applies — environment, shared config file, container
credentials, web identity, instance metadata — which is how instance and task roles work. Static keys,
when both are set, take precedence over the chain.

groxpi probes the bucket with `HeadBucket` at startup and refuses to start if it is unreachable. See
[deployment.md](deployment.md) for the IAM-role path and the permissions the bucket policy needs.

For MinIO set `GROXPI_S3_FORCE_PATH_STYLE=true`: MinIO cannot resolve virtual-hosted bucket names.
Checksum calculation and validation are configured as *when required* rather than always, because
older MinIO releases reject the checksum trailers the SDK emits by default.

### `hybrid`

A local L1 cache in front of an S3 L2. Every `s3` setting applies, plus:

| Variable | Default | Meaning |
|---|---|---|
| `GROXPI_LOCAL_CACHE_SIZE` | `10737418240` (10 GB) | Byte budget for L1. |
| `GROXPI_LOCAL_CACHE_DIR` | value of `GROXPI_CACHE_DIR` | Where L1 lives. |
| `GROXPI_LOCAL_CACHE_TTL` | `0` (disabled) | Age at which an L1 entry is dropped regardless of the byte budget. |
| `GROXPI_TIERED_SYNC_WORKERS` | `5` | Workers that back-fill L1 from L2 and upload L1 to L2. |
| `GROXPI_TIERED_SYNC_QUEUE_SIZE` | `100` | Depth of the sync queue. |

Writes are **local-first**: a download is committed to L1 and the request completes there. The upload
to L2 is queued on the worker pool and is best-effort — a failed upload is logged, not surfaced to the
client. The queue is drained on shutdown, bounded at 30 seconds, so a normal restart does not lose
pending uploads. A `SIGKILL` can.

## Timeouts

| Variable | Default | Meaning |
|---|---|---|
| `GROXPI_DOWNLOAD_TIMEOUT` | `0.9` | Time-to-first-byte budget for an upstream package file. |
| `GROXPI_CONNECT_TIMEOUT` | `0` (unset) | Summed with the next into one whole-request timeout for the index client. Also the S3 backend's setup deadline. |
| `GROXPI_READ_TIMEOUT` | `0` (unset) | Summed with the previous. Neither is a per-phase budget — read the note below before setting either. |

`GROXPI_DOWNLOAD_TIMEOUT` bounds only the wait for the upstream **response headers**. Once headers
arrive, the body streams to completion under the request's own lifetime — a transfer already flowing to
the client is never cut off by a budget meant for connection setup. If the budget expires first, the
client is redirected to the upstream URL and nothing is cached.

Set it to `0` to disable caching of package files altogether: every download becomes a redirect
upstream. That is what `redirect.reason=caching_disabled` counts.

`GROXPI_CONNECT_TIMEOUT` and `GROXPI_READ_TIMEOUT` apply to the index HTTP client, not to package
downloads. They are **summed into one `http.Client.Timeout`**, which covers the whole request —
connection setup, headers *and* body. Neither name means what it says:

- Setting only `GROXPI_CONNECT_TIMEOUT=3` caps the entire index request at 3 seconds, body included. A
  slow-to-transfer index answer fails as if it had failed to connect.
- With both unset the client keeps its built-in 60-second total timeout. `0` does not mean "no limit".
- To allow a long transfer, set both, and set the sum to what the whole request may take.

`GROXPI_CONNECT_TIMEOUT` is reused as the S3 backend's dial, TLS-handshake and credential-load budget
(default 10 seconds there), and `GROXPI_DOWNLOAD_TIMEOUT` as its response-header budget — so the `0.9`
default also bounds how long S3 may take to start answering.

## Server

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `5000` | Listen port. groxpi always binds all interfaces. |
| `GROXPI_LOGGING_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, `ERROR`. |
| `GROXPI_LOG_FORMAT` | `console` | `console` or `json`. |
| `GROXPI_LOG_COLOR` | `true` | ANSI colour in `console` format. |

## Observability

| Variable | Default | Meaning |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(empty)* | Collector base URL, e.g. `http://otel-collector:4318`. |
| `OTEL_SERVICE_NAME` | `groxpi` | Service name on exported telemetry. |

With no endpoint configured, telemetry is inert: no providers installed, no connection attempted,
instrumentation costs nothing. groxpi serves no Prometheus scrape endpoint — see
[adr/0002-otlp-over-prometheus-scrape.md](adr/0002-otlp-over-prometheus-scrape.md).

## Administrative interface

| Variable | Default | Meaning |
|---|---|---|
| `GROXPI_ADMIN_USERNAME` | *(unset)* | Basic-auth user. |
| `GROXPI_ADMIN_PASSWORD` | *(unset)* | Basic-auth password. |
| `GROXPI_ADMIN_ENABLED` | `false` | Assert that the surface is wanted. |

Credentials are the switch: with **both** set the admin routes are mounted; with either missing they
are not registered at all and answer 404. `GROXPI_ADMIN_ENABLED=true` without both credentials is a
**startup failure** — it turns a silent misconfiguration into a loud one.

Basic authentication sends credentials in cleartext, so any deployment exposing this surface needs a
TLS-terminating proxy in front of it. See [deployment.md](deployment.md).

The eviction routes `DELETE /cache/list` and `DELETE /cache/<package>` live inside this group.
**Breaking change**: they were unauthenticated in the Python implementation and in earlier groxpi
releases. They now require credentials, and 404 when none are configured. Scripts that call them must
be updated.

## Settings inherited from Python proxpi

groxpi reads the same variable names as proxpi where the setting still exists: `GROXPI_INDEX_URL`,
`GROXPI_INDEX_TTL`, `GROXPI_EXTRA_INDEX_URLS`, `GROXPI_EXTRA_INDEX_TTLS`, `GROXPI_CACHE_SIZE`,
`GROXPI_CACHE_DIR`, `GROXPI_DOWNLOAD_TIMEOUT`, `GROXPI_CONNECT_TIMEOUT`, `GROXPI_READ_TIMEOUT`,
`GROXPI_LOGGING_LEVEL`, `GROXPI_DISABLE_INDEX_SSL_VERIFICATION`. `PROXPI_`-prefixed spellings are not
read.

`GROXPI_BINARY_FILE_MIME_TYPE` does not exist. A cached file's content type comes from what upstream
sent, or from the filename extension.
