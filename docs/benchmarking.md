# Benchmarking

How to measure groxpi. **This guide publishes no figures.** Every number this repository once quoted
predates the current architecture and has been deleted rather than guessed at; there is nothing to
compare against until the suite is re-run.

## What the suite measures

`benchmarks/` compares groxpi against the Python proxpi implementation on two things:

- **Index API throughput and latency** (`scripts/wrk_api_test.sh`) — single requests against an empty
  cache, and a `wrk` load run against a warm one. There is no cold load run: only its first request
  would miss. A load run that gets any non-2xx/3xx reply counts as failed, and its rate is left out of
  the summary.
- **End-to-end install time** (`scripts/uv_install_test.sh`) — `uv add` of real packages into a fresh
  project and virtualenv through each proxy, in a container.

Resource use is sampled alongside both (`scripts/monitor_resources.sh`, one `docker stats` call
for both containers per tick; a tick takes the interval plus that call), and
`scripts/analyze_results_duckdb.sh` turns the raw output into comparison tables.

## Prerequisites

- `wrk` — `brew install wrk`, or `apt-get install wrk`
- Docker, for the install tests and for running both proxies. The benchmark compose file's
  healthchecks use `start_interval`, which needs Docker Engine 25.0+ and Docker Compose 2.20.2+
- `duckdb` — `brew install duckdb`, only for the analysis step
- Both proxies running from `benchmarks/docker/docker-compose.benchmark.yml` on the same host, since
  cold runs recreate those containers (see [Cache state](#cache-state))

## Running it

`benchmark.sh` starts by running `docker compose up -d --build --wait` on the benchmark compose file, so
groxpi is built from the current checkout as `groxpi:bench` (its own tag, apart from the
`groxpi:latest` of the other compose files) and both proxies are running before anything is measured.
Run it from the checkout you want to measure:

```bash
./benchmarks/benchmark.sh \
  --groxpi-url http://localhost:5005 \
  --proxpi-url http://localhost:5006
```

Both URLs are required. Options:

| Flag | Meaning |
|---|---|
| `--api-only` | Run only the `wrk` API benchmarks. |
| `--uv-only` | Run only the install benchmarks. |
| `--no-monitoring` | Skip resource sampling. Sampling reads `docker stats` for the local containers `groxpi-bench` and `proxpi-bench`, whatever the URLs point at. |
| `--timestamp TS` | Reuse a specific run timestamp instead of generating one. |
| `--docker-network N` | Docker network the containers share. |
| `--results-dir DIR` | Where results are written (default `benchmarks/results`). |
| `-h`, `--help` | Usage. |

`GROXPI_URL`, `PROXPI_URL` and `DOCKER_NETWORK` are read from the environment as defaults for the
corresponding flags; `BENCH_COMPOSE_FILE` names the compose file (see [Cache state](#cache-state)).

Individual stages can be run directly; they build nothing, so bring the proxies up first.
`scripts/wrk_api_test.sh` and `scripts/uv_install_test.sh` take positional arguments — `<groxpi_url> <proxpi_url> <timestamp> [scenario]` — and print their usage when
called with none. They write to `RESULTS_DIR` from the environment (default `benchmarks/results`);
`benchmark.sh` exports it from `--results-dir`. `scripts/analyze_results_duckdb.sh [timestamp]
[results_dir]` reads one run's `resources-`, `wrk-summary-` and `uv-summary-<timestamp>.csv` — the
latest timestamp over all three when none is given, so the tables never mix runs — and has a `--help`.

## Cache state

A comparison is meaningless unless both proxies start from the same cache state.
Before every cold measurement, `scripts/cache_manager.sh` empties the cache of the proxy about to be
measured, and only that one, by recreating its container: `docker compose rm -sfv groxpi-bench` (or
`proxpi-bench`), then `docker compose up -d --wait` for the same service, which reuses the image built
at the start of the run rather than rebuilding it. Recreating both would wipe the other proxy's cache
before its warm run.
`docker-compose.benchmark.yml` gives each proxy an anonymous cache volume, which `rm -v` deletes; a named
volume would survive it. The compose file is `benchmarks/docker/docker-compose.benchmark.yml` unless
`BENCH_COMPOSE_FILE` names another, and the containers must be on the host running the suite, behind
the URLs it is given. If recreating them fails, `wrk_api_test.sh` skips that cold measurement and
exits non-zero, and `uv_install_test.sh` stops, rather than record a warm cache as a cold one.

Neither proxy's HTTP eviction empties its cache, which is why the suite does not use it. groxpi's
`DELETE /cache/list` invalidates nothing, because the root index is proxied and not cached. proxpi's
`DELETE /cache/<package>` drops only the in-memory file list and keeps every downloaded file on disk.

The `wrk` load runs hit `/simple/numpy/`, not `/simple/`. groxpi passes the root index through to the
upstream uncached, so a `/simple/` load run would measure pypi.org.

## Go microbenchmarks

The storage and download paths carry Go benchmarks:

```bash
go test -bench=. -benchmem ./internal/storage/
go test -bench=. -benchmem ./internal/server/
```

These are for spotting a regression between two commits on one machine. They are not comparable across
machines and are not what the suite above measures.

## Reporting a result

A figure is only worth publishing with the commit it was measured at, the machine, the cache state, and
the command line that produced it. Anything less becomes the kind of claim this guide exists to avoid
repeating.
