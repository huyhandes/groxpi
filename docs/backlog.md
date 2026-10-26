# Backlog

Known limits of the current code, each small enough to pick up on its own. None is a correctness bug
under normal operation; each names where it lives, what goes wrong, a proposed fix, and the check
that proves it. Delete an entry when it ships.

## Storage

### A file can be fetched and uploaded twice

- **Where:** `internal/storage/store.go` — `Store.commitHybrid`.
- **What:** a request that missed a flight committing just before it leads re-checks once and serves
  the commit. One window remains in `hybrid` mode: the flight unregisters after the cache rename and
  before its S3 upload lands, so if the cache commit failed (or the LRU evicted the file) a request in
  that window misses both tiers and starts a second download and a second S3 upload. The result is
  correct; the cost is one extra upstream fetch and PUT.
- **Fix:** when the cache commit fails, keep the flight registered until the upload finishes, as
  `commitS3` does.
- **Check:** a hybrid store test that fails the cache commit, gates the fake S3 PUT, opens the file
  again, and counts fetcher calls and bucket PUTs (both 1).

### HEAD and conditional requests on pure-s3 hits cost a full GetObject

- **Where:** `internal/storage/store.go` — `Store.lookup`, the `s.cache == nil` branch.
- **What:** the lookup opens the whole-object `GetObject` before headers (so a failed GET is a miss,
  not a cut-off 200). A `HEAD`, a `304` revalidation or a ranged request then abandons that body
  unread, which also discards the S3 connection.
- **Fix:** pass the request's method/Range/conditional headers to `Open` as a hint, and open the body
  (or a ranged window) that the response will actually need.
- **Check:** extend `TestStore_S3HitTransfersOnlyTheRange` to assert no unranged GET for a Range
  request and none at all for a HEAD.

### Ranged S3 reads trust the backend to honour Range

- **Where:** `internal/storage/s3.go` — `S3Storage.getRange`.
- **What:** the response's status / `Content-Range` is not checked. A backend that ignored `Range`
  would return the object from byte 0 and the client would get the wrong bytes. AWS S3 and MinIO
  honour it.
- **Fix:** compare `GetObjectOutput.ContentRange` with the requested `off-end` and fail the read on a
  mismatch.
- **Check:** a fake S3 that ignores `Range`; assert the read errors instead of returning wrong bytes.

### A stalled S3 body read has no deadline

- **Where:** `internal/storage/store.go` — the detached context in `Store.lookup`;
  `internal/storage/s3.go` — `newS3Transport`.
- **What:** S3 hit and promotion bodies are read on `context.WithoutCancel`, and the transport only
  bounds the time to response headers. A body that stops mid-stream holds the reader (and, for a
  promotion, the flight) until the TCP connection dies.
- **Fix:** an idle-read timeout on the body (reset on every successful read), failing the read when
  it fires.
- **Check:** a fake S3 that sends half a body then blocks; assert the read fails within the timeout.

### Uploads larger than 5 GiB fail

- **Where:** `internal/storage/s3.go` — `S3Storage.Put` (`ponytail:` comment).
- **What:** the upload is one `PutObject`, which S3 caps at 5 GiB. A larger file is served and cached
  locally but never reaches S3; the failure is logged.
- **Fix:** multipart upload from the spool file (`io.SectionReader` per part) above a threshold.
- **Check:** a fake S3 that accepts multipart; upload a file over a lowered threshold.

### The default cache dir is the shared temp directory

- **Where:** `internal/config/config.go` — the `GROXPI_CACHE_DIR` default (`os.TempDir()`).
- **What:** the bare binary caches under `/tmp/packages`. Another local user can pre-create files
  there, and a cache hit is served without re-verifying its hash, so a planted file is served as the
  package. The Docker image sets `GROXPI_CACHE_DIR=/cache` and is not affected.
- **Fix:** default to `os.UserCacheDir()/groxpi`, falling back to `os.TempDir()/groxpi` when there is
  no user cache dir.
- **Check:** a config test asserting the default `CacheDir`.

### Package names that are not valid PEP 508 reach the upstream index

- **Where:** `internal/storage/store.go` — `Key`; `internal/index/service.go` — `Service.Resolve`.
- **What:** a name that normalises to something no project can have (`..` becomes `-`) passes `Key`'s
  single-segment check, so the request costs an upstream index query before it answers `404`.
- **Fix:** one shared `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$` check on the normalised name, applied in
  `storage.Key` and `index.Resolve`.
- **Check:** a table test of junk names asserting `ErrInvalidKey` and zero upstream hits.

### In s3 mode an upload still shows as in flight

- **Where:** `internal/storage/store.go` — `Store.commitS3`.
- **What:** `commitS3` unregisters the flight after its S3 Put, so `InFlight()` (the admin page)
  lists a download whose bytes are all served for as long as the upload takes.
- **Fix:** mark the flight as uploading once it finishes, and leave it out of `InFlight()` (or show it
  as an upload) while it stays registered for joiners.
- **Check:** an s3 store test that gates the fake S3 PUT and asserts `InFlight()` no longer lists the
  file once its reader has the whole body.

## Index

### The HTML package page carries no `#sha256=` href fragment

- **Where:** `internal/index/simple.go` — `RenderSimpleHTML`.
- **What:** PEP 503 lets a link carry its digest as `#sha256=<hex>`; the page groxpi renders links to
  `/simple/<pkg>/<file>` with no fragment, so an HTML-only client cannot verify the download against
  the index (the PEP 691 JSON body does carry `hashes`).
- **Fix:** append `#sha256=` plus the file's `Hashes["sha256"]` to the href when the upstream gave one.
- **Check:** a `RenderSimpleHTML` case with a hash asserting the fragment, and the round-trip test
  comparing `Hashes`.

## Admin and UI

### The admin listing is empty in pure s3 mode

- **Where:** `internal/admin/admin.go` — `Service.List`; `internal/storage/store.go` — `Store.Snapshot`.
- **What:** the listing shows the cache only, and pure `s3` mode has no cache, so `GET /admin` renders
  no rows however much the bucket holds.
- **Fix:** list the bucket with `ListObjectsV2` page by page when there is no cache (sizes and
  last-modified only; no access time).
- **Check:** a server test in s3 mode against the fake bucket asserting rows appear.

### The landing page reports a hardcoded version

- **Where:** `internal/config/config.go` — `Version`, the one constant the startup log, the home
  page and every upstream User-Agent read.
- **What:** the constant is fixed in source, not the build's version.
- **Fix:** set it from `runtime/debug.ReadBuildInfo` (or an `-ldflags -X` variable set in the
  Dockerfile).
- **Check:** a test asserting the home page and User-Agent carry the build-info version.

## Test gaps

- A cache commit failure in `hybrid` still uploads the verified spool to S3.
- An S3 upload failure in `s3` mode is logged, leaves no spool file, and the next request refetches.
- `rangedReader` at size 0, and seeking back to the open body's position reusing it without a new GET.
