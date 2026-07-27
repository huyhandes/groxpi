# Configuration

Groxpi supports comprehensive configuration through environment variables, maintaining full compatibility with the original proxpi implementation.

## Core Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `GROXPI_INDEX_URL` | `https://pypi.org/simple/` | Main PyPI index URL |
| `GROXPI_INDEX_TTL` | `1800` | Index cache TTL in seconds (30 minutes) |
| `GROXPI_EXTRA_INDEX_URLS` | - | Comma-separated extra indices, consulted **before** the main index in the order given |
| `GROXPI_EXTRA_INDEX_TTLS` | - | Per-index cache TTLs in seconds, positionally matched to the URLs (default `180`) |
| `GROXPI_CACHE_SIZE` | `5368709120` | File cache size in bytes (5GB) |
| `GROXPI_CACHE_DIR` | `./cache` | Cache directory path |
| `GROXPI_DOWNLOAD_TIMEOUT` | `0.9` | Timeout before redirect (seconds) |
| `GROXPI_CONNECT_TIMEOUT` | `30` | Socket connect timeout (seconds) |
| `GROXPI_READ_TIMEOUT` | `30` | Data read timeout (seconds) |
| `GROXPI_LOGGING_LEVEL` | `INFO` | Log level (DEBUG, INFO, WARN, ERROR) |
| `GROXPI_DISABLE_INDEX_SSL_VERIFICATION` | `false` | Skip SSL verification for indices |
| `GROXPI_BINARY_FILE_MIME_TYPE` | - | Force binary MIME types |

## Storage Configuration

Groxpi supports multiple storage backends for file caching.

### Local Storage (Default)

| Variable | Default | Description |
|----------|---------|-------------|
| `GROXPI_STORAGE_TYPE` | `local` | Storage backend type |
| `GROXPI_CACHE_SIZE` | `5368709120` | Local cache size limit with LRU eviction (5GB) |

### S3-Compatible Storage

| Variable | Default | Description |
|----------|---------|-------------|
| `GROXPI_STORAGE_TYPE` | `local` | Set to `s3` for S3 storage |
| `AWS_ENDPOINT_URL` | - | S3 endpoint URL (for MinIO/custom S3) |
| `AWS_ACCESS_KEY_ID` | - | S3 access key |
| `AWS_SECRET_ACCESS_KEY` | - | S3 secret key |
| `AWS_REGION` | `us-east-1` | AWS region |
| `GROXPI_S3_BUCKET` | - | S3 bucket name |
| `GROXPI_S3_PREFIX` | - | S3 key prefix |
| `GROXPI_S3_USE_SSL` | `true` | Enable SSL for S3 connections |
| `GROXPI_S3_FORCE_PATH_STYLE` | `false` | Force path-style URLs |

#### Removed Settings

The following variables were removed and are now **ignored**. They configured an
"async write" queue that `S3Storage.Put` submitted to and then immediately
blocked on, so the write was never asynchronous to the caller — the queue only
moved a blocking upload onto another goroutine and waited for it. Uploads now go
straight to S3, and the four buffer pools that fed that path are gone with it.
Remove these from your environment; setting them has no effect.

| Removed variable | Former default |
|------------------|----------------|
| `GROXPI_S3_ASYNC_WRITES` | `true` |
| `GROXPI_S3_ASYNC_WORKERS` | `10` |
| `GROXPI_S3_ASYNC_QUEUE_SIZE` | `1000` |

### Hybrid/Tiered Storage (Local L1 + S3 L2)

Hybrid storage provides a multi-tier caching system with fast local cache (L1) backed by persistent S3 storage (L2).

**Request Flow**: `User → Local Cache → S3 Cache → PyPI → Save to both S3 & Local`

| Variable | Default | Description |
|----------|---------|-------------|
| `GROXPI_STORAGE_TYPE` | `local` | Set to `hybrid` for tiered caching |
| `GROXPI_LOCAL_CACHE_SIZE` | `10737418240` | L1 local cache size limit (10GB) |
| `GROXPI_LOCAL_CACHE_DIR` | Same as `GROXPI_CACHE_DIR` | L1 local cache directory |
| `GROXPI_LOCAL_CACHE_TTL` | `0` (disabled) | L1 entry TTL in seconds. Expired entries are swept periodically — independently of the size limit — so a TTL takes effect even on a cache that never fills up |
| `GROXPI_TIERED_SYNC_WORKERS` | `5` | Workers for async L1 population from L2 |
| `GROXPI_TIERED_SYNC_QUEUE_SIZE` | `100` | Queue size for L1 sync operations |
| `AWS_ENDPOINT_URL` | - | S3 endpoint URL (required for hybrid) |
| `AWS_ACCESS_KEY_ID` | - | S3 access key (required for hybrid) |
| `AWS_SECRET_ACCESS_KEY` | - | S3 secret key (required for hybrid) |
| `AWS_REGION` | `us-east-1` | AWS region |
| `GROXPI_S3_BUCKET` | - | S3 bucket name (required for hybrid) |
| `GROXPI_S3_PREFIX` | `groxpi` | S3 key prefix |
| `GROXPI_S3_USE_SSL` | `true` | Enable SSL for S3 connections |
| `GROXPI_S3_FORCE_PATH_STYLE` | `false` | Force path-style URLs |

**Benefits of Hybrid Storage:**
- ⚡ **Fast Local Access**: L1 objects are real files, so they are served by path and `net/http` handles range and conditional requests (not a kernel zero copy — see [performance.md](performance.md))
- 💾 **S3 Persistence**: All packages stored durably in S3 (L2)
- 🔄 **Auto L1 Population**: L2 hits automatically populate L1 for future requests
- 📊 **LRU Eviction**: Intelligent L1 cache management based on access patterns, plus an optional TTL sweep (`GROXPI_LOCAL_CACHE_TTL`)
- 💰 **Cost Efficient**: Only cache hot packages locally, everything else in S3

## Server Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `5000` | HTTP server port |
| `HOST` | `0.0.0.0` | HTTP server host |

## Performance Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `GROXPI_RESPONSE_CACHE_SIZE` | `1000` | Response cache entries |
| `GROXPI_RESPONSE_CACHE_TTL` | `300` | Response cache TTL (seconds) |
| `GROXPI_MAX_CONCURRENT_DOWNLOADS` | `10` | Max concurrent downloads |

## Example Configurations

### Development Setup
```bash
export GROXPI_INDEX_URL="https://pypi.org/simple/"
export GROXPI_INDEX_TTL=1800
export GROXPI_CACHE_SIZE=1073741824  # 1GB
export GROXPI_LOGGING_LEVEL=DEBUG
```

### Production with S3 Backend
```bash
export GROXPI_STORAGE_TYPE=s3
export AWS_ENDPOINT_URL=https://s3.amazonaws.com
export AWS_ACCESS_KEY_ID=your_access_key
export AWS_SECRET_ACCESS_KEY=your_secret_key
export AWS_REGION=us-west-2
export GROXPI_S3_BUCKET=groxpi-cache
export GROXPI_S3_PREFIX=packages/
export GROXPI_CACHE_SIZE=10737418240  # 10GB
export GROXPI_LOGGING_LEVEL=INFO
```

### MinIO Setup
```bash
export GROXPI_STORAGE_TYPE=s3
export AWS_ENDPOINT_URL=http://localhost:9000
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin
export AWS_REGION=us-east-1
export GROXPI_S3_BUCKET=groxpi
export GROXPI_S3_USE_SSL=false
export GROXPI_S3_FORCE_PATH_STYLE=true
```

### Hybrid Storage Setup (Local L1 + S3 L2)
```bash
# Tiered caching: Fast local L1 + persistent S3 L2
export GROXPI_STORAGE_TYPE=hybrid

# Local L1 cache configuration
export GROXPI_LOCAL_CACHE_SIZE=10737418240  # 10GB local cache
export GROXPI_LOCAL_CACHE_DIR=/var/cache/groxpi
export GROXPI_LOCAL_CACHE_TTL=86400  # 24h; 0 disables TTL expiry
export GROXPI_TIERED_SYNC_WORKERS=5
export GROXPI_TIERED_SYNC_QUEUE_SIZE=100

# S3 L2 storage configuration
export AWS_ENDPOINT_URL=http://localhost:9000
export AWS_ACCESS_KEY_ID=minioadmin
export AWS_SECRET_ACCESS_KEY=minioadmin
export AWS_REGION=us-east-1
export GROXPI_S3_BUCKET=groxpi
export GROXPI_S3_PREFIX=packages/
export GROXPI_S3_USE_SSL=false
export GROXPI_S3_FORCE_PATH_STYLE=true

# Server configuration
export PORT=5000
export GROXPI_LOGGING_LEVEL=INFO
```

### Multiple Indices
```bash
export GROXPI_INDEX_URL="https://pypi.org/simple/"
export GROXPI_EXTRA_INDEX_URLS="https://private.pypi.example.com/simple/,https://test.pypi.org/simple/"
export GROXPI_EXTRA_INDEX_TTLS="900,3600"  # 15 minutes, 1 hour
```

Resolution order is **extras first, in the order listed, then the main index
last**. The first index that has the package supplies its whole file list; lists
from different indexes are never merged, so a package registered on PyPI under an
internal name cannot shadow the internal one. A 404 is returned only after every
index has missed, and each index's entries expire on that index's own TTL. An
extra index that *fails* (connection refused, 5xx) fails the request rather than
falling through to the main index. See
[ADR 0001](adr/0001-extras-first-index-resolution.md) for the reasoning.

Credentials may be embedded in any index URL
(`https://user:token@private.example.com/simple/`); they are redacted from logs,
error messages and the `/health` payload.

> **Upgrade note:** before this release `GROXPI_EXTRA_INDEX_URLS` was parsed and
> never queried. If you already had it set, resolution changes: packages that
> resolved from PyPI may now resolve from an extra index.

## Docker Environment

For Docker deployments, you can use an environment file:

```bash
# .env
GROXPI_INDEX_URL=https://pypi.org/simple/
GROXPI_INDEX_TTL=1800
GROXPI_CACHE_SIZE=5368709120
GROXPI_STORAGE_TYPE=local
GROXPI_LOGGING_LEVEL=INFO
```

Then run with Docker Compose:
```bash
docker-compose --env-file .env up
```

## Configuration Validation

Groxpi validates configuration on startup and will log warnings for:
- Invalid URLs
- Unreachable indices
- Insufficient disk space for cache
- Invalid S3 credentials (if using S3 storage)

Configuration errors will prevent the server from starting with clear error messages.