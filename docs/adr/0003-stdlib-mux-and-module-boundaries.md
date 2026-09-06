# 0003 — Standard-library router and four deep modules

- Status: accepted
- Date: 2026-09-06
- Context: branch `stdlib-mux-deep-modules`; supersedes the Gin transport the project started with

## Context

groxpi began as a port of the Python `proxpi` and inherited a web framework by default: Gin, with its
router, context type, response-writer wrapper and a tree of indirect dependencies (including a JSON
library the code never called). The server package had grown into the place everything lived — routes,
index resolution, download planning, the admin page and prefetch — with the seams between them drawn
by interfaces that each had exactly one implementation plus a test fake.

Two things prompted a revisit. Go 1.22 gave `http.ServeMux` method matching and path wildcards, which
was the whole of what groxpi used a router for. And the request path that matters most — serving a
cached file — was paying for the framework: Gin's response writer hides the `io.ReaderFrom` hook that
`net/http` uses to copy a file to a socket without a user-space round trip, so `c.File` could never
benefit from it.

## Decision

1. **The router is `http.ServeMux`.** Routes are method-and-pattern strings (`GET /simple/{package}/{$}`).
   The mux supplies trailing-slash redirects, `405` with `Allow`, path cleaning, and the matched
   pattern (`r.Pattern`) that names each request span. No third-party router is added in its place.

2. **Four deep modules on one mux, one storage seam under them.** `index` (resolution, cache, upstream
   client, root proxy), `download` (plan, coalesced fetch, in-flight registry, tee download), `admin`
   (auth, cross-site check, page, prefetch, eviction) and `server` (composition root, middleware,
   shutdown). Each module's surface is `Register(mux)` plus the two or three methods its one consumer
   needs. Dependencies point one way: `server → admin → download → index`, with `storage` used by `download` and `admin` (never by `index`).

3. **Interfaces only where a second implementation exists, defined by the consumer.** `storage.Storage`
   and its capability interfaces stay (four backends). `download.Resolver` is the single cross-module
   interface: one method, defined in `download`, satisfied by `*index.Service` in production and by a
   fake in tests. The `StreamingDownloader`, `packageIndex` and `fakeDownloader` seams are deleted.

4. **The singleflight is made visible.** The download module keeps a registry of running downloads
   with bytes streamed, declared size, start time and coalesced request count. The admin page renders
   it and a gauge reports its size. No cancel, no resume, no waiter streaming from a partial file.

## Rationale

**The framework bought nothing the standard library did not.** Every Gin feature groxpi used —
routing, path parameters, basic auth, recovery, a status-recording writer — is a few lines of `net/http`
or already in it. What it cost was a dependency tree, a second response-writer type in every handler
signature, and an opaque wrapper between the handlers and the socket.

**Deep modules over many shallow ones.** The old layout had a large `server` package with everything
in it and small leaf packages (`pypi`, `cache`, `streaming`) that were each half of a concept. The
module boundary that carries weight is the one a consumer actually crosses: `download` needs a file
list from `index`; `admin` needs `Warm`, `Invalidate` and `InFlight`; `server` needs `Register`. Those
are the surfaces. Files and folders inside a module are a housekeeping detail.

**Consumer-defined interfaces keep the seam honest.** `download.Resolver` exists because `download`'s
tests need to fake an index. It is one method because that is all `download` calls. It is defined in
`download` because `index` does not need to know the seam exists. Every other candidate interface had
one production implementation, which means it was a test fake wearing a type, and the tests now use
the real module against `httptest` upstreams instead.

**Coalescing you cannot see is coalescing you cannot debug.** The singleflight that deduplicates
downloads was already there; an operator staring at a slow install had no way to see that thirty
clients were parked behind one upstream stream. The registry is the same map the singleflight already
implies, with a read method.

## Consequences

- `go.mod` loses Gin and its transitive dependencies. Nothing else is added.
- `GET /simple/<package>` without the trailing slash is now a `307` redirect from the mux rather than a
  route of its own. `/index/<package>` (no slash) remains a distinct route for Python-proxpi clients.
- Request spans are named by the matched pattern, so span names change from Gin's `:param` form to
  `{param}`.
- The tracing middleware's writer wrapper must forward `io.ReaderFrom` and `Unwrap`; a test pins this.
  Whether the platform then avoids a user-space copy is not claimed or measured in the documentation.
- The benchmark suite is run before and after as a regression check. No figures are recorded; see
  [benchmarking.md](../benchmarking.md).
- Deferred, each as its own decision if it is ever wanted: S3 `Range` passthrough, download cancel,
  resume, and serving waiters from a partial file. PEP 658 `.metadata` passthrough is the next branch.

## Alternatives rejected

- **Chi or another lightweight router.** Middleware chaining and a `context`-free handler signature are
  the standard library's already. A router that adds only sugar is a dependency that adds only risk.
- **One package per "layer" (handlers, services, repositories).** Layers cut across the concepts;
  every change would touch three packages. Modules cut along them.
- **Keeping the interfaces for "future flexibility".** A seam with one caller is speculative
  machinery. The flexibility that was actually needed — swapping the index in download's tests — is
  covered by the one interface kept.
