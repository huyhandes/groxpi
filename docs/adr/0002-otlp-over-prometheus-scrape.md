# 0002 — Export over OTLP instead of serving a Prometheus scrape endpoint

- Status: accepted
- Date: 2026-07-27
- Context: spec 3 (extra indexes plus OpenTelemetry), issue #24

## Context

groxpi had no telemetry at all. What it had was documentation for a `/metrics` endpoint and a shipped
`monitoring/prometheus.yml` that scraped it every five seconds. The endpoint never existed, so that
scrape had been failing since the day it was written, and a referenced Grafana dashboard directory was
absent too. The practical cost was not the broken scrape: it was that several real caching failures —
a default timeout that prevented almost all caching, a cache that never evicted expired entries, an
integrity check whose result was only logged — were invisible from outside the process. Nobody could
have noticed them.

Fixing that means choosing how telemetry leaves the process. Two options were real:

1. **Serve `/metrics` in Prometheus exposition format.** Add `promhttp`, register counters and
   histograms, and let operators scrape groxpi directly.
2. **Export logs, metrics and traces over OTLP** to a collector endpoint the operator configures.

## Decision

groxpi exports all three signals over OTLP (HTTP/protobuf) to one configured endpoint. It serves no
Prometheus endpoint. With `OTEL_EXPORTER_OTLP_ENDPOINT` unset the providers are no-op.

## Rationale

**One protocol, three signals, one destination.** A scrape endpoint only carries metrics. Traces and
logs would each need a separate transport and a separate piece of configuration, and the interesting
questions about a caching proxy are trace-shaped: *where did this slow request spend its time*, *which
index answered*, *did this response come from cache*. A counter cannot answer those. Choosing the
metrics-only transport would have meant either shipping only metrics or bolting two more transports
alongside it.

**Correlation comes for free.** The logging library was swapped to one with an existing OpenTelemetry
bridge, so log records carry the trace and span identifiers of the request that produced them without
any hand-rolled plumbing. That correlation only exists if logs travel the same pipeline.

**A collector can still produce Prometheus.** Rejecting a scrape endpoint does not reject Prometheus.
The collector's `prometheus` exporter re-exposes everything groxpi sends, so operators who scrape keep
scraping — they scrape the collector. The capability is not lost, only relocated to a component that
already exists in most deployments. The reverse is not true: a `/metrics` endpoint cannot be turned
into traces.

**Pull would have to be re-plumbed anyway.** A scrape endpoint is a pull model that assumes the
scraper can reach the process. groxpi is deployed in places where that is awkward, and a push exporter
is the same amount of configuration for the operator: one URL.

## Consequences

- **Binary size grows.** The OpenTelemetry SDK plus three exporters is a substantial dependency. This
  was accepted deliberately in the spec discussion. If size becomes a hard constraint, the narrower
  fallback is metrics only — and that should be taken as its own decision, not discovered late.
- **A collector becomes part of a monitored deployment.** Operators who only run Prometheus must add
  one. This is the main cost of the decision and the main argument the other way.
- **Nothing is monitored by default.** With no endpoint configured groxpi is exactly as observable as
  it was before. Observability is opt-in, which is what keeps startup free of a dependency on a
  collector being reachable.
- **The documented `/metrics` endpoint is removed from the documentation rather than implemented**, and
  `monitoring/prometheus.yml` now scrapes a collector. Leaving either as it was would have contradicted
  this decision.

## Alternatives rejected

- **Both**: an OTLP exporter *and* a `/metrics` endpoint. Two ways to read the same numbers, two
  registries to keep in step, and a second public surface to keep working. There is no question
  answerable through the endpoint that the collector cannot answer.
- **OpenMetrics/Prometheus remote-write directly from groxpi.** Push, but metrics only — it inherits
  the single-signal problem while giving up the collector's ability to fan out to other backends.
