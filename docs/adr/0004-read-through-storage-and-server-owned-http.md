# 0004 — Read-through storage and server-owned HTTP

- Status: accepted
- Date: 2026-09-27
- Context: supersedes decision 2 and 3 of 0003 (module layout and the cross-module seam); decision 1
  (stdlib `http.ServeMux`) stands

## Context

0003 left four modules that each mount their own routes, with `download` as the orchestrator of the
cache: it asks storage whether a file exists, resolves the index, coalesces the upstream fetch, tees it
to the client and writes it back through `storage.Put`. Storage is a passive bucket, so the logic that
makes it a cache lives in its consumer, and the HTTP surface is spread across four `Register` calls.

The local LRU keeps every cached file in an in-memory list and loses recency on restart: the startup
scan re-adds files in directory-walk order.

## Decision

1. **Server owns HTTP.** Every route, handler, content negotiation, redirect decision, admin page and
   template lives in `server`. `index`, `storage`, `download` and `admin` are Go APIs with no `net/http`
   handler code and no `Register(mux)`.

2. **Storage is the cache, read-through.** `storage.Open(ctx, pkg, file)` returns an object the server
   hands to `http.ServeContent`. On a miss storage coalesces concurrent requests (singleflight), asks
   `download.Fetch` for the upstream body, writes it to a spool file under `GROXPI_CACHE_DIR` while
   hashing, and every waiting client tails that growing file. On a sha256 match the file is renamed into
   place (local) or uploaded from the finished file (s3, hybrid); on a mismatch it is deleted and every
   reader fails. Storage owns the in-flight registry.
   - A download continues after every client disconnects and is cached (`context.WithoutCancel`).
   - While a file is still downloading, `Range` is ignored and a 200 is streamed from byte 0. Once cached,
     `Range` and conditional requests work for every backend.
   - `Open` with a no-fetch option returns `ErrNotFound` on a miss; redirect mode
     (`GROXPI_DOWNLOAD_TIMEOUT=0`) is a server decision built on it.

3. **Download is a stateless fetcher.** `download.Fetch(ctx, pkg, file)` resolves the URL, size and
   sha256 through `index` and returns the upstream body. It imports neither storage nor HTTP handlers.
   The single cross-module interface is `storage.Fetcher`, defined by its consumer, implemented by
   `download`, faked in tests.

4. **Three storage modes, two backends.** `local`, `s3`, `hybrid`. Local and S3 sit behind an unexported
   backend interface inside `storage`; hybrid is "local then S3" logic in the store, not a third backend.
   An L1 miss with an L2 hit streams from S3 and promotes into L1 through the same spool-and-tail path as
   an upstream miss. An S3 hit is a lazy `io.ReadSeeker`: the seek-then-read `ServeContent` performs
   becomes one ranged `GetObject`. No presigned redirects.

5. **The filesystem is the LRU index.** A hit sets the file's atime with `os.Chtimes` (throttled to about
   once an hour per file, mtime untouched because it is `Last-Modified`). One atomic counter tracks total
   size, seeded by a startup walk. Crossing the size limit triggers a walk that deletes by oldest atime
   down to 90%; the same walk applies the TTL. No in-memory entry list; recency survives restarts. The
   admin listing is an on-demand paginated walk showing last-accessed time; the hits column is dropped.

6. **Admin is a service.** `List`, `Evict` (storage delete plus index invalidate), `Prefetch`,
   `InFlight`. The server renders its page.

Dependencies point one way: `server → admin → storage → download → index`, with `server` also calling
`storage` and `index` directly.

## Constraints

- No externally visible change: routes, environment variables, storage key layout
  (`packages/{pkg}/{file}`), response headers and redirect mode are preserved. HTTP-level
  characterization tests against `server.Router()` land first and stay unchanged through the refactor.
- Memory per download is one pooled copy buffer regardless of file size; nothing buffers a whole file.
- The benchmark suite is run before and after as a no-regression gate. No figures are published.

## Consequences

- `download` shrinks to the fetch; the tee, singleflight and in-flight registry move into `storage`.
- `LRUCache`, `LRULocalStorage` and the `l1Storage` interface are deleted.
- The atime helper needs a small build-tagged file per OS (`Stat_t.Atim` on Linux, `Atimespec` on darwin).
- Eviction cost is an O(n) directory walk, paid only when the cache crosses its limit.
