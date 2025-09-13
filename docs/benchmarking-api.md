# API Performance Benchmarking

HTTP API performance testing using WRK to compare groxpi vs proxpi response times and throughput.

## Overview

Measures HTTP request performance to validate groxpi's performance improvements over proxpi.

### Key Metrics
- **Requests per Second (RPS)**: Total throughput capacity
- **Latency**: P50 (median) and P99 response times
- **Cache Performance**: Cold vs warm cache scenarios

## Test Scenarios

### Load Testing
- **Index Tests**: `/simple/` endpoint performance
- **Package Tests**: `/simple/numpy/` specific package lookups
- **Cache States**: Cold (empty) vs Warm (populated) cache
- **Load**: 8 threads, 100 connections, 60 seconds

### Single Request Tests
- Cold cache response time measurement
- Individual request latency validation

## Usage

### Run API Tests
```bash
# Complete API testing
./scripts/wrk_api_test.sh \
    http://localhost:5005 \
    http://localhost:5006 \
    20240101_120000 \
    all

# Specific scenarios
./scripts/wrk_api_test.sh ... load-cold
./scripts/wrk_api_test.sh ... load-warm

# Via orchestrator
./benchmark.sh --api-only
```

### Available Scenarios
- `all` - All test scenarios
- `load-cold` - Load test with cold cache
- `load-warm` - Load test with warm cache
- `single-cold` - Single request tests

## Configuration

### Default WRK Parameters
```bash
WRK_THREADS=8          # Number of threads
WRK_CONNECTIONS=100    # Concurrent connections
WRK_DURATION=60        # Test duration in seconds
WRK_TIMEOUT=30         # Request timeout
```

### Load Profiles
```bash
# Light load
export WRK_THREADS=2 WRK_CONNECTIONS=25 WRK_DURATION=30

# Heavy load
export WRK_THREADS=32 WRK_CONNECTIONS=1000 WRK_DURATION=300
```

## Cache Management

Cache clearing is automatic between test scenarios:
```bash
curl -X DELETE http://localhost:5005/cache/list  # groxpi
curl -X DELETE http://localhost:5006/cache/list  # proxpi
```

## Output Format

### CSV Results (`wrk-summary-{timestamp}.csv`)
```csv
run_timestamp,test_datetime,service,test_type,test_name,endpoint,requests_per_sec,avg_latency_ms,p50_latency_ms,p99_latency_ms
test_20240101_120000,2024-01-01 12:00:00,groxpi,load,index-warm,simple/,15420.5,2.45,2.1,8.9
test_20240101_120000,2024-01-01 12:05:00,proxpi,load,index-warm,simple/,987.3,45.2,42.1,125.7
```

## Expected Performance

### Requests per Second
- **groxpi**: 15,000-20,000 RPS
- **proxpi**: 800-1,200 RPS
- **Improvement**: 15-20x faster

### Latency
- **groxpi P50**: 1-3ms
- **proxpi P50**: 40-60ms
- **Improvement**: 20-40x faster

## Troubleshooting

### High Error Rates
```bash
# Reduce load
export WRK_CONNECTIONS=50 WRK_THREADS=4

# Check service health
curl -f http://localhost:5005/health
```

### Inconsistent Results
```bash
# Clean environment
docker-compose restart && sleep 30

# Multiple test runs
for i in {1..3}; do
    ./scripts/wrk_api_test.sh ... load-warm
    sleep 60
done
```