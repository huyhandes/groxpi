# Benchmark Troubleshooting

Quick solutions for common benchmark issues.

## Connection Issues

### Services Not Responding
```bash
# Check service status
docker ps --filter "name=bench"
docker logs groxpi-bench
docker logs proxpi-bench

# Test connectivity
curl -f http://localhost:5005/health
curl -f http://localhost:5006/
```

### Network Problems
```bash
# Verify network
docker network inspect benchmark-network

# Test container connectivity
docker exec groxpi-bench ping -c 3 proxpi-bench
```

## Performance Issues

### Low Performance
- Check system load: `top` or `htop`
- Increase Docker resources (CPU/Memory)
- Restart Docker: `sudo systemctl restart docker`

### High Error Rates
```bash
# Reduce load
export WRK_CONNECTIONS=50
export WRK_THREADS=4

# Increase timeouts
export WRK_TIMEOUT=60
export DOCKER_TIMEOUT=1200
```

### Installation Timeouts
```bash
# Increase UV timeouts
export DOCKER_TIMEOUT=1800
export INSTALL_TIMEOUT=900

# Test with lightweight packages
export TEST_PACKAGES="requests click"
```

## Data Issues

### Missing CSV Data
```bash
# Check file permissions
ls -la results/
chmod 644 results/*.csv

# Validate CSV format
head -5 results/wrk-summary-*.csv
```

### DuckDB Analysis Errors
```bash
# Test CSV compatibility
duckdb -c "SELECT COUNT(*) FROM read_csv('results/wrk-summary-*.csv', header=true)"
```

## Container Issues

### Container Not Found
```bash
# Check actual container names
docker ps --format "table {{.Names}}\t{{.Status}}"

# Update container names if needed
export GROXPI_CONTAINER="actual-name"
export PROXPI_CONTAINER="actual-name"
```

### Resource Monitoring Issues
```bash
# Test monitoring manually
docker stats --no-stream groxpi-bench proxpi-bench

# Fix script syntax errors
./scripts/monitor_resources.sh results/test.csv test1 test2 1
```

## Quick Recovery

### Complete Reset
```bash
# Stop and clean everything
docker-compose -f docker/docker-compose.benchmark.yml down
docker system prune -a --volumes

# Rebuild and start
docker-compose -f docker/docker-compose.benchmark.yml up -d --build
```

### Service Restart
```bash
# Restart specific services
docker-compose -f docker/docker-compose.benchmark.yml restart groxpi-bench
docker-compose -f docker/docker-compose.benchmark.yml restart proxpi-bench
```

## Validation Checklist

Before running benchmarks:
- [ ] Services respond to health checks
- [ ] Cache APIs return valid responses
- [ ] Docker network exists and containers can communicate
- [ ] Sufficient disk space for results
- [ ] DuckDB installed for analysis

## Expected Performance Ranges

**Red Flags:**
- groxpi RPS < 10,000
- Improvement ratio < 5:1
- Error rate > 5%
- High result variability (>20%)

**Normal Ranges:**
- groxpi: 15,000-20,000 RPS
- proxpi: 800-1,200 RPS
- Latency improvement: 15-40x
- Installation improvement: 30-50%