# API

Every route groxpi registers, and nothing else.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/` | — | Landing page: configured index, cache size, index TTL, links. |
| `GET` | `/simple/` | — | Root project list, proxied from upstream (PEP 503/691). |
| `GET` | `/simple/<package>/` | — | File list for one package. |
| `GET` | `/simple/<package>/<file>` | — | Download one distribution file. |
| `GET` | `/simple/<package>/<file>.metadata` | — | PEP 658 core metadata for one distribution, when the index advertises it. |
| `GET` | `/index/` | — | Alias of `/simple/`. |
| `GET` | `/index/<package>/` | — | Alias of `/simple/<package>/`; what pip requests under an `/index/` index URL. |
| `GET` | `/index/<package>` | — | Also an alias of `/simple/<package>/`: no redirect, unlike `/simple/<package>`. |
| `GET` | `/index/<package>/<file>` | — | Alias of `/simple/<package>/<file>`. |
| `GET` | `/health` | — | Liveness and configuration echo. |
| `GET` | `/admin` | basic | Cache administration page. |
| `GET` | `/admin/rows` | basic | HTML fragment: the package table. |
| `GET` | `/admin/inflight` | basic | HTML fragment the page polls: downloads in flight and recent failures. |
| `GET` | `/admin/htmx.min.js` | basic | Embedded interaction library. |
| `POST` | `/admin/prefetch` | basic | Warm the cache for one package. |
| `DELETE` | `/cache/list` | basic | Python-proxpi's list invalidation; evicts a package named `list`. |
| `DELETE` | `/cache/<package>` | basic | Evict one package's index entry and cached files. |

Point pip at `/simple/` (or `/index/`, which exists only for compatibility with the Python
implementation).

The router is the standard library's `http.ServeMux`, and two of its behaviours are part of the API:

- `GET /simple/<package>` without the trailing slash answers `307` with `Location: /simple/<package>/`
  and never consults upstream. `/simple` likewise redirects to `/simple/`.
- Paths are cleaned before matching, so `/index/../etc/passwd` redirects to the cleaned path rather
  than reaching a handler.

## Package names

Names are normalised per PEP 503 — lowercased, runs of `-`, `_` and `.` collapsed to a single `-` —
before anything else happens. `Flask`, `flask` and `FLASK` are one cache entry.

## Content negotiation

`GET /simple/<package>/` answers JSON when the request asks for it, HTML otherwise.

- `?format=` wins outright over `Accept`. Any value containing `json` selects JSON.
- Otherwise the `Accept` header selects JSON only when it contains **both** `application/vnd.pypi.simple`
  and `json`.
- JSON responses carry `Content-Type: application/vnd.pypi.simple.v1+json` and
  `Vary: Accept, Accept-Encoding`; HTML responses carry `Vary: Accept`.
- A client sending `Accept-Encoding: gzip` gets the gzipped body stored alongside the entry in the index
  cache, with `Content-Encoding: gzip`. Nothing is compressed on the request path; there is no
  compression middleware.
- HTML is rendered on demand from the parsed file list, with `data-requires-python` and `data-yanked`
  attributes preserved and every href rewritten to point back at groxpi. File URLs (in HTML and JSON)
  are percent-encoded per path segment, so a `#` in a file name becomes `%23`. Every interpolated value
  (package name, href, file name, requires-python, yanked reason) is HTML-escaped, so `>=3.8` is
  emitted as `&gt;=3.8`.
- PEP 658 metadata availability is passed through as the upstream stated it — `true` or a hash map —
  under the `core-metadata` key only in JSON (PEP 714: servers should not emit the old key in JSON) and
  under both `data-core-metadata` and `data-dist-info-metadata` attributes in HTML. Either upstream
  spelling is accepted. A file the upstream does not mark gets no marker.
- In JSON, every file carries `hashes` (an empty object when the upstream declared none), and a yanked
  file carries `yanked` as the reason string, or `true` when no reason was given (PEP 691).

## The root index

`GET /simple/` is a byte-level pass-through of one of four upstream representations. The client's
headers are not forwarded verbatim; they are normalised first:

- `Accept` becomes `application/vnd.pypi.simple.v1+json` when the request asks for JSON (by the same
  `?format=` / `Accept` rule as a package page above), and `text/html` otherwise.
- `Accept-Encoding` becomes `gzip` when the client's header contains `gzip`, and is not sent otherwise.

The body is returned undecoded, with the upstream status, `Content-Type` and `Content-Encoding`, and the
response carries `Vary: Accept, Accept-Encoding`.

Nothing is cached: the full project list is tens of megabytes, and every representation a client can
ask for is one the upstream already produces. Concurrent requests for the same representation are
coalesced into a single upstream fetch. An upstream failure answers `502`.

The proxied root index is capped at **256 MiB**, and an upstream response above that answers `502`
rather than a truncated list. The whole body is held in memory to serve every client that shared the
fetch, so the cap is what stops an upstream answering this route with an endless body from exhausting
the process. It is a fixed limit with no setting, far above PyPI's own project list, and unrelated to
`GROXPI_INDEX_CACHE_SIZE`, which bounds the per-package index cache and happens to default to a
similar number.

A per-package index body (`GET /simple/<package>/`, JSON or HTML) is capped the same way at
**64 MiB**. A body above it fails the resolution with `500` rather than serving a truncated file list,
and nothing is cached, so the next request asks upstream again. It is also a fixed limit with no
setting.

Because it is a pass-through, the HTML form lists the real upstream projects, not just the ones groxpi
has cached.

## Downloading a file

`GET /simple/<package>/<file>` resolves to one of four outcomes:

1. **Cached** — served from the cache through `net/http`'s `ServeContent`, which brings range requests,
   conditional requests and `Last-Modified` with it. In `hybrid` mode a cache miss that hits S3 is
   copied into the cache while it streams, like a download: that first serve ignores `Range`, carries no
   `Last-Modified`, and takes its `Content-Length` from S3. If that copy cannot start (no spool file
   can be created in the cache), the request answers `500`, not `404`. In pure `s3` mode the `GetObject` is issued
   before any response header is sent; if it fails, the request is handled as a miss (outcome 2 or 3)
   rather than a `200` cut off mid-body.
2. **Stream and cache** — the file is fetched from upstream into a spool file, and the client is sent
   the bytes as they land. Concurrent requests for the same file share that one fetch: every one of them,
   the first included, reads the same growing spool. `Range` is ignored while a download is in flight.
3. **Redirect (`302`)** — the client is sent to the upstream URL. This happens when caching is disabled
   (`GROXPI_DOWNLOAD_TIMEOUT=0`) or when the upstream fetch failed or exceeded its time-to-first-byte
   budget before upstream answered `200`. The `Location` never carries index credentials: user-info is
   stripped from the upstream URL (and a warning logged with the stripped URL), so a private index in
   redirect mode hands clients an upstream `401`, not its password. An upstream URL that does not
   parse is never handed out: that request answers `502` instead.
4. **`404`** — the package's index could not be resolved, or the index does not list that filename.
   A filename that is not a single path segment (empty, containing `/`, `\` or NUL, or starting with
   `.`, so an encoded `..%2F` too) answers `404 File not found` before anything else: no upstream query,
   no cache read, no redirect.

Every downloaded file is verified before it is committed: against the SHA-256 the index declared, and
against the expected length (the declared size, else the upstream `Content-Length`) when one is known.
A file with neither is cached **unverified** and a warning is logged. A file that contradicts either is
not cached. The spool is discarded, and every client streaming it has
its response aborted before the last byte, so no client sees a complete body that failed verification.

Response headers on a download in flight are `Content-Type` (derived from the filename extension), plus
`Content-Length` when the size is known. A cache hit carries `Content-Type`, `Content-Length` and
`Last-Modified`; a `hybrid` S3 hit streams like a download in flight, with `Content-Length` from S3.
No package file response carries an `ETag`.

Package files are never compressed by groxpi — they are already-compressed archives.

### `GET /simple/<package>/<file>.metadata`

The PEP 658 metadata file follows the same four outcomes and the same cache as the distribution. It is
planned from the distribution's index entry: the upstream URL is the file's URL with `.metadata`
appended, the expected SHA-256 is the one the index declared for the metadata (when it declared one),
and the storage key is `packages/<package>/<file>.metadata`. A `.metadata` request for a file the
index lists without a metadata marker, or does not list at all, answers `404`. The response
`Content-Type` is `text/plain; charset=utf-8`.

## `GET /health`

```json
{
  "status": "success",
  "timestamp": 1753574400,
  "data": {
    "cache_dir": "/tmp",
    "index_url": "https://pypi.org/simple/",
    "extra_index_urls": [],
    "cache_size": 5368709120,
    "index_ttl_seconds": 1800,
    "storage_type": "local"
  }
}
```

`timestamp` is Unix seconds. `cache_dir` and `cache_size` describe the cache the storage mode really
uses: `GROXPI_CACHE_DIR`/`GROXPI_CACHE_SIZE` in `local`, `GROXPI_LOCAL_CACHE_DIR`/`GROXPI_LOCAL_CACHE_SIZE`
in `hybrid`, and `""`/`0` in `s3`, which has no cache. The home page's "Cache Size" follows the same
rule. Index URLs are redacted, because this endpoint is unauthenticated and must
not leak an index's credentials.

## Administration

The admin routes exist only when both `GROXPI_ADMIN_USERNAME` and `GROXPI_ADMIN_PASSWORD` are set; see
[configuration.md](configuration.md). Without them, nothing below is registered and every path answers
`404`.

Basic authentication transmits the credentials in cleartext. Terminate TLS in front of groxpi — see
[deployment.md](deployment.md).

Every admin route — including both `DELETE /cache/*` routes — also rejects requests a browser reports as
cross-site. The check reads the `Sec-Fetch-Site` request header: `same-origin` and `none` (address-bar
navigation) proceed, anything else (`cross-site`, `same-site`) answers `403` before the handler runs.
Browsers resend cached basic-auth credentials on a cross-site form post, so the credential alone does not
prove the operator intended the request.

**Stated limitation:** when the header is absent the request proceeds. That keeps curl, scripts and other
non-browser clients working, and it means browsers too old to send fetch metadata are unprotected. This is
deliberate: the threat is a browser, and every browser capable of mounting the attack sends the header. No
token, session or cookie is involved.

### `GET /admin`

An HTML page listing what the local cache holds: packages, their files, sizes, ages and last-accessed times, plus
the 20 most recent prefetch failures. Above the table, a "Downloading now" section lists every upstream
download in progress — package, file, bytes so far against the declared size, age, and how many client
requests are coalesced onto it — and disappears when nothing is in flight.

Only the in-flight section polls: every 3s while the tab is visible the page fetches
`GET /admin/inflight`, which renders the downloads and failures from memory and never walks the cache.
The page sends `?shown=N`, the number of download rows it is showing; when fewer are in flight now, a
download has finished and the reply carries `HX-Trigger: cache-changed`. The package table,
`GET /admin/rows`, walks the cache, so it reloads only on that event, a filter change, the Refresh
button, or an eviction from the page.

`/admin` and `/admin/rows` accept `?page=N` (1-based; missing or
malformed means 1): packages are listed in name order, 50 per page, with a package's files never split
across pages, and the table keeps the page it was rendered for. All three also accept `?q=`, a substring
of the package name, trimmed and normalised like a package name (so `Typing_Ext` matches `typing-ext`),
applied to every cached package before paging and to the "Downloading now" rows. The page's filter box
sends it, with the page, on every reload and poll, reloading the table as the operator types and starting
again at page 1; it shows the filter as typed (trimmed), not its normalised form. Without JavaScript, submitting the box
reloads the page with it, starting at page 1. The pager links keep it. The listing shows the local cache only, so in pure
`s3` mode, which has no cache, the table is empty.

### `POST /admin/prefetch`

Form-encoded, field `package`. Answers `202 Accepted` immediately and downloads on a context detached
from the request, so a large package cannot be cut off by a reverse-proxy timeout. There is no job ID:
progress is the files appearing in the table. A package whose prefetch is still running is accepted
again but not queued: one prefetch per package runs at a time.

Prefetch downloads the **newest final release only** — yanked files and pre-releases are excluded,
versions are compared under PEP 440 ordering, and all files of the winning release (wheels plus sdist)
are fetched. A filename whose version will not parse is skipped. An empty `package` field answers `400`.

Once shutdown has begun the route answers `503` instead of accepting: the download could not have
finished, and registering it then would race the shutdown drain. Prefetches already running finish
their cache writes before storage is released, but only within the shutdown budget — one still running
when the budget is spent is abandoned with a warning, so a wedged upstream cannot hold the process open
until it is killed. See [deployment](deployment.md) for the budget and how to size the stop grace.

### `DELETE /cache/list`

Python proxpi's "invalidate the package list". The root index is proxied rather than cached, so there
is no list to invalidate; the route is the package route below for a package named `list`, so it evicts
that package's index entry and cached files (if any) and answers as that route does —
`{"status":"success","data":null}` when nothing is cached. Python-proxpi clients keep working, and a
package called `list` can still be evicted.

### `DELETE /cache/<package>`

Drops the package's index cache entry **and deletes its cached files**. Answers
`{"status":"success","data":null}`, `400`
(`{"status":"error","message":"Invalid package name"}`) for a name that is not a single path segment
(containing `/`, `\` or NUL, or starting with `.`, after normalisation), in which case nothing is
deleted and the index entry is kept, or `500` if the deletion failed.

In `hybrid` mode both the cached copies and the S3 objects are deleted, so the next request cannot promote
the package back from S3. In pure `s3` mode the objects under the package's prefix are deleted.

> **Breaking change, and deliberate.** Both `DELETE /cache/*` routes required no authentication in the
> Python implementation and in earlier groxpi releases. They now sit inside the admin group, so **in the
> default configuration — no `GROXPI_ADMIN_USERNAME`, no `GROXPI_ADMIN_PASSWORD` — both answer `404`, not
> `200`.** `DELETE /cache/list` is kept for Python-proxpi compatibility, but that compatibility only
> exists once admin credentials are configured. Setting both variables is the whole fix; callers then
> send basic auth. This is not a defect to "restore": authenticating the whole group was chosen after the
> compatibility route was kept, in full knowledge that it breaks unauthenticated callers.

## Errors

| Status | When |
|---|---|
| `302` | Redirect to upstream for a file groxpi will not serve itself. |
| `307` | Trailing-slash or path-cleaning redirect issued by the router. |
| `400` | Missing required parameter on an admin route, or an invalid package name on `DELETE /cache/<package>`. |
| `401` | Missing or wrong basic-auth credentials on an admin route. |
| `403` | An admin route reached with a `Sec-Fetch-Site` header reporting a cross-site request. |
| `404` | Unknown path, unknown package, unknown file, or an admin route that is not configured. On a file route (`/simple/<package>/<file>`, `/index/<package>/<file>`) also any index resolution failure, absence or not (`Package not found`). |
| `405` | Known path reached with the wrong method — the response carries `Allow`. |
| `500` | On a package index route (`/simple/<package>/`, `/index/<package>/`, `/index/<package>`), index resolution failed for a reason other than absence (including an index body over the 64 MiB cap); or a storage operation failed. |
| `502` | The upstream root index could not be fetched, or exceeded the 256 MiB cap; or a file to redirect has an upstream URL that does not parse. |
| `503` | A prefetch was submitted after shutdown had begun. |

A known path with the wrong method answers `405`, not `404`: `DELETE /simple/` tells you the method is
wrong rather than that the path does not exist.
