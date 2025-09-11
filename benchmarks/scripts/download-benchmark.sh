#!/bin/bash

# Download Benchmark Script using UV
# Tests real package download performance of groxpi vs proxpi

set -euo pipefail

# Configuration
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
RESULTS_DIR="$(dirname "$0")/../results"
DOCKER_DIR="$(dirname "$0")/../docker"

# Test packages - small to very large
TEST_PACKAGES=("requests" "numpy" "pandas" "pyspark" "polars" "fastapi")

# Cache test configuration
CACHE_TEST_PACKAGE="numpy"  # Package to test cache performance

# Ensure results directory exists
mkdir -p "$RESULTS_DIR"

# Function to wait for services
wait_for_services() {
    echo "Waiting for services to be healthy..."
    
    # Wait for groxpi
    for i in {1..30}; do
        if curl -sf "http://localhost:5005/health" > /dev/null 2>&1; then
            echo "groxpi is healthy!"
            break
        fi
        if [ $i -eq 30 ]; then
            echo "ERROR: groxpi failed to start"
            return 1
        fi
        sleep 2
    done
    
    # Wait for proxpi
    for i in {1..30}; do
        if curl -sf "http://localhost:5006/" > /dev/null 2>&1; then
            echo "proxpi is healthy!"
            break
        fi
        if [ $i -eq 30 ]; then
            echo "ERROR: proxpi failed to start"
            return 1
        fi
        sleep 2
    done
}

# Function to get container stats
get_container_stats() {
    local container_name=$1
    local service_name=$2
    local phase=$3
    local stats_file="$RESULTS_DIR/download-stats-${service_name}-${phase}-${TIMESTAMP}.log"
    
    echo "Container stats for $service_name ($phase):"
    docker stats --no-stream --format "  CPU: {{.CPUPerc}} | Memory: {{.MemUsage}} ({{.MemPerc}})" "$container_name" 2>/dev/null | tee "$stats_file" || echo "  Stats unavailable"
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
    wait_for_services
}

# Function to run UV benchmark in container
run_uv_benchmark() {
    local service=$1
    local config_file=$2
    local cache_mode=$3  # "cold" or "warm"
    local output_file="$RESULTS_DIR/download-${service}-${cache_mode}-${TIMESTAMP}.log"
    
    echo "Testing $service with UV package installer (${cache_mode} cache)..."
    
    # Get baseline container stats
    if [ "$service" == "groxpi" ]; then
        get_container_stats "groxpi-bench" "$service" "${cache_mode}-before"
    else
        get_container_stats "proxpi-bench" "$service" "${cache_mode}-before"
    fi
    
    # Run tests in container
    docker run --rm \
        --network "docker_benchmark-network" \
        -v "$DOCKER_DIR/uv:/workspace" \
        -w /workspace \
        uv-tester:latest /bin/bash -c "
        
        # Copy the appropriate config
        cp $config_file pyproject.toml
        
        echo '=== UV Download Benchmark - $service ($cache_mode cache) ==='
        echo 'Test packages: ${TEST_PACKAGES[*]}'
        echo 'Timestamp: $TIMESTAMP'
        echo 'Cache mode: $cache_mode'
        echo ''
        
        if [ '$cache_mode' == 'cold' ]; then
            echo '=== COLD CACHE TEST ==='
            echo 'Testing single package installation without cache...'
            echo ''
            
            # Test only the cache test package
            package='$CACHE_TEST_PACKAGE'
            echo \"Testing \$package (cold cache)...\"
            
            # Create clean virtual environment
            rm -rf .venv
            uv venv --quiet
            
            # Time the installation
            start_time=\$(date +%s.%N)
            if timeout 300 uv add \$package --quiet 2>&1; then
                end_time=\$(date +%s.%N)
                duration=\$(echo \"\$end_time - \$start_time\" | bc -l)
                printf \"  ✓ Cold cache - \$package: %.2fs\\n\" \$duration
                
                # Get installed package size
                if [ -d .venv/lib/python*/site-packages ]; then
                    size=\$(du -sh .venv/lib/python*/site-packages 2>/dev/null | cut -f1 || echo 'unknown')
                    echo \"  Package size: \$size\"
                fi
            else
                echo \"  ✗ Cold cache - \$package: FAILED\"
            fi
            echo \"\"
        else
            echo '=== WARM CACHE TEST ==='
            echo 'Pre-warming cache and testing multiple installations...'
            echo ''
            
            # First warm up the cache
            echo 'Warming up cache...'
            for pkg in ${TEST_PACKAGES[@]}; do
                rm -rf .venv
                uv venv --quiet
                uv add \$pkg --quiet 2>&1 || true
            done
            echo 'Cache warmed up!'
            echo ''
            
            # Test each package with warm cache
            for package in ${TEST_PACKAGES[@]}; do
                echo \"=== Testing \$package (warm cache) ===\"
                
                # Create clean virtual environment
                rm -rf .venv
                uv venv --quiet
                
                # Time the installation
                start_time=\$(date +%s.%N)
                if timeout 300 uv add \$package --quiet 2>&1; then
                    end_time=\$(date +%s.%N)
                    duration=\$(echo \"\$end_time - \$start_time\" | bc -l)
                    printf \"  ✓ Warm cache - \$package: %.2fs\\n\" \$duration
                    
                    # Get installed package size
                    if [ -d .venv/lib/python*/site-packages ]; then
                        size=\$(du -sh .venv/lib/python*/site-packages 2>/dev/null | cut -f1 || echo 'unknown')
                        echo \"  Package size: \$size\"
                    fi
                else
                    echo \"  ✗ Warm cache - \$package: FAILED\"
                fi
                echo \"\"
            done
        fi
        
        if [ '$cache_mode' == 'warm' ]; then
            echo '=== Cache Speedup Comparison ==='
            echo 'Comparing cold vs warm cache performance...'
            
            # Test cache speedup for the cache test package
            echo ''
            echo \"Cache speedup test: $CACHE_TEST_PACKAGE\"
            rm -rf .venv
            uv venv --quiet
            
            echo '  Install with warm cache (should be fast):'
            start_time=\$(date +%s.%N)
            uv add $CACHE_TEST_PACKAGE --quiet 2>&1
            end_time=\$(date +%s.%N)
            duration_warm=\$(echo \"\$end_time - \$start_time\" | bc -l)
            printf \"    Time: %.2fs\\n\" \$duration_warm
            
            echo ''
            echo '=== Parallel Installation Test ==='
            echo 'Installing multiple packages simultaneously...'
            rm -rf .venv
            uv venv --quiet
            
            start_time=\$(date +%s.%N)
            if timeout 300 uv add requests click flask fastapi --quiet 2>&1; then
                end_time=\$(date +%s.%N)
                duration=\$(echo \"\$end_time - \$start_time\" | bc -l)
                printf \"  ✓ Parallel install completed in %.2fs\\n\" \$duration
            else
                echo \"  ✗ Parallel install failed\"
            fi
        fi
        
    " > "$output_file" 2>&1
    
    # Get after container stats
    if [ "$service" == "groxpi" ]; then
        get_container_stats "groxpi-bench" "$service" "${cache_mode}-after"
    else
        get_container_stats "proxpi-bench" "$service" "${cache_mode}-after"
    fi
    
    echo "  Results saved to: $output_file"
    echo ""
}

# Main benchmark execution
main() {
    echo "=== UV Download Benchmark ==="
    echo "Timestamp: $TIMESTAMP"
    echo "Test packages: ${TEST_PACKAGES[*]}"
    echo ""
    
    # Wait for services
    wait_for_services
    
    # Build UV container if needed
    if ! docker images | grep -q uv-tester; then
        echo "Building UV benchmark container..."
        docker build -t uv-tester "$DOCKER_DIR/uv/"
    fi
    
    echo ""
    echo "======================================"
    echo "=== COLD CACHE TESTS (Single Request) ==="
    echo "======================================"
    echo ""
    
    # Clear caches before cold tests
    clear_cache "groxpi"
    clear_cache "proxpi"
    
    # Test groxpi cold
    echo "=== Testing groxpi (COLD cache) ==="
    run_uv_benchmark "groxpi" "pyproject.toml" "cold"
    
    # Clear cache and test proxpi cold
    clear_cache "proxpi"
    echo "=== Testing proxpi (COLD cache) ==="
    run_uv_benchmark "proxpi" "pyproject.proxpi.toml" "cold"
    
    echo ""
    echo "======================================"
    echo "=== WARM CACHE TESTS (Multiple Packages) ==="
    echo "======================================"
    echo ""
    
    # No need to clear cache - we want it warm
    echo "=== Testing groxpi (WARM cache) ==="
    run_uv_benchmark "groxpi" "pyproject.toml" "warm"
    
    echo "=== Testing proxpi (WARM cache) ==="
    run_uv_benchmark "proxpi" "pyproject.proxpi.toml" "warm"
    
    echo "=== Download Benchmark Complete ==="
    echo ""
    
    # Show summary
    echo "COLD Cache Installation Time ($CACHE_TEST_PACKAGE):"
    echo "------------------------------------------------"
    echo -n "  groxpi: "
    grep "✓ Cold cache - $CACHE_TEST_PACKAGE:" "$RESULTS_DIR/download-groxpi-cold-${TIMESTAMP}.log" 2>/dev/null | awk '{print $5}' || echo "N/A"
    echo -n "  proxpi: "
    grep "✓ Cold cache - $CACHE_TEST_PACKAGE:" "$RESULTS_DIR/download-proxpi-cold-${TIMESTAMP}.log" 2>/dev/null | awk '{print $5}' || echo "N/A"
    
    echo ""
    echo "WARM Cache Installation Times:"
    echo "------------------------------"
    for package in "${TEST_PACKAGES[@]}"; do
        echo "$package:"
        echo -n "  groxpi: "
        grep "✓ Warm cache - $package:" "$RESULTS_DIR/download-groxpi-warm-${TIMESTAMP}.log" 2>/dev/null | head -1 | awk '{print $5}' || echo "N/A"
        echo -n "  proxpi: "
        grep "✓ Warm cache - $package:" "$RESULTS_DIR/download-proxpi-warm-${TIMESTAMP}.log" 2>/dev/null | head -1 | awk '{print $5}' || echo "N/A"
    done
    
    echo ""
    echo "Resource Usage:"
    echo "---------------"
    echo "Check $RESULTS_DIR/download-stats-*-${TIMESTAMP}.log for detailed metrics"
    
    echo ""
    echo "Results saved to: $RESULTS_DIR"
}

# Run main function
main "$@"