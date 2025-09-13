# Package Installation Benchmarking

Python package installation performance testing using UV package manager to compare groxpi vs proxpi download and installation speeds.

## Overview

Measures real-world package installation performance to validate groxpi improvements in developer workflows and CI/CD scenarios.

### Key Metrics
- **Installation Time**: Total time from command start to completion
- **Cache Efficiency**: Cold vs warm cache performance
- **Package Size**: Installed sizes and dependency counts
- **Success Rates**: Installation reliability

## Test Scenarios

### Individual Package Testing
- **Cold Cache**: Empty cache installation timing
- **Warm Cache**: Pre-populated cache installation timing
- **Packages**: numpy, pandas, polars, pyspark, fastapi

### Batch Installation Testing
- **Small Batch**: 5 typical packages
- **Large Batch**: Full dependency stacks

### Package Categories
- **Lightweight**: fastapi, requests, click
- **Medium**: numpy, pandas, polars
- **Heavy**: pyspark, tensorflow, pytorch

## Usage

### Run Package Tests
```bash
# Complete package testing
./scripts/uv_install_test.sh \
    http://localhost:5005 \
    http://localhost:5006 \
    20240101_120000 \
    all

# Specific scenarios
./scripts/uv_install_test.sh ... cold-install
./scripts/uv_install_test.sh ... warm-install

# Via orchestrator
./benchmark.sh --uv-only
```

### Available Scenarios
- `all` - All test scenarios
- `cold-install` - Individual packages with cold cache
- `warm-install` - Individual packages with warm cache
- `batch-install` - Batch installation tests

## Configuration

### UV Container Setup
```bash
DOCKER_NETWORK="benchmark-network"
UV_IMAGE="ghcr.io/astral-sh/uv:latest"
DOCKER_TIMEOUT=600  # 10 minutes for large packages
```

### pyproject.toml Template
Uses dynamic index endpoint replacement:
```toml
[[tool.uv.index]]
name = "custom-index"
url = "http://<index_endpoint>/simple/"
default = true
```

## Output Format

### CSV Results (`uv-summary-{timestamp}.csv`)
```csv
run_timestamp,test_datetime,service,test_name,cache_mode,package,install_time_seconds,install_success,installed_size_mb,dependency_count
test_20240101_120000,2024-01-01 12:30:00,groxpi,single-numpy,cold,numpy,12.45,TRUE,15.2,8
test_20240101_120000,2024-01-01 12:35:00,proxpi,single-numpy,cold,numpy,28.76,TRUE,15.2,8
```

## Expected Performance

### Installation Time Improvements
- **groxpi**: 30-50% faster installation
- **numpy**: 12s vs 29s (60% improvement)
- **pandas**: 18s vs 42s (57% improvement)
- **pyspark**: 45s vs 98s (54% improvement)

### Cache Benefits
- Cold to warm cache: 60-80% time savings
- Consistent cache hit performance

## Cache Management

Automatic cache clearing between test scenarios:
```bash
curl -X DELETE http://localhost:5005/cache/list  # groxpi
curl -X DELETE http://localhost:5006/cache/list  # proxpi
```

## Custom Package Testing

### Add Custom Packages
```bash
# Edit TEST_PACKAGES in uv_install_test.sh
TEST_PACKAGES=("requests" "flask" "sqlalchemy")
```

### Domain-specific Stacks
```bash
# Web development
WEB_PACKAGES=("fastapi" "uvicorn" "pydantic")

# Data science
DATA_PACKAGES=("numpy" "pandas" "matplotlib")
```

## Troubleshooting

### Installation Timeouts
```bash
# Increase timeouts
export DOCKER_TIMEOUT=1800  # 30 minutes
export INSTALL_TIMEOUT=600  # 10 minutes per package
```

### Network Issues
```bash
# Test connectivity
docker run --rm --network=benchmark-network alpine:latest \
    ping -c 3 groxpi-bench
```

### Package Failures
```bash
# Check service logs
docker logs groxpi-bench
docker logs proxpi-bench

# Test manually
docker run --rm -it --network=benchmark-network $UV_IMAGE bash
```