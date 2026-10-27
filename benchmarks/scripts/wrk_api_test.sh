#!/bin/bash

# WRK API Test Script
# Tests HTTP API performance using wrk load testing tool
# Usage: ./wrk_api_test.sh <groxpi_url> <proxpi_url> <timestamp> [test_scenario]

set -euo pipefail

# Configuration
GROXPI_URL="${1:-}"
PROXPI_URL="${2:-}"
TIMESTAMP="${3:-}"
TEST_SCENARIO="${4:-all}"

# WRK Configuration
WRK_DURATION="60s"
WRK_THREADS=8
WRK_CONNECTIONS=100
WRK_TIMEOUT="30s"

# Test packages - vary in size and complexity
TEST_PACKAGES=("numpy" "pandas" "polars" "pyspark" "fastapi")

# Failed measurements, counted instead of aborting so the summary CSV is still written
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
    echo "  cold-single   - Cold cache single request timing"
    echo "  warm-load     - Warm cache load testing"
    echo "  package-tests - Individual package testing"
    echo ""
    echo "Test Packages: ${TEST_PACKAGES[*]}"
    echo ""
    echo "Examples:"
    echo "  $0 http://server1:5005 http://server2:5006 20240101_120000"
    echo "  $0 http://server1:5005 http://server2:5006 20240101_120000 warm-load"
}

# Function to check if wrk is installed
check_wrk() {
    if ! command -v wrk >/dev/null 2>&1; then
        log_error "wrk is not installed. Please install wrk first:"
        echo "  macOS: brew install wrk"
        echo "  Ubuntu/Debian: sudo apt-get install wrk"
        echo "  From source: https://github.com/wg/wrk"
        exit 1
    fi

    log_info "Using wrk: $(which wrk)"
    wrk --version 2>/dev/null || log_info "wrk installed"
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

# Empty one proxy's cache (groxpi or proxpi) using cache_manager.sh
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
        # Wait a moment for cache clear to take effect
        sleep 3
        return 0
    else
        log_error "Failed to clear caches"
        return 1
    fi
}

# Function to run single request timing test
run_single_request_test() {
    local url=$1
    local endpoint=$2
    local service=$3
    local test_name=$4
    local results_dir=$5

    local output_file="$results_dir/wrk-${service}-single-${test_name}-${TIMESTAMP}.log"

    log_info "Single request test: $service - $test_name ($endpoint)"

    # Make single request; curl times it, so no clock arithmetic here
    local response
    local http_code
    if response=$(curl -s -o /dev/null -w "HTTP_CODE:%{http_code}\nTIME_TOTAL:%{time_total}" --max-time 30 "$url$endpoint" 2>&1); then
        # Extract metrics
        http_code=$(echo "$response" | grep -o 'HTTP_CODE:[0-9]*' | cut -d':' -f2)
        time_total=$(echo "$response" | grep -o 'TIME_TOTAL:[0-9.]*' | cut -d':' -f2)

        # Write results
        {
            echo "=== WRK Single Request Test ==="
            echo "Service: $service"
            echo "Test: $test_name"
            echo "URL: $url$endpoint"
            echo "Timestamp: $(date '+%Y-%m-%d %H:%M:%S')"
            echo ""
            echo "Results:"
            echo "  HTTP Code: $http_code"
            echo "  Response Time: ${time_total}s"
            echo ""
        } > "$output_file"

        if [ "$http_code" = "200" ]; then
            log_success "  Response time: ${time_total}s (HTTP $http_code)"
        else
            log_warning "  Response time: ${time_total}s (HTTP $http_code)"
        fi
    else
        log_error "  Request failed"
        echo "ERROR: Request failed" > "$output_file"
        return 1
    fi
}

# Function to run wrk load test
run_wrk_load_test() {
    local url=$1
    local endpoint=$2
    local service=$3
    local test_name=$4
    local results_dir=$5
    local duration=${6:-$WRK_DURATION}
    local threads=${7:-$WRK_THREADS}
    local connections=${8:-$WRK_CONNECTIONS}

    local output_file="$results_dir/wrk-${service}-load-${test_name}-${TIMESTAMP}.log"

    log_info "Load test: $service - $test_name ($endpoint)"
    log_info "  Duration: $duration, Threads: $threads, Connections: $connections"

    # Run wrk load test
    if wrk -t$threads -c$connections -d$duration \
           --timeout $WRK_TIMEOUT \
           --latency \
           "$url$endpoint" > "$output_file" 2>&1; then

        # wrk counts every response as a request, so error replies would pass
        # for throughput. Any non-2xx/3xx reply fails the measurement, and the
        # log is renamed out of the summary CSV's glob so its rate is not reported.
        local non_2xx
        non_2xx=$(awk '/Non-2xx or 3xx responses:/ {print $NF; exit}' "$output_file")
        if [ -n "$non_2xx" ]; then
            local failed_file="${output_file%.log}.failed.log"
            mv "$output_file" "$failed_file"
            log_error "  Load test got $non_2xx non-2xx/3xx responses; see $failed_file"
            return 1
        fi

        # Extract key metrics
        local rps=$(grep "Requests/sec:" "$output_file" | awk '{print $2}' | head -1)
        local latency_avg=$(grep "Latency" "$output_file" | awk '{print $2}' | head -1)
        local latency_p50=$(grep "50%" "$output_file" | awk '{print $2}' | head -1)
        local latency_p99=$(grep "99%" "$output_file" | awk '{print $2}' | head -1)

        log_success "  RPS: ${rps:-N/A}, Avg Latency: ${latency_avg:-N/A}"
        log_info "  P50: ${latency_p50:-N/A}, P99: ${latency_p99:-N/A}"
    else
        log_error "  Load test failed"
        return 1
    fi
}

# Run one measurement; a failure is counted instead of aborting the suite
single() { run_single_request_test "$@" || FAILURES=$((FAILURES + 1)); }
load() { run_wrk_load_test "$@" || FAILURES=$((FAILURES + 1)); }

# Empty one proxy's cache (groxpi or proxpi), then run one measurement against it:
#   cold <target> <measurement> <args...>
# Only the measured proxy is recreated. A failed clear is counted and the
# measurement skipped, so a warm number is never recorded as a cold one.
cold() {
    local target=$1
    shift
    if clear_cache "$target"; then
        "$@"
    else
        FAILURES=$((FAILURES + 1))
    fi
}

# Function to run cold cache tests
run_cold_cache_tests() {
    local results_dir=$1

    echo ""
    echo "=========================================="
    echo "=== COLD CACHE TESTS ==="
    echo "=========================================="
    echo ""

    if [ "$TEST_SCENARIO" = "cold-single" ] || [ "$TEST_SCENARIO" = "all" ]; then
        log_info "Running cold cache single request tests..."

        # Test package index
        cold groxpi single "$GROXPI_URL" "/simple/" "groxpi" "index-cold" "$results_dir"
        cold proxpi single "$PROXPI_URL" "/simple/" "proxpi" "index-cold" "$results_dir"

        # Test specific package (numpy)
        cold groxpi single "$GROXPI_URL" "/simple/numpy/" "groxpi" "numpy-cold" "$results_dir"
        cold proxpi single "$PROXPI_URL" "/simple/numpy/" "proxpi" "numpy-cold" "$results_dir"
    fi

    # No cold load run: only its first request would miss, so it would measure a
    # warm cache under a cold name. cold-single covers the cold request.
}

# Function to warm up cache
warm_up_cache() {
    local url=$1
    local service=$2

    log_info "Warming up cache for $service..."

    for package in "${TEST_PACKAGES[@]}"; do
        curl -s "$url/simple/$package/" >/dev/null 2>&1 || true
        sleep 0.5  # Small delay between requests
    done

    log_success "$service cache warmed up"
}

# Function to run warm cache tests
run_warm_cache_tests() {
    local results_dir=$1

    echo ""
    echo "=========================================="
    echo "=== WARM CACHE TESTS ==="
    echo "=========================================="
    echo ""

    if [ "$TEST_SCENARIO" = "warm-load" ] || [ "$TEST_SCENARIO" = "all" ]; then
        # Warm up both caches
        warm_up_cache "$GROXPI_URL" "groxpi"
        warm_up_cache "$PROXPI_URL" "proxpi"

        log_info "Running warm cache load tests..."

        # Test specific package with warm cache. No /simple/ load run: groxpi
        # passes the root index through uncached, so it would measure pypi.org.
        load "$GROXPI_URL" "/simple/numpy/" "groxpi" "numpy-warm" "$results_dir"
        load "$PROXPI_URL" "/simple/numpy/" "proxpi" "numpy-warm" "$results_dir"
    fi
}

# Function to run individual package tests
run_package_tests() {
    local results_dir=$1

    if [ "$TEST_SCENARIO" = "package-tests" ] || [ "$TEST_SCENARIO" = "all" ]; then
        echo ""
        echo "=========================================="
        echo "=== INDIVIDUAL PACKAGE TESTS ==="
        echo "=========================================="
        echo ""

        # Test each package individually
        for package in "${TEST_PACKAGES[@]}"; do
            log_info "Testing package: $package"

            cold groxpi single "$GROXPI_URL" "/simple/$package/" "groxpi" "$package-individual" "$results_dir"
            cold proxpi single "$PROXPI_URL" "/simple/$package/" "proxpi" "$package-individual" "$results_dir"

            echo ""
        done
    fi
}

# Parse one `wrk --latency` log into CSV fields:
#   requests_per_sec,avg_latency_ms,p50_latency_ms,p99_latency_ms,duration_seconds,threads,connections
# wrk prints latencies as us/ms/s and the duration as s/m/h; all are normalised.
# A field wrk did not print is left empty, which DuckDB reads as NULL.
parse_wrk_log() {
    awk '
        function ms(v) {
            if (v ~ /us$/) return sprintf("%.3f", substr(v, 1, length(v) - 2) / 1000)
            if (v ~ /ms$/) return sprintf("%.3f", substr(v, 1, length(v) - 2))
            if (v ~ /s$/)  return sprintf("%.3f", substr(v, 1, length(v) - 1) * 1000)
            return ""
        }
        function secs(v) {
            if (v ~ /s$/) return substr(v, 1, length(v) - 1) + 0
            if (v ~ /m$/) return substr(v, 1, length(v) - 1) * 60
            if (v ~ /h$/) return substr(v, 1, length(v) - 1) * 3600
            return ""
        }
        $1 == "Running" && $3 == "test" { duration = secs($2) }
        $2 == "threads" && $3 == "and" && $5 == "connections" { threads = $1; connections = $4 }
        $1 == "Latency" && avg == "" { avg = ms($2) }
        $1 == "50%" { p50 = ms($2) }
        $1 == "99%" { p99 = ms($2) }
        $1 == "Requests/sec:" { rps = $2 }
        END { printf "%s,%s,%s,%s,%s,%s,%s\n", rps, avg, p50, p99, duration, threads, connections }
    ' "$1"
}

# Function to generate summary CSV with proper DuckDB-friendly format
generate_summary_csv() {
    local results_dir=$1
    local summary_file="$results_dir/wrk-summary-${TIMESTAMP}.csv"

    log_info "Generating summary CSV: $summary_file"

    # CSV header with proper data types for DuckDB
    echo "run_timestamp,test_datetime,service,test_type,test_name,endpoint,requests_per_sec,avg_latency_ms,p50_latency_ms,p99_latency_ms,response_time_single_sec,http_code,duration_seconds,threads,connections" > "$summary_file"

    # Process all log files
    for log_file in "$results_dir"/wrk-*-"$TIMESTAMP".log; do
        if [ -f "$log_file" ]; then
            local filename=$(basename "$log_file")
            # Extract service and test info from filename: wrk-{service}-{type}-{name}-{timestamp}.log
            local service=$(echo "$filename" | cut -d'-' -f2)
            local test_type=$(echo "$filename" | cut -d'-' -f3)
            local test_name=$(echo "$filename" | cut -d'-' -f4- | sed "s/-${TIMESTAMP}.log$//")

            # Get test datetime from log file
            local test_datetime=$(grep "Timestamp:" "$log_file" | awk '{print $2 " " $3}' | head -1)
            test_datetime="${test_datetime:-$(date '+%Y-%m-%d %H:%M:%S')}"

            if [ "$test_type" = "load" ]; then
                local rps avg_latency p50_latency p99_latency duration threads connections
                IFS=, read -r rps avg_latency p50_latency p99_latency duration threads connections \
                    < <(parse_wrk_log "$log_file")

                # Get endpoint from log or derive from test name
                local endpoint
                if [[ $test_name == *"index"* ]]; then
                    endpoint="simple/"
                elif [[ $test_name == *"numpy"* ]]; then
                    endpoint="simple/numpy/"
                else
                    endpoint=$(awk '/^Running /{print $NF; exit}' "$log_file" | sed -E 's|^[a-z]+://[^/]*/||')
                fi

                echo "$TIMESTAMP,$test_datetime,$service,$test_type,$test_name,$endpoint,$rps,$avg_latency,$p50_latency,$p99_latency,,,$duration,$threads,$connections" >> "$summary_file"

            elif [ "$test_type" = "single" ]; then
                # Extract single request metrics
                local response_time=$(grep "Response Time:" "$log_file" | awk '{print $3}' | sed 's/s$//')
                local http_code=$(grep "HTTP Code:" "$log_file" | awk '{print $3}')

                # Get endpoint from test name or log
                local endpoint
                if [[ $test_name == *"index"* ]]; then
                    endpoint="simple/"
                elif [[ $test_name == *"numpy"* ]]; then
                    endpoint="simple/numpy/"
                else
                    local pkg=$(echo "$test_name" | sed 's/-cold$//' | sed 's/-warm$//' | sed 's/-individual$//')
                    endpoint="simple/${pkg}/"
                fi

                echo "$TIMESTAMP,$test_datetime,$service,$test_type,$test_name,$endpoint,,,,,$response_time,$http_code,,," >> "$summary_file"
            fi
        fi
    done

    log_success "Summary CSV generated with DuckDB-friendly format: $summary_file"
}

# Main function
main() {
    local start_time=$(date '+%Y-%m-%d %H:%M:%S')

    echo "=== WRK API Performance Testing ==="
    echo "Start time: $start_time"
    echo "Timestamp: $TIMESTAMP"
    echo "Test scenario: $TEST_SCENARIO"
    echo "groxpi URL: $GROXPI_URL"
    echo "proxpi URL: $PROXPI_URL"
    echo "Test packages: ${TEST_PACKAGES[*]}"
    echo ""

    # Validate inputs
    validate_inputs

    # Check dependencies
    check_wrk

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
        cold-single)
            run_cold_cache_tests "$results_dir"
            ;;
        warm-load)
            run_warm_cache_tests "$results_dir"
            ;;
        package-tests)
            run_package_tests "$results_dir"
            ;;
        all)
            run_cold_cache_tests "$results_dir"
            run_warm_cache_tests "$results_dir"
            run_package_tests "$results_dir"
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
    echo "=== WRK API Testing Complete ==="
    echo "Start time: $start_time"
    echo "End time: $end_time"
    echo "Results saved to: $results_dir"
    echo "Summary: $results_dir/wrk-summary-${TIMESTAMP}.csv"

    if [ "$FAILURES" -gt 0 ]; then
        log_error "$FAILURES measurement(s) or cache clear(s) failed; see the logs above"
        exit 1
    fi

    log_success "WRK API testing completed successfully"
}

# Run main function (skipped when sourced, so parse_wrk_log can be checked alone)
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi