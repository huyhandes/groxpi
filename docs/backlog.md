# Backlog

Known limits of the current code, each small enough to pick up on its own. None is a correctness bug
under normal operation; each names where it lives, what goes wrong, a proposed fix, and the check
that proves it. Delete an entry when it ships.

## Storage

### Evicting a package can race an in-flight upload

- **Where:** `internal/storage/store.go` — `Store.DeletePrefix` vs the upload at the end of `Store.fill`.
- **What:** in `hybrid` or `s3` mode, an admin evict that runs while a download of the same package is
  still uploading deletes the S3 objects, then the upload lands and recreates one. The next cache miss
  promotes it back, so the evict is partly undone.
- **Fix:** have `DeletePrefix` wait for (or cancel) flights whose key has the prefix before deleting,
  or record an eviction generation that a finishing upload checks first.
- **Check:** a store test that gates the fake S3 PUT, evicts mid-upload, releases the gate, and asserts
  the bucket is empty.

### A file can be fetched and uploaded twice

- **Where:** `internal/storage/store.go` — the `ponytail:` comment in `Store.Open`.
- **What:** a leader that commits between another request's `lookup` and its lock on the flight map is
  missed, so that request starts a second download. In `hybrid` and `s3` mode that is also a second
  S3 upload. A cache commit failure, or the LRU evicting the file before a reader returns, causes the
  same. The result is correct; the cost is one extra upstream fetch and PUT.
- **Fix:** re-run `lookup` under the flight-map lock (or right after taking it) before starting a
  leader.
- **Check:** a test that forces the interleaving and counts fetcher calls and bucket PUTs (both 1).

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

## Observability

### The upstream-fetch metric includes the S3 upload in hybrid mode

- **Where:** `internal/storage/store.go` — `Store.lead`, `telemetry.UpstreamFetch`.
- **What:** `fill` (which ends with the synchronous S3 upload) runs inside the timed region, so the
  upstream-fetch duration in `hybrid` and `s3` mode includes the PUT.
- **Fix:** record the duration when the spool is verified, before the commit and upload, and give the
  upload its own span.
- **Check:** a test with a slow fake S3 asserting the recorded fetch duration excludes the PUT delay.

## Admin and UI

### The admin listing is empty in pure s3 mode

- **Where:** `internal/admin/admin.go` — `Service.List`; `internal/storage/store.go` — `Store.Snapshot`.
- **What:** the listing shows the cache only, and pure `s3` mode has no cache, so `GET /admin` renders
  no rows however much the bucket holds.
- **Fix:** list the bucket with `ListObjectsV2` page by page when there is no cache (sizes and
  last-modified only; no access time).
- **Check:** a server test in s3 mode against the fake bucket asserting rows appear.

### The landing page reports a hardcoded version

- **Where:** `internal/server/server.go` — `handleHome`.
- **What:** `/` prints a fixed version string, not the build's.
- **Fix:** read it from `runtime/debug.ReadBuildInfo` (or an `-ldflags -X` variable set in the
  Dockerfile).
- **Check:** a test asserting the page carries the build-info version.

## Test gaps

- A cache commit failure in `hybrid` still uploads the verified spool to S3.
- An S3 upload failure in `s3` mode is logged, leaves no spool file, and the next request refetches.
- `rangedReader` at size 0, and seeking back to the open body's position reusing it without a new GET.
