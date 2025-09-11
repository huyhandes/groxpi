# groxpi Benchmark Suite

Performance benchmarking for groxpi PyPI caching proxy, comparing against proxpi.

## Quick Start

```bash
# Run all benchmarks (API + Download tests)
./benchmark.sh

# Run only API benchmarks
./benchmark.sh --api-only

# Run only download benchmarks  
./benchmark.sh --download-only
```

## Benchmark Results

### API Performance (WRK)
- **Package Index**: groxpi 40,707 RPS vs proxpi 2.52 RPS (16,000x faster)
- **Package Details**: groxpi 990 RPS vs proxpi 1,590 RPS
- **Response Times**: 25ms vs 8,400ms for package index

### Download Performance (UV)
- Tests real package installations with requests, numpy, pandas
- Measures cache hit performance
- Validates compatibility with package managers

## Structure

```
benchmarks/
├── benchmark.sh         # Main entry point
├── scripts/
│   ├── api-benchmark.sh     # WRK API tests
│   ├── download-benchmark.sh # UV package tests
│   └── run-all.sh          # Orchestrator
├── docker/
│   ├── docker-compose.benchmark.yml
│   └── uv/                 # UV test container
└── results/                # Test outputs
```

## Requirements

- Docker and Docker Compose
- wrk (HTTP benchmarking tool)
- curl, bc

### Installing wrk

```bash
# macOS
brew install wrk

# Ubuntu/Debian
sudo apt-get install wrk

# From source
git clone https://github.com/wg/wrk.git
cd wrk && make
```

## Configuration

Tests are configured for:
- 15 second duration per test
- 4 threads, 50 connections
- Tests against localhost:5005 (groxpi) and localhost:5006 (proxpi)

## Results

All results are saved to `results/` with timestamps:
- `api-{service}-{test}-{timestamp}.log` - Raw WRK output
- `download-{service}-{timestamp}.log` - UV test results
- `benchmark-report-{timestamp}.md` - Summary report