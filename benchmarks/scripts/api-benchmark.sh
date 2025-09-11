#!/bin/bash

# API Benchmark Script using WRK
# Tests HTTP API performance of groxpi vs proxpi

set -euo pipefail

# Configuration
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
RESULTS_DIR="$(dirname "$0")/../results"
GROXPI_URL="http://localhost:5005"
PROXPI_URL="http://localhost:5006"
WRK_DURATION="60s"
WRK_THREADS=8
WRK_CONNECTIONS=100

# Test packages - popular and large packages
TEST_PACKAGES=("requests" "numpy" "pandas" "pyspark" "polars" "fastapi")

# Cache test configuration
CACHE_TEST_PACKAGE="numpy"  # Package to test cache performance

# Ensure results directory exists
mkdir -p "$RESULTS_DIR"

# Function to wait for service health
wait_for_service() {
    local url=$1
    local service_name=$2
    echo "Waiting for $service_name to be healthy..."
    
    for i in {1..30}; do
        if curl -sf "$url/health" > /dev/null 2>&1 || curl -sf "$url/" > /dev/null 2>&1; then
            echo "$service_name is healthy!"
            return 0
        fi
        echo "Attempt $i/30: $service_name not ready, waiting..."
        sleep 2
    done
    
    echo "ERROR: $service_name failed to become healthy"
    return 1
}

# Function to get container stats
get_container_stats() {
    local container_name=$1
    local service_name=$2
    local phase=$3
    local stats_file="$RESULTS_DIR/stats-${service_name}-${phase}-${TIMESTAMP}.log"
    
    echo "Collecting resource stats for $service_name ($phase)..."
    
    # Get container stats
    docker stats --no-stream --format "table {{.Container}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}" "$container_name" > "$stats_file" 2>&1
    
    # Get detailed memory stats
    docker exec "$container_name" cat /proc/meminfo 2>/dev/null | head -10 >> "$stats_file" 2>&1 || true
    
    # Extract key metrics
    local cpu=$(docker stats --no-stream --format "{{.CPUPerc}}" "$container_name" 2>/dev/null | sed 's/%//' || echo "N/A")
    local mem=$(docker stats --no-stream --format "{{.MemUsage}}" "$container_name" 2>/dev/null || echo "N/A")
    
    echo "  - CPU Usage: ${cpu}%"
    echo "  - Memory: $mem"
}

# Function to clear cache
clear_cache() {
    local service=$1
    echo "Clearing cache for $service..."
    
    # Use docker-compose to properly manage volumes
    local compose_file="$(dirname "$0")/../docker/docker-compose.benchmark.yml"
    
    # Stop the specific service
    docker-compose -f "$compose_file" stop "${service}-bench"
    
    # Remove the specific volume
    docker-compose -f "$compose_file" rm -v -f "${service}-bench" 2>/dev/null || true
    
    # Recreate the service with fresh volume
    docker-compose -f "$compose_file" up -d "${service}-bench"
    
    # Wait for service to be healthy
    echo "Waiting for $service to restart with clean cache..."
    sleep 10
    
    if [ "$service" == "groxpi" ]; then
        wait_for_service "$GROXPI_URL" "groxpi"
    else
        wait_for_service "$PROXPI_URL" "proxpi"
    fi
}

# Function to run WRK benchmark
run_wrk_benchmark() {
    local url=$1
    local endpoint=$2
    local service=$3
    local test_name=$4
    local duration=${5:-$WRK_DURATION}
    local threads=${6:-$WRK_THREADS}
    local connections=${7:-$WRK_CONNECTIONS}
    
    local output_file="$RESULTS_DIR/api-${service}-${test_name}-${TIMESTAMP}.log"
    
    echo "Testing $service - $test_name..."
    
    # Run wrk and capture output
    wrk -t$threads -c$connections -d$duration \
        --timeout 30s \
        "$url$endpoint" > "$output_file" 2>&1
    
    # Extract key metrics
    local rps=$(grep "Requests/sec:" "$output_file" | awk '{print $2}')
    local latency=$(grep "Latency" "$output_file" | awk '{print $2}')
    
    echo "  - Requests/sec: $rps"
    echo "  - Avg Latency: $latency"
    echo ""
}

# Function to run single request benchmark
run_single_request() {
    local url=$1
    local endpoint=$2
    local service=$3
    local test_name=$4
    
    echo "Single request test for $service - $test_name..."
    
    # Time a single request
    local start_time=$(date +%s%N)
    curl -s "$url$endpoint" > /dev/null
    local end_time=$(date +%s%N)
    
    local duration=$((($end_time - $start_time) / 1000000))
    echo "  - Response time: ${duration}ms"
    echo ""
}

# Main benchmark execution
main() {
    echo "=== API Performance Benchmark ==="
    echo "Timestamp: $TIMESTAMP"
    echo "Configuration: ${WRK_DURATION} duration, ${WRK_THREADS} threads, ${WRK_CONNECTIONS} connections"
    echo ""
    
    # Wait for services
    wait_for_service "$GROXPI_URL" "groxpi"
    wait_for_service "$PROXPI_URL" "proxpi"
    
    echo ""
    echo "=== Container Resource Usage (Baseline) ==="
    get_container_stats "groxpi-bench" "groxpi" "baseline"
    get_container_stats "proxpi-bench" "proxpi" "baseline"
    
    echo ""
    echo "======================================"
    echo "=== TEST 1: WITHOUT CACHE (COLD) ==="
    echo "======================================"
    echo ""
    
    # Clear caches
    clear_cache "groxpi"
    clear_cache "proxpi"
    
    echo "Running single request tests (cold cache)..."
    echo ""
    
    # Single request to package index
    echo "1.1 Cold Cache - Package Index (/index/)"
    echo "------------------------------------------"
    run_single_request "$GROXPI_URL" "/index/" "groxpi" "cold-index"
    get_container_stats "groxpi-bench" "groxpi" "cold-index"
    
    clear_cache "proxpi"  # Clear proxpi cache again
    run_single_request "$PROXPI_URL" "/index/" "proxpi" "cold-index"
    get_container_stats "proxpi-bench" "proxpi" "cold-index"
    
    # Single request to specific package
    echo "1.2 Cold Cache - Package Details (/index/$CACHE_TEST_PACKAGE)"
    echo "--------------------------------------------------------------"
    clear_cache "groxpi"
    run_single_request "$GROXPI_URL" "/index/$CACHE_TEST_PACKAGE" "groxpi" "cold-package"
    get_container_stats "groxpi-bench" "groxpi" "cold-package"
    
    clear_cache "proxpi"
    run_single_request "$PROXPI_URL" "/index/$CACHE_TEST_PACKAGE" "proxpi" "cold-package"
    get_container_stats "proxpi-bench" "proxpi" "cold-package"
    
    echo ""
    echo "===================================="
    echo "=== TEST 2: WITH CACHE (WARM) ==="
    echo "===================================="
    echo ""
    
    # Warm up the cache
    echo "Warming up caches..."
    for package in "${TEST_PACKAGES[@]}"; do
        curl -s "$GROXPI_URL/index/$package" > /dev/null
        curl -s "$PROXPI_URL/index/$package" > /dev/null
    done
    echo "Caches warmed up!"
    echo ""
    
    echo "Running load tests (warm cache)..."
    echo ""
    
    # Test 1: Package index with load
    echo "2.1 Warm Cache - Package Index Load Test"
    echo "-----------------------------------------"
    run_wrk_benchmark "$GROXPI_URL" "/index/" "groxpi" "warm-index"
    get_container_stats "groxpi-bench" "groxpi" "warm-index-load"
    
    run_wrk_benchmark "$PROXPI_URL" "/index/" "proxpi" "warm-index"
    get_container_stats "proxpi-bench" "proxpi" "warm-index-load"
    
    # Test 2: Multiple package details with load
    echo "2.2 Warm Cache - Package Details Load Test"
    echo "-------------------------------------------"
    for package in "${TEST_PACKAGES[@]}"; do
        echo "Testing /index/$package"
        run_wrk_benchmark "$GROXPI_URL" "/index/$package" "groxpi" "warm-package-$package"
        run_wrk_benchmark "$PROXPI_URL" "/index/$package" "proxpi" "warm-package-$package"
    done
    
    echo ""
    echo "=== Container Resource Usage (After Full Load Test) ==="
    get_container_stats "groxpi-bench" "groxpi" "final"
    get_container_stats "proxpi-bench" "proxpi" "final"
    
    # Generate comparison summary
    echo ""
    echo "=== Performance Comparison Summary ==="
    echo ""
    echo "COLD CACHE Performance (Single Request):"
    echo "  See timing results above for cold cache response times"
    echo ""
    
    echo "WARM CACHE Performance (Load Test):"
    echo "Package Index:"
    echo -n "  groxpi: "
    grep "Requests/sec:" "$RESULTS_DIR/api-groxpi-warm-index-${TIMESTAMP}.log" 2>/dev/null || echo "N/A"
    echo -n "  proxpi: "
    grep "Requests/sec:" "$RESULTS_DIR/api-proxpi-warm-index-${TIMESTAMP}.log" 2>/dev/null || echo "N/A"
    echo ""
    
    echo "Package Details ($CACHE_TEST_PACKAGE):"
    echo -n "  groxpi: "
    grep "Requests/sec:" "$RESULTS_DIR/api-groxpi-warm-package-$CACHE_TEST_PACKAGE-${TIMESTAMP}.log" 2>/dev/null || echo "N/A"
    echo -n "  proxpi: "
    grep "Requests/sec:" "$RESULTS_DIR/api-proxpi-warm-package-$CACHE_TEST_PACKAGE-${TIMESTAMP}.log" 2>/dev/null || echo "N/A"
    
    echo ""
    echo "Resource Usage Summary:"
    echo "  Cold cache stats: $RESULTS_DIR/stats-*-cold-*-${TIMESTAMP}.log"
    echo "  Warm cache stats: $RESULTS_DIR/stats-*-warm-*-${TIMESTAMP}.log"
    echo "  Final stats: $RESULTS_DIR/stats-*-final-${TIMESTAMP}.log"
    echo ""
    echo "Results saved to: $RESULTS_DIR"
    echo ""
}

# Run main function
main "$@"