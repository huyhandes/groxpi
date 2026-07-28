# Deployment

## Binary

```bash
go build -ldflags="-w -s" -o groxpi ./cmd/groxpi
GROXPI_CACHE_DIR=/var/cache/groxpi ./groxpi
```

groxpi listens on `PORT` (default `5000`) on all interfaces. It shuts down gracefully on `SIGINT` and
`SIGTERM`: in-flight requests are given a few seconds, the index cache sweeper stops, and the storage
backend is closed — which is what drains pending uploads in `hybrid` mode.

## Docker

The image is built from `scratch` and contains the binary, CA certificates and timezone data. Nothing
else — no shell, no `curl`, no `wget`, no templates (the admin page's assets are embedded in the
binary).

```bash
docker build -t groxpi:latest .
docker run -p 5000:5000 -v groxpi_cache:/cache groxpi:latest
```

The image has no shell or HTTP client, so the health check is the binary itself: `groxpi
--health-check` GETs `/health` on `127.0.0.1` at the configured `PORT` and exits 0 only on a 200. It
starts no server, opens no storage backend and installs no telemetry. The Dockerfile's `HEALTHCHECK`
and both compose files use it, so `depends_on: service_healthy` gates correctly and Docker restarts a
container whose server has stopped answering. A non-default `PORT` is honoured, since the probe reads
the same configuration the server does.

Probing from outside the container — a Kubernetes `httpGet` probe, a load-balancer check — remains
valid and is what the Kubernetes manifest below uses.

```bash
docker compose up -d      # see docker-compose.yml
```

`docker-compose.minio.yml` brings up MinIO alongside groxpi for exercising the S3 path locally.

## Kubernetes

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: groxpi
spec:
  replicas: 2
  selector:
    matchLabels: { app: groxpi }
  template:
    metadata:
      labels: { app: groxpi }
    spec:
      containers:
        - name: groxpi
          image: ghcr.io/huyhandes/groxpi:latest
          ports:
            - containerPort: 5000
          env:
            - name: GROXPI_CACHE_DIR
              value: /cache
            - name: GROXPI_CACHE_SIZE
              value: "5368709120"
            - name: OTEL_EXPORTER_OTLP_ENDPOINT
              value: http://otel-collector:4318
          volumeMounts:
            - name: cache
              mountPath: /cache
          readinessProbe:
            httpGet: { path: /health, port: 5000 }
          livenessProbe:
            httpGet: { path: /health, port: 5000 }
      volumes:
        - name: cache
          emptyDir: {}
```

Each replica keeps its own cache. To share one cache between replicas, use `GROXPI_STORAGE_TYPE=hybrid`
with a common S3 bucket: each pod keeps a local L1 and the bucket is the shared L2.

## Storage choice

| Mode | Use when |
|---|---|
| `local` | One instance, or per-instance caches are acceptable. |
| `s3` | Cache must be shared and durable, and local disk is unavailable. Note that files are then always served across the network — there is no local path to hand to `net/http`. |
| `hybrid` | Several instances that should share a durable cache while still serving hot files from local disk. |

See [configuration.md](configuration.md) for the settings each mode reads.

## S3 credentials

groxpi uses the AWS SDK for Go v2 and configures static keys **only** when both `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` are set. With neither set, the SDK's default credential chain applies, so the
preferred deployment sets no keys at all and relies on the role attached to the workload:

- **EKS** — an IAM role for the service account (or Pod Identity). The projected token and role ARN the
  cluster injects are picked up by the chain; set `GROXPI_S3_BUCKET` and `AWS_REGION` and nothing else.
- **EC2 / ECS** — the instance profile or task role, read from the metadata or container credentials
  endpoint by the same chain.
- **Static keys** — for MinIO and local development, where there is no role to assume.

Whichever path is used, groxpi calls `HeadBucket` at startup and refuses to start if the bucket is
unreachable — a missing or unauthorised credential is a startup failure, not a runtime surprise. Loading
the SDK config and that probe share one deadline: `GROXPI_CONNECT_TIMEOUT` if set, otherwise 10 seconds.
So a slow metadata endpoint on a cold node shows up as a failure to start, and on such a node the
setting has to be raised. The
startup log line records `static_credentials=true|false`, which is the quickest way to confirm a pod is
really using its role.

The role needs `s3:GetObject`, `s3:PutObject` and `s3:ListBucket` on the bucket — `ListBucket` covers the
`HeadBucket` probe. No request path deletes an S3 object, so `s3:DeleteObject` is not required; see the
eviction note in [api-endpoints.md](api-endpoints.md).

## Reverse proxy and TLS

The admin surface authenticates with HTTP basic auth, which transmits the username and password in
cleartext. **If the admin surface is enabled, a TLS-terminating proxy in front of groxpi is required.**
groxpi terminates no TLS of its own.

```nginx
server {
    listen 443 ssl;
    server_name pypi.example.com;

    ssl_certificate     /etc/ssl/certs/pypi.crt;
    ssl_certificate_key /etc/ssl/private/pypi.key;

    location / {
        proxy_pass http://groxpi:5000;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # Package downloads stream; do not buffer them, and allow slow ones.
        proxy_buffering off;
        proxy_read_timeout 300s;
    }
}
```

If the admin surface is not needed, leave `GROXPI_ADMIN_USERNAME` and `GROXPI_ADMIN_PASSWORD` unset:
the routes are then not registered at all, including the `DELETE /cache/*` eviction routes.

## Observability

Set `OTEL_EXPORTER_OTLP_ENDPOINT` to a collector's HTTP endpoint and all three signals — traces,
metrics, logs — are exported there. Unset, telemetry is inert and costs nothing.

groxpi exposes no `/metrics` endpoint. Prometheus scrapes the collector, not groxpi;
`monitoring/prometheus.yml` is configured that way, and
[adr/0002-otlp-over-prometheus-scrape.md](adr/0002-otlp-over-prometheus-scrape.md) explains why. The
collector's own configuration owns the OTLP receiver and whatever exporter Prometheus scrapes, so the
metric names Prometheus sees are whatever that pipeline produces from the OTLP names in
[architecture.md](architecture.md).

What to watch:

- `groxpi.redirects` — every increment is a request groxpi did not serve from cache. A rising
  `redirect.reason=fetch_failed` usually means `GROXPI_DOWNLOAD_TIMEOUT` is too tight for the upstream.
- `groxpi.cache.occupancy` and `groxpi.cache.evictions` with `cache.layer=index` — the only honest basis
  for changing `GROXPI_INDEX_CACHE_SIZE` from its provisional default.
- `groxpi.index.resolutions` with `index.result=error` — an upstream index in trouble.
- `groxpi.verification.failures` — a download whose bytes contradicted the index. Never zero-tolerance
  noise; investigate.

For logs, set `GROXPI_LOG_FORMAT=json` when something aggregates them, and
`GROXPI_LOGGING_LEVEL=DEBUG` while diagnosing cache behaviour.

## Client configuration

```bash
pip install --index-url http://pypi.example.com/simple/ numpy
```

```ini
# pip.conf / pip.ini
[global]
index-url = http://pypi.example.com/simple/
trusted-host = pypi.example.com   # only if not behind TLS
```

```bash
UV_INDEX_URL=http://pypi.example.com/simple/ uv pip install numpy
```

```toml
# pyproject.toml, Poetry
[[tool.poetry.source]]
name = "groxpi"
url = "http://pypi.example.com/simple/"
priority = "primary"
```
