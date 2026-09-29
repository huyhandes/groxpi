# Glossary

The storage vocabulary groxpi's code and docs use since ADR 0004. One term per line, with where it lives.

- **Store** — the read-through front of the cache and/or the durable store; it holds them as concrete types, with no backend interface. `Open` serves a hit, or starts or joins a download on a miss. `internal/storage/store.go:Store`
- **Object** — an opened package file. It is seekable when it comes from the cache (the server hands it to `http.ServeContent`), and forward-only (`InFlight`) while it is still downloading. `internal/storage/store.go:Object`
- **Fetcher** — the one-method interface the store uses to open a file upstream. `download.Service` implements it, and tests fake it. `internal/storage/store.go:Fetcher`
- **flight** — one download running for one storage key. Every request for that key joins it, and it keeps running after they all disconnect. `internal/storage/store.go:flight`
- **spool** — the temp file (`.tmp-*`) a flight writes to while it hashes. It sits under the cache directory in `local` and `hybrid`, and under `<GROXPI_CACHE_DIR>/spool` in pure s3 mode. `internal/storage/store.go:Store.fill`
- **tail** — a reader that follows a spool as it grows. Every client of a flight reads through one, the first client included. `internal/storage/store.go:tail`
- **holdback** — the final-byte holdback: a tail does not hand out the last expected byte until the flight is verified, so no client ever gets a complete body that then fails verification. `internal/storage/store.go:tail.Read`
- **promotion** — in hybrid mode, when the cache misses and S3 hits, the S3 body is copied into the cache through the same spool and tail as an upstream download. A promoted file is not uploaded back to S3. `internal/storage/store.go:Store.lookup`
- **storage modes** — `local` (the cache only), `s3` (the durable store only; a hit issues its `GetObject` before response headers and reads ranges in bounded windows), and `hybrid` (the cache in front of the durable store). Set by `GROXPI_STORAGE_TYPE`. `internal/server/server.go:initStorage`
- **cache (L1)** — the local filesystem (`GROXPI_CACHE_DIR`/`GROXPI_CACHE_SIZE` in local mode; `GROXPI_LOCAL_CACHE_DIR`/`_SIZE`/`_TTL` in hybrid, the only mode with a TTL): bounded, disposable, and its own LRU index (eviction goes by atime and stops at 90% of the budget). The only thing groxpi evicts. `internal/storage/local.go`
- **durable store (L2)** — the S3 bucket. Every verified download lands there, and groxpi never expires it; retention is the operator's, via bucket lifecycle rules. In hybrid mode the download goroutine uploads the finished file synchronously right after renaming it into the cache; a failed upload is logged. `internal/storage/s3.go`, `internal/storage/store.go:Store`
