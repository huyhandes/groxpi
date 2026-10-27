# Benchmarking

How to measure groxpi. **This guide publishes no figures.** Every number this repository once quoted
predates the current architecture and has been deleted rather than guessed at; there is nothing to
compare against until the suite is re-run.

## What the suite measures

`benchmarks/` compares groxpi against the Python proxpi implementation on two things:

- **Index API throughput and latency** (`scripts/wrk_api_test.sh`) — `wrk` against the index endpoints,
  cold and warm.
- **End-to-end install time** (`scripts/uv_install_test.sh`) — `uv add` of real packages into a fresh
  project and virtualenv through each proxy, in a container.

Resource use is sampled alongside both (`scripts/monitor_resources.sh`), and
`scripts/analyze_results_duckdb.sh` turns the raw output into comparison tables.

## Prerequisites

- `wrk` — `brew install wrk`, or `apt-get install wrk`
- Docker, for the install tests and for running both proxies
- `duckdb` — `brew install duckdb`, only for the analysis step
- Two running servers to point at: groxpi and proxpi

## Running it

Bring the two proxies up (`benchmarks/docker/docker-compose.benchmark.yml` does this), then:

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
| `--no-monitoring` | Skip resource sampling. |
| `--timestamp TS` | Reuse a specific run timestamp instead of generating one. |
| `--docker-network N` | Docker network the containers share. |
| `--results-dir DIR` | Where results are written (default `benchmarks/results`). |
| `-h`, `--help` | Usage. |

`GROXPI_URL`, `PROXPI_URL` and `DOCKER_NETWORK` are read from the environment as defaults for the
corresponding flags.

Individual stages can be run directly. `scripts/wrk_api_test.sh` and `scripts/uv_install_test.sh` take
positional arguments — `<groxpi_url> <proxpi_url> <timestamp> [scenario]` — and print their usage when
called with none. They write to `RESULTS_DIR` from the environment (default `benchmarks/results`);
`benchmark.sh` exports it from `--results-dir`. `scripts/analyze_results_duckdb.sh [timestamp]
[results_dir]` reads one run's `resources-`, `wrk-summary-` and `uv-summary-<timestamp>.csv` — the
latest timestamp when none is given — and has a `--help`.

## Cache state

A comparison is meaningless unless both proxies start from the same cache state.
`scripts/cache_manager.sh` clears them between runs. On groxpi `DELETE /cache/list` does nothing
(the root index is proxied, not cached), so the script issues `DELETE /cache/<package>` for every
package under test — `BENCH_PACKAGES`, a space-separated list defaulting to the scenarios' packages.
proxpi gets `DELETE /cache/list` as well. Any failed groxpi eviction aborts the run.

Those routes exist on groxpi only when admin credentials are configured — see
[configuration.md](configuration.md#administrative-interface) — so the groxpi under test must have
them. `docker-compose.benchmark.yml` starts groxpi with `GROXPI_ADMIN_USERNAME` and
`GROXPI_ADMIN_PASSWORD` both defaulting to `bench`, and the script sends the same variables, with the
same defaults, as basic auth. Against any other groxpi, export the credentials it was started with.
If the clear is refused (`401`, `403`, or `404`) the run aborts rather than benchmark a warm cache as a
cold one. proxpi has no admin auth and is never sent credentials.

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
