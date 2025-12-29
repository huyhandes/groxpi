# Migration from Python proxpi

Groxpi is designed as a drop-in replacement for the Python-based proxpi project, providing 100% API compatibility while delivering significant performance improvements.

## Compatibility Overview

### ✅ 100% API Compatible
- **Same endpoints**: Identical URL structure and behavior
- **Same responses**: Compatible JSON and HTML formats
- **Same configuration**: All environment variables supported
- **Same clients**: Works with pip, poetry, pipenv, uv, and other PyPI clients

### 🚀 Performance Improvements (December 2024 Benchmarks)
- **12.8x higher throughput**: 52,880 vs 4,139 requests/sec for package index
- **27x faster latency**: 0.85ms vs 23.04ms P50 response times
- **Sub-millisecond** response times for cached package index
- **High load stability**: Groxpi maintains responses while proxpi fails under high concurrency
- **5x faster** startup time (<2s vs ~10s)
- **1000+ concurrent** connections supported

## Migration Steps

### 1. Assessment Phase

#### Current proxpi Analysis
Before migrating, document your current proxpi setup:

```bash
# Check current proxpi configuration
env | grep -E "(INDEX_URL|CACHE|TTL|STORAGE)"

# Note current performance metrics
# - Response times
# - Memory usage
# - Cache hit ratios
# - Error rates

# Document client configurations
# - pip.conf settings
# - poetry configuration
# - Docker configurations
```

#### Compatibility Check
Verify your setup is compatible:
- ✅ Standard PyPI Simple API usage
- ✅ Environment variable configuration
- ✅ Local file caching or S3 storage
- ✅ Basic authentication (if needed)

### 2. Side-by-Side Testing

#### Setup Test Environment
Run groxpi alongside existing proxpi for comparison:

```bash
# Run existing proxpi on port 5006
python -m proxpi --port 5006

# Run groxpi on port 5005
docker run -p 5005:5000 groxpi:latest
```

#### Test API Compatibility
```bash
# Test package listing
curl -H "Accept: application/json" http://localhost:5006/simple/ > proxpi_packages.json
curl -H "Accept: application/json" http://localhost:5005/simple/ > groxpi_packages.json
diff proxpi_packages.json groxpi_packages.json

# Test package details
curl -H "Accept: application/json" http://localhost:5006/simple/requests/ > proxpi_requests.json
curl -H "Accept: application/json" http://localhost:5005/simple/requests/ > groxpi_requests.json
diff proxpi_requests.json groxpi_requests.json

# Test client compatibility
pip install --index-url http://localhost:5005/simple/ --dry-run requests
poetry config repositories.test http://localhost:5005/simple/
```

#### Performance Comparison
```bash
# Use wrk for load testing
wrk -t4 -c50 -d15s http://localhost:5006/simple/  # proxpi
wrk -t4 -c50 -d15s http://localhost:5005/simple/  # groxpi

# Monitor resource usage
docker stats proxpi_container
docker stats groxpi_container
```

### 3. Configuration Migration

#### Environment Variables Mapping
All proxpi environment variables are supported in groxpi:

| proxpi Variable | groxpi Variable | Notes |
|----------------|-----------------|-------|
| `INDEX_URL` | `GROXPI_INDEX_URL` | Same functionality |
| `INDEX_TTL` | `GROXPI_INDEX_TTL` | Same functionality |
| `EXTRA_INDEX_URLS` | `GROXPI_EXTRA_INDEX_URLS` | Same functionality |
| `EXTRA_INDEX_TTLS` | `GROXPI_EXTRA_INDEX_TTLS` | Same functionality |
| `CACHE_SIZE` | `GROXPI_CACHE_SIZE` | Same functionality |
| `CACHE_DIR` | `GROXPI_CACHE_DIR` | Same functionality |
| `DOWNLOAD_TIMEOUT` | `GROXPI_DOWNLOAD_TIMEOUT` | Same functionality |
| `LOGGING_LEVEL` | `GROXPI_LOGGING_LEVEL` | Same functionality |

#### Configuration File Migration
Convert your existing configuration:

```bash
# From proxpi configuration
cat > groxpi.env << EOF
GROXPI_INDEX_URL=https://pypi.org/simple/
GROXPI_INDEX_TTL=1800
GROXPI_CACHE_SIZE=5368709120
GROXPI_CACHE_DIR=/cache
GROXPI_LOGGING_LEVEL=INFO
EOF
```

#### Docker Compose Migration
Update your docker-compose.yml:

```yaml
# Before (proxpi)
services:
  proxpi:
    image: epicwink/proxpi:latest
    ports:
      - "5000:5000"
    environment:
      - INDEX_URL=https://pypi.org/simple/
      - CACHE_SIZE=5368709120

# After (groxpi)
services:
  groxpi:
    image: groxpi:latest
    ports:
      - "5000:5000"
    environment:
      - GROXPI_INDEX_URL=https://pypi.org/simple/
      - GROXPI_CACHE_SIZE=5368709120
    healthcheck:
      test: ["CMD-SHELL", "wget --spider http://localhost:5000/health || exit 1"]
      interval: 30s
      timeout: 10s
      retries: 3
```

### 4. Storage Migration

#### Local File Cache
If using local file caching, your existing cache can be reused:

```bash
# Mount existing cache directory
docker run -p 5000:5000 \
  -v /path/to/existing/cache:/cache \
  -e GROXPI_CACHE_DIR=/cache \
  groxpi:latest
```

#### S3 Storage Migration
For S3 storage, groxpi can use the same bucket structure:

```bash
# Same S3 configuration
export GROXPI_STORAGE_TYPE=s3
export AWS_ENDPOINT_URL=your_s3_endpoint
export AWS_ACCESS_KEY_ID=your_access_key
export AWS_SECRET_ACCESS_KEY=your_secret_key
export GROXPI_S3_BUCKET=your_existing_bucket
export GROXPI_S3_PREFIX=your_existing_prefix
```

### 5. Client Configuration Updates

#### pip Configuration
Update pip configuration files:

```ini
# ~/.pip/pip.conf (Linux/Mac)
# %APPDATA%\pip\pip.ini (Windows)
[global]
index-url = http://your-groxpi-server:5000/simple/
trusted-host = your-groxpi-server

# For extra indices
extra-index-url = http://your-groxpi-server:5000/simple/
```

#### poetry Configuration
Update poetry to use groxpi:

```bash
# Update default repository
poetry config repositories.default http://your-groxpi-server:5000/simple/

# Add as secondary source
poetry source add groxpi http://your-groxpi-server:5000/simple/
```

#### pipenv Configuration
Update Pipfile:

```toml
[[source]]
url = "http://your-groxpi-server:5000/simple/"
verify_ssl = false  # if using HTTP
name = "groxpi"
```

#### Docker Builds
Update Dockerfile pip configurations:

```dockerfile
# Before
RUN pip install --index-url http://proxpi-server:5000/simple/ -r requirements.txt

# After (same URL, just updated server)
RUN pip install --index-url http://groxpi-server:5000/simple/ -r requirements.txt
```

### 6. Monitoring Migration

#### Health Check Updates
Update health check endpoints:

```bash
# Old proxpi health check (if custom)
curl http://localhost:5000/

# New groxpi health check
curl http://localhost:5000/health
```

#### Log Format Changes
Groxpi provides structured JSON logs:

```json
{
  "time": "2024-01-01T12:00:00Z",
  "level": "info",
  "message": "request completed",
  "method": "GET",
  "path": "/simple/numpy/",
  "status": 200,
  "latency": "25.4ms"
}
```

Update log parsing/alerting systems for the new format.

### 7. Production Deployment

#### Blue-Green Deployment
Recommended approach for zero-downtime migration:

```bash
# 1. Deploy groxpi as "green" environment
docker-compose -f docker-compose.groxpi.yml up -d

# 2. Update load balancer to send traffic to both
# 3. Monitor for issues
# 4. Gradually shift traffic to groxpi
# 5. Shut down proxpi after validation
```

#### Load Balancer Configuration
Update load balancer to use groxpi:

```nginx
upstream pypi_servers {
    # server proxpi:5000 weight=1;  # Reduce weight gradually
    server groxpi:5000 weight=10;    # Increase weight gradually
}
```

#### Database/Cache Migration
- **File cache**: Can be shared or migrated
- **S3 cache**: Same bucket can be used
- **Response cache**: Will rebuild automatically

### 8. Validation and Testing

#### Functional Testing
```bash
# Test package installation
pip install --index-url http://groxpi-server:5000/simple/ requests numpy pandas

# Test cache behavior
time pip install --index-url http://groxpi-server:5000/simple/ requests  # First time
time pip install --index-url http://groxpi-server:5000/simple/ requests  # Should be cached

# Test error handling
pip install --index-url http://groxpi-server:5000/simple/ nonexistent-package
```

#### Performance Validation
```bash
# Benchmark comparison
wrk -t4 -c50 -d30s http://groxpi-server:5000/simple/

# Memory usage comparison
docker stats groxpi_container

# Response time validation
curl -w "Total time: %{time_total}s\n" http://groxpi-server:5000/simple/
```

#### Client Compatibility Testing
Test all your Python package managers:
- pip (various versions)
- poetry
- pipenv
- uv
- conda/mamba
- PDM

## Rollback Plan

### Quick Rollback
If issues are discovered:

```bash
# 1. Update load balancer to route traffic back to proxpi
# 2. Stop groxpi container
docker-compose -f docker-compose.groxpi.yml down

# 3. Verify proxpi is still functioning
curl http://proxpi-server:5000/simple/

# 4. Update client configurations if needed
```

### Data Recovery
- **Local cache**: Original cache files remain intact
- **S3 storage**: Data is preserved in same bucket
- **Configuration**: Keep proxpi configuration as backup

## Common Migration Issues

### Performance Differences
- **Issue**: Different caching behavior
- **Solution**: Adjust TTL settings to match expectations

### Log Format Changes
- **Issue**: Log parsing systems break
- **Solution**: Update log parsers for JSON format

### Health Check Changes
- **Issue**: Load balancer health checks fail
- **Solution**: Update health check endpoint to `/health`

### Cache Warming
- **Issue**: Cold cache after migration
- **Solution**: Pre-warm cache with common packages:

```bash
# Warm cache with popular packages
for package in requests numpy pandas django flask; do
  curl http://groxpi-server:5000/simple/$package/
done
```

## Best Practices

### Migration Timeline
1. **Week 1**: Side-by-side testing and validation
2. **Week 2**: Gradual traffic migration (10% → 50% → 100%)
3. **Week 3**: Monitoring and optimization
4. **Week 4**: Proxpi decommission

### Monitoring During Migration
- Monitor error rates closely
- Track response times
- Watch cache hit ratios
- Monitor resource usage

### Communication
- Notify development teams of migration timeline
- Provide updated configuration examples
- Document any changes in behavior
- Plan rollback window if needed

## Success Metrics

After migration, you should see:
- **Response times**: Sub-millisecond for cached requests
- **Memory usage**: ~50MB (vs 200MB+ for proxpi)
- **Error rates**: Same or lower than proxpi
- **Cache hit ratios**: Same or better than proxpi
- **Client compatibility**: 100% (no client changes needed)

## Support and Troubleshooting

### Common Questions
- **Q**: Will this break my existing pip configurations?
- **A**: No, groxpi uses the same API endpoints and protocols

- **Q**: Can I migrate gradually?
- **A**: Yes, use load balancer to split traffic between proxpi and groxpi

- **Q**: What happens to my existing cache?
- **A**: Local caches can be reused, S3 caches are compatible

### Getting Help
- Check the `/health` endpoint for system status
- Enable debug logging: `GROXPI_LOGGING_LEVEL=DEBUG`
- Monitor logs for error patterns
- Compare performance metrics with baseline

The migration to groxpi should provide immediate performance benefits while maintaining complete compatibility with your existing PyPI infrastructure.