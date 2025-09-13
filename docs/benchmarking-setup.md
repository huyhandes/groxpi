# Benchmark Setup and Configuration

Setup guide for the groxpi benchmarking environment.

## Prerequisites

### System Requirements
- **Docker**: Version 20.10+ with Docker Compose
- **Memory**: 8GB+ RAM (16GB recommended)
- **Disk**: 20GB available space
- **Network**: Internet access for PyPI packages

### Required Tools
```bash
# Docker and Docker Compose
curl -fsSL https://get.docker.com -o get-docker.sh
sh get-docker.sh

# DuckDB for analysis
brew install duckdb  # macOS
sudo apt-get install duckdb  # Linux
```

## Quick Setup

### 1. Environment
```bash
cd groxpi/benchmarks

# Set server URLs
export GROXPI_URL=http://localhost:5005
export PROXPI_URL=http://localhost:5006
```

### 2. Start Services
```bash
# Local services
docker-compose -f docker/docker-compose.benchmark.yml up -d

# Verify health
curl -f http://localhost:5005/health
curl -f http://localhost:5006/
```

### 3. Run Benchmarks
```bash
./benchmark.sh --groxpi-url $GROXPI_URL --proxpi-url $PROXPI_URL
```

## Configuration

### Service Configuration
Both services use equivalent settings for fair comparison:
- Cache Size: 5GB
- Index TTL: 30 minutes
- PyPI upstream: https://pypi.org/simple/

### Test Parameters
```bash
# API Testing (WRK)
WRK_THREADS=8
WRK_CONNECTIONS=100
WRK_DURATION=60

# Package Testing (UV)
DOCKER_TIMEOUT=600
INSTALL_TIMEOUT=300
TEST_PACKAGES=("numpy" "pandas" "polars" "pyspark" "fastapi")

# Resource Monitoring
MONITOR_INTERVAL=2
```

## Deployment Scenarios

### Local Testing
```bash
docker-compose -f docker/docker-compose.benchmark.yml up -d
./benchmark.sh --groxpi-url http://localhost:5005 --proxpi-url http://localhost:5006
```

### Remote Servers
```bash
./benchmark.sh \
  --groxpi-url http://server1:5005 \
  --proxpi-url http://server2:5006
```

### CI/CD Integration
```bash
export GROXPI_URL=$CI_GROXPI_URL
export PROXPI_URL=$CI_PROXPI_URL
./benchmark.sh --api-only  # Faster for CI
```

## Directory Structure
```
benchmarks/
├── benchmark.sh              # Main orchestrator
├── docker/                   # Container configs
├── scripts/                  # Test components
└── results/                  # Output directory
```

## Validation Checklist
- [ ] Services respond to health checks
- [ ] Cache APIs accessible
- [ ] Container networking working
- [ ] Sufficient disk space
- [ ] DuckDB installed

## Quick Validation
```bash
./scripts/cache_manager.sh $GROXPI_URL $PROXPI_URL test-connection
```