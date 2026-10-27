#!/bin/bash

# UV Package Installation Test Script
# Tests package installation performance using uv in containers
# Usage: ./uv_install_test.sh <groxpi_url> <proxpi_url> <timestamp> [test_scenario]

set -euo pipefail

# Configuration
GROXPI_URL="${1:-}"
PROXPI_URL="${2:-}"
TIMESTAMP="${3:-}"
TEST_SCENARIO="${4:-all}"

# Test packages - vary in size and complexity
TEST_PACKAGES=("numpy" "pandas" "polars" "pyspark" "fastapi")

# Docker configuration
DOCKER_NETWORK="${DOCKER_NETWORK:-benchmark-network}"
UV_IMAGE="${UV_IMAGE:-ghcr.io/astral-sh/uv:python3.12-bookworm-slim}"
DOCKER_TIMEOUT=600  # 10 minutes timeout for large packages

# Failed test runs; any failure makes the script exit nonzero after the summary
FAILURES=0

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Function to print colored output
log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
log_warning() { echo -e "${YELLOW}[WARNING]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Function to show usage
show_usage() {
    echo "Usage: $0 <groxpi_url> <proxpi_url> <timestamp> [test_scenario]"
    echo ""
    echo "Parameters:"
    echo "  groxpi_url     - groxpi server URL (e.g., http://server1:5005)"
    echo "  proxpi_url     - proxpi server URL (e.g., http://server2:5006)"
    echo "  timestamp      - Consistent timestamp for file naming (e.g., 20240101_120000)"
    echo "  test_scenario  - Test scenario to run (default: all)"
    echo ""
    echo "Test Scenarios:"
    echo "  all           - Run all test scenarios"
    echo "  cold-install  - Cold cache individual package installs"
    echo "  warm-install  - Warm cache individual package installs"
    echo "  batch-install - Batch installation tests"
    echo "  large-package - Large package (pyspark) specific tests"
    echo ""
    echo "Test Packages: ${TEST_PACKAGES[*]}"
    echo ""
    echo "Environment Variables:"
    echo "  DOCKER_NETWORK - Docker network name (default: benchmark-network)"
    echo "  UV_IMAGE       - UV container image (default: ghcr.io/astral-sh/uv:python3.12-bookworm-slim)"
    echo ""
    echo "Examples:"
    echo "  $0 http://server1:5005 http://server2:5006 20240101_120000"
    echo "  $0 http://server1:5005 http://server2:5006 20240101_120000 cold-install"
}

# Function to check dependencies
check_dependencies() {
    if ! command -v docker >/dev/null 2>&1; then
        log_error "Docker is not installed or not in PATH"
        exit 1
    fi

    # Pull UV image if not available locally
    if ! docker images --format "{{.Repository}}:{{.Tag}}" | grep -q "^${UV_IMAGE}$"; then
        log_info "Pulling UV image: $UV_IMAGE"
        if docker pull "$UV_IMAGE"; then
            log_success "UV image pulled successfully"
        else
            log_error "Failed to pull UV image: $UV_IMAGE"
            exit 1
        fi
    else
        log_info "Using UV image: $UV_IMAGE"
    fi
}

# Function to validate inputs
validate_inputs() {
    if [ -z "$GROXPI_URL" ] || [ -z "$PROXPI_URL" ] || [ -z "$TIMESTAMP" ]; then
        show_usage
        exit 1
    fi

    # Remove trailing slashes
    GROXPI_URL="${GROXPI_URL%/}"
    PROXPI_URL="${PROXPI_URL%/}"

    # Validate timestamp format (YYYYMMDD_HHMMSS)
    if [[ ! $TIMESTAMP =~ ^[0-9]{8}_[0-9]{6}$ ]]; then
        log_error "Invalid timestamp format. Expected: YYYYMMDD_HHMMSS (e.g., 20240101_120000)"
        exit 1
    fi
}

# Function to test server connectivity
test_server_connection() {
    local url=$1
    local name=$2

    log_info "Testing connection to $name ($url)..."

    if curl -sf --connect-timeout 5 --max-time 10 "$url/health" >/dev/null 2>&1 || \
       curl -sf --connect-timeout 5 --max-time 10 "$url/" >/dev/null 2>&1; then
        log_success "$name is reachable"
        return 0
    else
        log_error "$name is not reachable ($url)"
        return 1
    fi
}

# Empty one proxy's cache (groxpi or proxpi). Only the proxy about to run cold is
# recreated: clearing both would wipe the other's cache before its warm run.
clear_cache() {
    local target=$1
    local cache_manager_script="$(dirname "$0")/cache_manager.sh"

    if [ ! -f "$cache_manager_script" ]; then
        log_error "cache_manager.sh not found at: $cache_manager_script"
        return 1
    fi

    log_info "Clearing $target cache..."
    if "$cache_manager_script" "$GROXPI_URL" "$PROXPI_URL" "clear-$target"; then
        log_success "$target cache cleared"
        sleep 3  # Wait for cache clear to take effect
        return 0
    else
        log_error "Failed to clear caches"
        return 1
    fi
}

# Map a host-local URL to one the uv container can reach: inside the container localhost is the
# container itself, so localhost/127.0.0.1 become host.docker.internal (resolved via --add-host
# in docker run). Any other host is left alone.
container_url() {
    local url=$1
    if [[ $url =~ ^([a-zA-Z][a-zA-Z0-9+.-]*://)([^/@]*@)?(localhost|127\.0\.0\.1)([:/].*)?$ ]]; then
        printf '%s%shost.docker.internal%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[4]}"
    else
        printf '%s\n' "$url"
    fi
}

# Function to create pyproject.toml from template with dynamic index endpoint
create_pyproject_config() {
    local server_url=$1
    local service_name=$2
    local config_file=$3

    # Path to the template pyproject.toml
    local template_file="$(dirname "$0")/../docker/uv/pyproject.toml"

    if [ ! -f "$template_file" ]; then
        log_error "Template pyproject.toml not found at: $template_file"
        return 1
    fi

    # Copy template and replace the index endpoint placeholder
    sed "s|<index_endpoint>|$server_url|g" "$template_file" > "$config_file"

    log_info "Created pyproject.toml for $service_name from template: $config_file"
}

# Function to run UV installation test
run_uv_install_test() {
    local server_url=$1
    local service_name=$2
    local test_name=$3
    local packages_list=$4
    local results_dir=$5
    local cache_mode=$6  # "cold" or "warm"

    local output_file="$results_dir/uv-${service_name}-${test_name}-${TIMESTAMP}.log"
    local temp_dir=$(mktemp -d)
    local config_file="$temp_dir/pyproject.toml"

    # Create configuration; the container needs a URL it can reach
    server_url=$(container_url "$server_url")
    create_pyproject_config "$server_url" "$service_name" "$config_file"

    log_info "UV test: $service_name - $test_name ($cache_mode cache)"
    log_info "  Packages: $packages_list"

    # Test script for the container, written to $temp_dir/run.sh
    local test_script="#!/bin/bash
set -euo pipefail

echo '=== UV Installation Test - $service_name ($test_name) ==='
echo 'Server URL: $server_url'
echo 'Cache mode: $cache_mode'
echo 'Packages: $packages_list'
echo 'Timestamp: $TIMESTAMP'
echo 'Test start: '\$(date '+%Y-%m-%d %H:%M:%S')
echo ''

# uv add writes to pyproject.toml and uv.lock; keep the template so every install
# starts from it instead of re-resolving the packages added before.
cp pyproject.toml pyproject.toml.template

# Fresh project: template pyproject.toml, no lock file, empty virtual environment
reset_project() {
    cp pyproject.toml.template pyproject.toml
    rm -f uv.lock
    rm -rf .venv
    uv venv --quiet
}

# Function to install and time a package
install_package() {
    local pkg=\$1
    echo \"=== Installing \$pkg ===\"

    reset_project

    # Record start time
    local start_time=\$(date +%s.%N)

    # Install package with timeout
    echo \"Starting installation of \$pkg...\"
    if timeout 300 uv add \$pkg --quiet; then
        local end_time=\$(date +%s.%N)
        local duration=\$(awk -v s=\"\$start_time\" -v e=\"\$end_time\" 'BEGIN { print e - s }')

        printf \"✓ \$pkg installed successfully in %.2fs\\n\" \$duration

        # Get package size information
        if [ -d .venv/lib/python*/site-packages ]; then
            local size=\$(du -sh .venv/lib/python*/site-packages 2>/dev/null | cut -f1 || echo 'unknown')
            echo \"  Installed size: \$size\"
        fi

        # Get dependency count
        local dep_count=\$(uv pip list --format=freeze 2>/dev/null | wc -l | tr -d ' ' || echo 'unknown')
        echo \"  Dependencies installed: \$dep_count\"

        return 0
    else
        local end_time=\$(date +%s.%N)
        local duration=\$(awk -v s=\"\$start_time\" -v e=\"\$end_time\" 'BEGIN { print e - s }')

        printf \"✗ \$pkg installation failed after %.2fs\\n\" \$duration
        return 1
    fi
    echo ''
}

# Function to install multiple packages at once
install_batch() {
    local packages=\"\$1\"
    echo \"=== Batch Installation ===\"
    echo \"Packages: \$packages\"

    reset_project

    # Record start time
    local start_time=\$(date +%s.%N)

    # Install all packages together
    if timeout 600 uv add \$packages --quiet; then
        local end_time=\$(date +%s.%N)
        local duration=\$(awk -v s=\"\$start_time\" -v e=\"\$end_time\" 'BEGIN { print e - s }')

        printf \"✓ Batch installation completed in %.2fs\\n\" \$duration

        # Get total size
        if [ -d .venv/lib/python*/site-packages ]; then
            local size=\$(du -sh .venv/lib/python*/site-packages 2>/dev/null | cut -f1 || echo 'unknown')
            echo \"  Total installed size: \$size\"
        fi

        # Get dependency count
        local dep_count=\$(uv pip list --format=freeze 2>/dev/null | wc -l | tr -d ' ' || echo 'unknown')
        echo \"  Total dependencies: \$dep_count\"

        return 0
    else
        local end_time=\$(date +%s.%N)
        local duration=\$(awk -v s=\"\$start_time\" -v e=\"\$end_time\" 'BEGIN { print e - s }')

        printf \"✗ Batch installation failed after %.2fs\\n\" \$duration
        return 1
    fi
    echo ''
}

# Parse test type and run appropriate test. A failed install sets rc instead of
# aborting (set -e), so the packages after it are still installed - a warmup that
# stopped early would leave them cold for runs labelled warm.
rc=0
case '$test_name' in
    *batch*)
        # Batch installation
        install_batch '$packages_list' || rc=1
        ;;
    *individual*)
        # Install packages individually
        for pkg in $packages_list; do
            install_package \$pkg || rc=1
        done
        ;;
    *single*)
        # Single package installation
        install_package '$packages_list' || rc=1
        ;;
    *)
        # Default: individual installation
        for pkg in $packages_list; do
            install_package \$pkg || rc=1
        done
        ;;
esac

echo ''
echo '=== Test Summary ==='
echo 'Service: $service_name'
echo 'Test: $test_name'
echo 'Cache mode: $cache_mode'
echo 'Completion time: '\$(date '+%Y-%m-%d %H:%M:%S')
echo ''
exit \$rc
"

    printf '%s\n' "$test_script" > "$temp_dir/run.sh"

    # Execute the test in container
    local status=0
    local host_start
    host_start=$(date +%s)
    docker run --rm \
        --network="$DOCKER_NETWORK" \
        --add-host=host.docker.internal:host-gateway \
        -v "$temp_dir:/workspace" \
        -w /workspace \
        "$UV_IMAGE" \
        timeout "$DOCKER_TIMEOUT" bash /workspace/run.sh > "$output_file" 2>&1 || status=$?

    if [ "$status" -eq 0 ]; then
        log_success "  Test completed successfully"

        # Extract timing information
        local install_times=$(grep "installed successfully in" "$output_file" | awk '{print $6}' | sed 's/s$//')
        if [ -n "$install_times" ]; then
            log_info "  Installation times: $(echo "$install_times" | paste -sd ',' -)"
        fi

        local batch_time=$(grep "Batch installation completed in" "$output_file" | awk '{print $6}' | sed 's/s$//')
        if [ -n "$batch_time" ]; then
            log_info "  Batch installation time: ${batch_time}s"
        fi
    else
        log_error "  Test failed or timed out (exit $status); see $output_file"
        if ! grep -q "^✗ " "$output_file"; then
            printf '✗ %s installation failed after %ss\n' "$test_name" "$(( $(date +%s) - host_start ))" >> "$output_file"
        fi
    fi

    # Cleanup
    rm -rf "$temp_dir"

    return "$status"
}

# Run one test; a failure is counted (and recorded in its log) instead of aborting the run
run_test() {
    run_uv_install_test "$@" || FAILURES=$((FAILURES + 1))
}

# Empty one proxy's cache (groxpi or proxpi), then run one test against it:
#   cold <target> <test> <args...>
# A failed clear is counted and the test skipped, so a warm run is never
# recorded as a cold one.
cold() {
    local target=$1
    shift
    if clear_cache "$target"; then
        "$@"
    else
        FAILURES=$((FAILURES + 1))
    fi
}

# Function to run cold cache installation tests
run_cold_install_tests() {
    local results_dir=$1

    echo ""
    echo "=========================================="
    echo "=== COLD CACHE INSTALLATION TESTS ==="
    echo "=========================================="
    echo ""

    if [ "$TEST_SCENARIO" = "cold-install" ] || [ "$TEST_SCENARIO" = "all" ]; then
        log_info "Running cold cache installation tests..."

        # Test each package individually with cold cache
        for package in "${TEST_PACKAGES[@]}"; do
            log_info "Testing cold installation: $package"

            # Empty groxpi only, then test it
            cold groxpi run_test "$GROXPI_URL" "groxpi" "single-$package" "$package" "$results_dir" "cold"

            # Empty proxpi only, then test it
            cold proxpi run_test "$PROXPI_URL" "proxpi" "single-$package" "$package" "$results_dir" "cold"

            echo ""
        done
    fi
}

# Function to warm up cache
warm_up_cache() {
    local results_dir=$1

    log_info "Warming up caches with all test packages..."

    # Warm up groxpi cache
    run_test "$GROXPI_URL" "groxpi" "warmup" "${TEST_PACKAGES[*]}" "$results_dir" "warmup"

    # Warm up proxpi cache
    run_test "$PROXPI_URL" "proxpi" "warmup" "${TEST_PACKAGES[*]}" "$results_dir" "warmup"

    log_success "Cache warmup completed"
}

# Function to run warm cache installation tests
run_warm_install_tests() {
    local results_dir=$1

    echo ""
    echo "=========================================="
    echo "=== WARM CACHE INSTALLATION TESTS ==="
    echo "=========================================="
    echo ""

    if [ "$TEST_SCENARIO" = "warm-install" ] || [ "$TEST_SCENARIO" = "all" ]; then
        # Warm up caches first
        warm_up_cache "$results_dir"

        log_info "Running warm cache installation tests..."

        # Test each package individually with warm cache
        for package in "${TEST_PACKAGES[@]}"; do
            log_info "Testing warm installation: $package"

            run_test "$GROXPI_URL" "groxpi" "warm-$package" "$package" "$results_dir" "warm"
            run_test "$PROXPI_URL" "proxpi" "warm-$package" "$package" "$results_dir" "warm"

            echo ""
        done
    fi
}

# Function to run batch installation tests
run_batch_install_tests() {
    local results_dir=$1

    echo ""
    echo "=========================================="
    echo "=== BATCH INSTALLATION TESTS ==="
    echo "=========================================="
    echo ""

    if [ "$TEST_SCENARIO" = "batch-install" ] || [ "$TEST_SCENARIO" = "all" ]; then
        log_info "Running batch installation tests..."

        # Test small package batch (fast packages)
        local small_packages="requests click fastapi"
        cold groxpi run_test "$GROXPI_URL" "groxpi" "batch-small" "$small_packages" "$results_dir" "cold"
        cold proxpi run_test "$PROXPI_URL" "proxpi" "batch-small" "$small_packages" "$results_dir" "cold"

        # Test mixed size batch
        local mixed_packages="numpy pandas polars"
        cold groxpi run_test "$GROXPI_URL" "groxpi" "batch-mixed" "$mixed_packages" "$results_dir" "cold"
        cold proxpi run_test "$PROXPI_URL" "proxpi" "batch-mixed" "$mixed_packages" "$results_dir" "cold"

        # Test all packages batch
        cold groxpi run_test "$GROXPI_URL" "groxpi" "batch-all" "${TEST_PACKAGES[*]}" "$results_dir" "cold"
        cold proxpi run_test "$PROXPI_URL" "proxpi" "batch-all" "${TEST_PACKAGES[*]}" "$results_dir" "cold"
    fi
}

# Function to run large package specific tests
run_large_package_tests() {
    local results_dir=$1

    echo ""
    echo "=========================================="
    echo "=== LARGE PACKAGE TESTS ==="
    echo "=========================================="
    echo ""

    if [ "$TEST_SCENARIO" = "large-package" ] || [ "$TEST_SCENARIO" = "all" ]; then
        log_info "Running large package (pyspark) tests..."

        # Test pyspark specifically (it's ~300MB)
        cold groxpi run_test "$GROXPI_URL" "groxpi" "large-pyspark-cold" "pyspark" "$results_dir" "cold"

        cold proxpi run_test "$PROXPI_URL" "proxpi" "large-pyspark-cold" "pyspark" "$results_dir" "cold"

        # Test pyspark with warm cache
        log_info "Testing pyspark with warm cache..."
        run_test "$GROXPI_URL" "groxpi" "large-pyspark-warm" "pyspark" "$results_dir" "warm"
        run_test "$PROXPI_URL" "proxpi" "large-pyspark-warm" "pyspark" "$results_dir" "warm"
    fi
}

# Function to generate summary CSV with proper DuckDB-friendly format
generate_summary_csv() {
    local results_dir=$1
    local summary_file="$results_dir/uv-summary-${TIMESTAMP}.csv"

    log_info "Generating summary CSV: $summary_file"

    # CSV header with proper data types for DuckDB
    echo "run_timestamp,test_datetime,service,test_name,cache_mode,package,install_time_seconds,install_success,installed_size_mb,dependency_count,test_type,test_start,test_end" > "$summary_file"

    # Process all log files
    for log_file in "$results_dir"/uv-*-"$TIMESTAMP".log; do
        if [ -f "$log_file" ]; then
            local filename=$(basename "$log_file")
            # Extract service and test info from filename: uv-{service}-{test_name}-{timestamp}.log
            local service=$(echo "$filename" | cut -d'-' -f2)
            local test_name=$(echo "$filename" | cut -d'-' -f3- | sed "s/-${TIMESTAMP}.log$//")

            # Get test start/end times from log
            local test_start=$(grep "Test start:" "$log_file" | awk '{print $3 " " $4}' | head -1)
            local test_end=$(grep "Completion time:" "$log_file" | awk '{print $3 " " $4}' | head -1)
            test_start="${test_start:-$(date '+%Y-%m-%d %H:%M:%S')}"
            test_end="${test_end:-$(date '+%Y-%m-%d %H:%M:%S')}"

            # Determine cache mode and test type
            local cache_mode="unknown"
            local test_type="individual"
            if [[ $test_name == *"warmup"* ]]; then
                cache_mode="warmup"
                test_type="warmup"
            elif [[ $test_name == *"cold"* ]] || [[ $test_name == *"single"* ]]; then
                cache_mode="cold"
            elif [[ $test_name == *"warm"* ]]; then
                cache_mode="warm"
            elif [[ $test_name == *"batch"* ]]; then
                test_type="batch"
                cache_mode="cold"  # Most batch tests are cold
            fi

            # Extract individual package installations
            while IFS= read -r line; do
                if [[ $line =~ ✓\ ([a-zA-Z0-9_-]+)\ installed\ successfully\ in\ ([0-9.]+)s ]]; then
                    local package="${BASH_REMATCH[1]}"
                    local install_time="${BASH_REMATCH[2]}"

                    # Try to find package size and dependency info with better parsing
                    local size_raw=$(grep -A2 "✓ $package installed successfully" "$log_file" | grep "Installed size:" | awk '{print $3}' | head -1)
                    local size_mb=""
                    if [ -n "$size_raw" ] && [ "$size_raw" != "unknown" ]; then
                        # Convert size to MB
                        if [[ $size_raw == *"GB"* ]] || [[ $size_raw == *"G"* ]]; then
                            size_mb=$(echo "$size_raw" | sed 's/[^0-9.]//g' | awk '{print $1 * 1024}')
                        elif [[ $size_raw == *"MB"* ]] || [[ $size_raw == *"M"* ]]; then
                            size_mb=$(echo "$size_raw" | sed 's/[^0-9.]//g')
                        elif [[ $size_raw == *"KB"* ]] || [[ $size_raw == *"K"* ]]; then
                            size_mb=$(echo "$size_raw" | sed 's/[^0-9.]//g' | awk '{print $1 / 1024}')
                        fi
                    fi

                    local deps=$(grep -A3 "✓ $package installed successfully" "$log_file" | grep "Dependencies installed:" | awk '{print $3}' | head -1)
                    if [ "$deps" = "unknown" ]; then deps=""; fi

                    echo "$TIMESTAMP,$test_start,$service,$test_name,$cache_mode,$package,$install_time,TRUE,$size_mb,$deps,$test_type,$test_start,$test_end" >> "$summary_file"
                fi
            done < "$log_file"

            # Extract batch installations
            while IFS= read -r line; do
                if [[ $line =~ ✓\ Batch\ installation\ completed\ in\ ([0-9.]+)s ]]; then
                    local install_time="${BASH_REMATCH[1]}"

                    # Parse batch installation size
                    local size_raw=$(grep -A2 "Batch installation completed" "$log_file" | grep "Total installed size:" | awk '{print $4}' | head -1)
                    local size_mb=""
                    if [ -n "$size_raw" ] && [ "$size_raw" != "unknown" ]; then
                        if [[ $size_raw == *"GB"* ]] || [[ $size_raw == *"G"* ]]; then
                            size_mb=$(echo "$size_raw" | sed 's/[^0-9.]//g' | awk '{print $1 * 1024}')
                        elif [[ $size_raw == *"MB"* ]] || [[ $size_raw == *"M"* ]]; then
                            size_mb=$(echo "$size_raw" | sed 's/[^0-9.]//g')
                        elif [[ $size_raw == *"KB"* ]] || [[ $size_raw == *"K"* ]]; then
                            size_mb=$(echo "$size_raw" | sed 's/[^0-9.]//g' | awk '{print $1 / 1024}')
                        fi
                    fi

                    local deps=$(grep -A3 "Batch installation completed" "$log_file" | grep "Total dependencies:" | awk '{print $3}' | head -1)
                    if [ "$deps" = "unknown" ]; then deps=""; fi

                    # For batch installations, use a combined package name
                    local batch_packages="batch_all"
                    if [[ $test_name == *"small"* ]]; then
                        batch_packages="batch_small"
                    elif [[ $test_name == *"mixed"* ]]; then
                        batch_packages="batch_mixed"
                    fi

                    echo "$TIMESTAMP,$test_start,$service,$test_name,$cache_mode,$batch_packages,$install_time,TRUE,$size_mb,$deps,batch,$test_start,$test_end" >> "$summary_file"
                fi
            done < "$log_file"

            # Extract failed installations
            while IFS= read -r line; do
                if [[ $line =~ ✗\ ([a-zA-Z0-9_-]+)\ installation\ failed\ after\ ([0-9.]+)s ]]; then
                    local package="${BASH_REMATCH[1]}"
                    local install_time="${BASH_REMATCH[2]}"

                    echo "$TIMESTAMP,$test_start,$service,$test_name,$cache_mode,$package,$install_time,FALSE,,,$test_type,$test_start,$test_end" >> "$summary_file"
                fi
            done < "$log_file"
        fi
    done

    log_success "Summary CSV generated with DuckDB-friendly format: $summary_file"
}

# Main function
main() {
    local start_time=$(date '+%Y-%m-%d %H:%M:%S')

    echo "=== UV Package Installation Testing ==="
    echo "Start time: $start_time"
    echo "Timestamp: $TIMESTAMP"
    echo "Test scenario: $TEST_SCENARIO"
    echo "groxpi URL: $GROXPI_URL"
    echo "proxpi URL: $PROXPI_URL"
    echo "Test packages: ${TEST_PACKAGES[*]}"
    echo "Docker network: $DOCKER_NETWORK"
    echo "UV image: $UV_IMAGE"
    echo ""

    # Validate inputs
    validate_inputs

    # Check dependencies
    check_dependencies

    # Test server connections
    if ! test_server_connection "$GROXPI_URL" "groxpi"; then
        exit 1
    fi

    if ! test_server_connection "$PROXPI_URL" "proxpi"; then
        exit 1
    fi

    # Create results directory
    local results_dir="${RESULTS_DIR:-$(dirname "$0")/../results}"
    mkdir -p "$results_dir"

    # Run tests based on scenario
    case "$TEST_SCENARIO" in
        cold-install)
            run_cold_install_tests "$results_dir"
            ;;
        warm-install)
            run_warm_install_tests "$results_dir"
            ;;
        batch-install)
            run_batch_install_tests "$results_dir"
            ;;
        large-package)
            run_large_package_tests "$results_dir"
            ;;
        all)
            run_cold_install_tests "$results_dir"
            run_warm_install_tests "$results_dir"
            run_batch_install_tests "$results_dir"
            run_large_package_tests "$results_dir"
            ;;
        *)
            log_error "Unknown test scenario: $TEST_SCENARIO"
            show_usage
            exit 1
            ;;
    esac

    # Generate summary
    generate_summary_csv "$results_dir"

    local end_time=$(date '+%Y-%m-%d %H:%M:%S')
    echo ""
    echo "=== UV Installation Testing Complete ==="
    echo "Start time: $start_time"
    echo "End time: $end_time"
    echo "Results saved to: $results_dir"
    echo "Summary: $results_dir/uv-summary-${TIMESTAMP}.csv"

    if [ "$FAILURES" -gt 0 ]; then
        log_error "$FAILURES test run(s) failed or were skipped; see the log and the FALSE rows in the summary"
        exit 1
    fi

    log_success "UV installation testing completed successfully"
}

# Run main function
main "$@"