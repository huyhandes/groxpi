# API

Every route groxpi registers, and nothing else.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/` | — | Landing page: configured index, cache size, index TTL, links. |
| `GET` | `/simple/` | — | Root project list, proxied from upstream (PEP 503/691). |
| `GET` | `/simple/<package>/` | — | File list for one package. |
| `GET` | `/simple/<package>/<file>` | — | Download one distribution file. |
| `GET` | `/index/` | — | Alias of `/simple/`. |
| `GET` | `/index/<package>` | — | Alias of `/simple/<package>/` (note: no trailing slash). |
| `GET` | `/index/<package>/<file>` | — | Alias of `/simple/<package>/<file>`. |
| `GET` | `/health` | — | Liveness and configuration echo. |
| `GET` | `/admin` | basic | Cache administration page. |
| `GET` | `/admin/rows` | basic | HTML fragment the page polls. |
| `GET` | `/admin/htmx.min.js` | basic | Embedded interaction library. |
| `POST` | `/admin/prefetch` | basic | Warm the cache for one package. |
| `DELETE` | `/cache/list` | basic | No-op, kept for Python-proxpi compatibility. |
| `DELETE` | `/cache/<package>` | basic | Evict one package's index entry and cached files. |

Point pip at `/simple/` (or `/index/`, which exists only for compatibility with the Python
implementation).

## Package names

Names are normalised per PEP 503 — lowercased, runs of `-`, `_` and `.` collapsed to a single `-` —
before anything else happens. `Flask`, `flask` and `FLASK` are one cache entry.

## Content negotiation

`GET /simple/<package>/` answers JSON when the request asks for it, HTML otherwise.

- `?format=` wins outright over `Accept`. Any value containing `json` selects JSON.
- Otherwise the `Accept` header selects JSON only when it contains **both** `application/vnd.pypi.simple`
  and `json`.
- JSON responses carry `Content-Type: application/vnd.pypi.simple.v1+json` and `Vary: Accept-Encoding`.
- A client sending `Accept-Encoding: gzip` gets the gzipped body stored alongside the entry in the index
  cache, with `Content-Encoding: gzip`. Nothing is compressed on the request path; there is no
  compression middleware.
- HTML is rendered on demand from the parsed file list, with `data-requires-python` and `data-yanked`
  attributes preserved and every href rewritten to point back at groxpi.

## The root index

`GET /simple/` is a byte-level pass-through. The client's `Accept` and `Accept-Encoding` are forwarded
upstream and the body is returned undecoded, with the upstream status, `Content-Type` and
`Content-Encoding`. `?format=` overrides `Accept` here too, and the response carries
`Vary: Accept, Accept-Encoding`.

Nothing is cached: the full project list is tens of megabytes, and every representation a client can
ask for is one the upstream already produces. Concurrent requests for the same representation are
coalesced into a single upstream fetch. An upstream failure answers `502`.

The proxied root index is capped at **256 MiB**, and an upstream response above that answers `502`
rather than a truncated list. The whole body is held in memory to serve every client that shared the
fetch, so the cap is what stops an upstream answering this route with an endless body from exhausting
the process. It is a fixed limit with no setting, far above PyPI's own project list, and unrelated to
`GROXPI_INDEX_CACHE_SIZE`, which bounds the per-package index cache and happens to default to a
similar number.

Because it is a pass-through, the HTML form lists the real upstream projects, not just the ones groxpi
has cached.

## Downloading a file

`GET /simple/<package>/<file>` resolves to one of four outcomes:

1. **Cached** — served from the object store. When the backend can name a local file, the path is handed
   to `net/http`, which brings range requests and conditional requests with it.
2. **Stream and cache** — the file is fetched from upstream and streamed to the client while the same
   bytes are written into the cache. Concurrent requests for the same file are deduplicated: one request
   streams, and the others are served the freshly cached object once it lands.
3. **Redirect (`302`)** — the client is sent to the upstream URL. This happens when caching is disabled
   (`GROXPI_DOWNLOAD_TIMEOUT=0`), when the upstream fetch failed or exceeded its time-to-first-byte
   budget, or when a coordinated download left nothing cached.
4. **`404`** — the package's index could not be resolved, or the index does not list that filename.

Every cached file is verified before it is committed: against the SHA-256 the index declared, or failing
that against the declared content length. A file that matches neither is cached **unverified** and a
warning is logged. A file that contradicts either is not cached at all and the partial write is
discarded.

Response headers on a download are `Content-Type` (derived from the filename extension), `Content-Length`
and `ETag` (the SHA-256, quoted) when the index supplied them. Files served from storage additionally
carry `Content-Disposition: attachment` and `Cache-Control: public, max-age=3600`.

Package files are never compressed by groxpi — they are already-compressed archives.

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

`timestamp` is Unix seconds. Index URLs are redacted, because this endpoint is unauthenticated and must
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

An HTML page listing what the local cache holds: packages, their files, sizes, hit counts and ages, plus
the 20 most recent prefetch failures. The table polls `GET /admin/rows` for updates. Only backends that
hold real local files can be listed, so in pure `s3` mode the table is empty.

### `POST /admin/prefetch`

Form-encoded, field `package`. Answers `202 Accepted` immediately and downloads on a context detached
from the request, so a large package cannot be cut off by a reverse-proxy timeout. There is no job ID:
progress is the files appearing in the table.

Prefetch downloads the **newest final release only** — yanked files and pre-releases are excluded,
versions are compared under PEP 440 ordering, and all files of the winning release (wheels plus sdist)
are fetched. A filename whose version will not parse is skipped. An empty `package` field answers `400`.

Once shutdown has begun the route answers `503` instead of accepting: the download could not have
finished, and registering it then would race the shutdown drain. Prefetches already running finish
their cache writes before storage is released, but only within the shutdown budget — one still running
when the budget is spent is abandoned with a warning, so a wedged upstream cannot hold the process open
until it is killed. See [deployment](deployment.md) for the budget and how to size the stop grace.

### `DELETE /cache/list`

Answers `{"status":"success","data":null}` and does nothing. The root index is proxied rather than
cached, so there is no cached list to invalidate. The route exists so Python-proxpi clients keep working.

### `DELETE /cache/<package>`

Drops the package's index cache entry **and deletes its cached files**. Answers
`{"status":"success","data":null}`, `400` for an empty package name, or `500` if the deletion failed.

In `hybrid` mode only the local L1 copies are deleted; the objects stay in S3 by design. In pure `s3`
mode no files are deleted — see the note in [architecture.md](architecture.md).

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
| `400` | Missing required parameter on an admin route. |
| `401` | Missing or wrong basic-auth credentials on an admin route. |
| `403` | An admin route reached with a `Sec-Fetch-Site` header reporting a cross-site request. |
| `404` | Unknown path, unknown package, unknown file, or an admin route that is not configured. |
| `405` | Known path reached with the wrong method — the response carries `Allow`. |
| `500` | Index resolution failed for a reason other than absence, or a storage operation failed. |
| `502` | The upstream root index could not be fetched, or exceeded the 256 MiB cap. |
| `503` | A prefetch was submitted after shutdown had begun. |

A known path with the wrong method answers `405`, not `404`: `DELETE /simple/` tells you the method is
wrong rather than that the path does not exist.
