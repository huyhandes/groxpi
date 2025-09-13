#!/bin/bash

# groxpi Master Benchmark Orchestrator
# Coordinates comprehensive performance testing between groxpi and proxpi
# Usage: ./benchmark.sh --groxpi-url <url> --proxpi-url <url> [options]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Configuration with defaults
GROXPI_URL=""
PROXPI_URL=""
TIMESTAMP=""
API_ONLY=false
UV_ONLY=false
RESOURCE_MONITORING=true
DOCKER_NETWORK="benchmark-network"
RESULTS_DIR="$SCRIPT_DIR/results"

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
    echo "groxpi Master Benchmark Orchestrator"
    echo ""
    echo "Usage: $0 --groxpi-url <url> --proxpi-url <url> [options]"
    echo ""
    echo "Required Parameters:"
    echo "  --groxpi-url URL   groxpi server URL (e.g., http://server1:5005)"
    echo "  --proxpi-url URL   proxpi server URL (e.g., http://server2:5006)"
    echo ""
    echo "Options:"
    echo "  --timestamp TS     Use specific timestamp (default: auto-generated)"
    echo "  --api-only         Run only WRK API benchmarks"
    echo "  --uv-only          Run only UV installation benchmarks"
    echo "  --no-monitoring    Disable resource monitoring"
    echo "  --docker-network N Docker network name (default: groxpi_benchmark-network)"
    echo "  --results-dir DIR  Results directory (default: ./results)"
    echo "  -h, --help         Show this help message"
    echo ""
    echo "Environment Variables:"
    echo "  GROXPI_URL         Alternative way to specify groxpi URL"
    echo "  PROXPI_URL         Alternative way to specify proxpi URL"
    echo "  DOCKER_NETWORK     Docker network name"
    echo ""
    echo "Examples:"
    echo "  $0 --groxpi-url http://server1:5005 --proxpi-url http://server2:5006"
    echo "  $0 --groxpi-url http://localhost:5005 --proxpi-url http://localhost:5006 --api-only"
    echo ""
    echo "Export GROXPI_URL and PROXPI_URL environment variables for convenience:"
    echo "  export GROXPI_URL=http://server1:5005"
    echo "  export PROXPI_URL=http://server2:5006"
    echo "  $0"
}

# Function to parse command line arguments
parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --groxpi-url)
                GROXPI_URL="$2"
                shift 2
                ;;
            --proxpi-url)
                PROXPI_URL="$2"
                shift 2
                ;;
            --timestamp)
                TIMESTAMP="$2"
                shift 2
                ;;
            --api-only)
                API_ONLY=true
                shift
                ;;
            --uv-only)
                UV_ONLY=true
                shift
                ;;
            --no-monitoring)
                RESOURCE_MONITORING=false
                shift
                ;;
            --docker-network)
                DOCKER_NETWORK="$2"
                shift 2
                ;;
            --results-dir)
                RESULTS_DIR="$2"
                shift 2
                ;;
            -h|--help)
                show_usage
                exit 0
                ;;
            *)
                log_error "Unknown parameter: $1"
                show_usage
                exit 1
                ;;
        esac
    done

    # Use environment variables if not provided via command line
    GROXPI_URL="${GROXPI_URL:-${GROXPI_URL:-}}"
    PROXPI_URL="${PROXPI_URL:-${PROXPI_URL:-}}"

    # Generate timestamp if not provided
    if [ -z "$TIMESTAMP" ]; then
        TIMESTAMP=$(date +%Y%m%d_%H%M%S)
    fi

    # Validate required parameters
    if [ -z "$GROXPI_URL" ] || [ -z "$PROXPI_URL" ]; then
        log_error "Both groxpi and proxpi URLs are required"
        show_usage
        exit 1
    fi

    # Remove trailing slashes
    GROXPI_URL="${GROXPI_URL%/}"
    PROXPI_URL="${PROXPI_URL%/}"

    # Validate timestamp format if provided
    if [[ ! $TIMESTAMP =~ ^[0-9]{8}_[0-9]{6}$ ]]; then
        log_error "Invalid timestamp format. Expected: YYYYMMDD_HHMMSS (e.g., 20240101_120000)"
        exit 1
    fi

    # Create results directory
    mkdir -p "$RESULTS_DIR"
}

# Function to validate environment and dependencies
validate_environment() {
    log_info "Validating environment..."

    # Check required scripts
    local required_scripts=("cache_manager.sh" "monitor_resources.sh" "wrk_api_test.sh" "uv_install_test.sh")
    for script in "${required_scripts[@]}"; do
        local script_path="$SCRIPT_DIR/scripts/$script"
        if [ ! -x "$script_path" ]; then
            log_error "Required script not found or not executable: $script_path"
            exit 1
        fi
    done

    # Test server connectivity
    if ! "$SCRIPT_DIR/scripts/cache_manager.sh" "$GROXPI_URL" "$PROXPI_URL" test-connection; then
        log_error "Server connectivity test failed"
        exit 1
    fi

    log_success "Environment validation passed"
}

# Function to start resource monitoring
start_resource_monitoring() {
    if [ "$RESOURCE_MONITORING" = false ]; then
        log_info "Resource monitoring disabled"
        return 0
    fi

    log_info "Starting resource monitoring..."

    local monitor_script="$SCRIPT_DIR/scripts/monitor_resources.sh"
    local resource_file="$RESULTS_DIR/resources-${TIMESTAMP}.csv"

    # Extract container names from URLs for docker container monitoring
    local groxpi_container="groxpi-bench"
    local proxpi_container="proxpi-bench"

    # Start monitoring in background
    "$monitor_script" start "$groxpi_container" "$proxpi_container" "$resource_file" 2 &
    local monitor_pid=$!

    # Wait a moment to ensure monitoring started
    sleep 3

    if kill -0 "$monitor_pid" 2>/dev/null; then
        log_success "Resource monitoring started (PID: $monitor_pid)"
        echo "$monitor_pid" > "$RESULTS_DIR/monitor_pid_${TIMESTAMP}"
        return 0
    else
        log_warning "Resource monitoring failed to start, continuing without it"
        return 1
    fi
}

# Function to stop resource monitoring
stop_resource_monitoring() {
    if [ "$RESOURCE_MONITORING" = false ]; then
        return 0
    fi

    local pid_file="$RESULTS_DIR/monitor_pid_${TIMESTAMP}"
    if [ -f "$pid_file" ]; then
        local monitor_pid=$(cat "$pid_file")
        log_info "Stopping resource monitoring (PID: $monitor_pid)..."

        if kill "$monitor_pid" 2>/dev/null; then
            # Wait for monitoring to stop gracefully
            sleep 2
            log_success "Resource monitoring stopped"
        else
            log_warning "Failed to stop resource monitoring process"
        fi

        rm -f "$pid_file"
    else
        log_info "No resource monitoring to stop"
    fi
}

# Function to run WRK API tests
run_api_tests() {
    log_info "Running WRK API performance tests..."

    local wrk_script="$SCRIPT_DIR/scripts/wrk_api_test.sh"

    if "$wrk_script" "$GROXPI_URL" "$PROXPI_URL" "$TIMESTAMP" all; then
        log_success "WRK API tests completed successfully"
        return 0
    else
        log_error "WRK API tests failed"
        return 1
    fi
}

# Function to run UV installation tests
run_uv_tests() {
    log_info "Running UV package installation tests..."

    local uv_script="$SCRIPT_DIR/scripts/uv_install_test.sh"

    # Set Docker network environment variable
    export DOCKER_NETWORK="$DOCKER_NETWORK"

    if "$uv_script" "$GROXPI_URL" "$PROXPI_URL" "$TIMESTAMP" all; then
        log_success "UV installation tests completed successfully"
        return 0
    else
        log_error "UV installation tests failed"
        return 1
    fi
}

# Function to generate consolidated report
generate_consolidated_report() {
    log_info "Generating consolidated benchmark report..."

    local report_file="$RESULTS_DIR/benchmark-report-${TIMESTAMP}.md"

    cat > "$report_file" << EOF
# groxpi vs proxpi Benchmark Report

**Timestamp:** $TIMESTAMP
**Generated:** $(date '+%Y-%m-%d %H:%M:%S')

## Test Configuration

- **groxpi URL:** $GROXPI_URL
- **proxpi URL:** $PROXPI_URL
- **Docker Network:** $DOCKER_NETWORK
- **Resource Monitoring:** $([ "$RESOURCE_MONITORING" = true ] && echo "Enabled" || echo "Disabled")

## Test Scenarios Executed

EOF

    if [ "$API_ONLY" = true ]; then
        echo "- ✅ WRK API Performance Tests" >> "$report_file"
        echo "- ❌ UV Package Installation Tests (skipped)" >> "$report_file"
    elif [ "$UV_ONLY" = true ]; then
        echo "- ❌ WRK API Performance Tests (skipped)" >> "$report_file"
        echo "- ✅ UV Package Installation Tests" >> "$report_file"
    else
        echo "- ✅ WRK API Performance Tests" >> "$report_file"
        echo "- ✅ UV Package Installation Tests" >> "$report_file"
    fi

    cat >> "$report_file" << EOF

## Results Summary

### API Performance (WRK)
EOF

    if [ -f "$RESULTS_DIR/wrk-summary-${TIMESTAMP}.csv" ]; then
        echo "Detailed results available in: \`wrk-summary-${TIMESTAMP}.csv\`" >> "$report_file"
        echo "" >> "$report_file"

        # Extract some key metrics for the report
        echo "#### Key Metrics" >> "$report_file"
        echo "| Service | Test | Requests/sec | Avg Latency |" >> "$report_file"
        echo "|---------|------|-------------|-------------|" >> "$report_file"

        # Process the CSV to extract key metrics
        if command -v awk >/dev/null 2>&1; then
            tail -n +2 "$RESULTS_DIR/wrk-summary-${TIMESTAMP}.csv" | \
            awk -F',' '$3=="load" && $4=="index-warm" {printf "| %s | %s | %s | %s |\n", $2, $4, $6, $7}' >> "$report_file" || true
        fi
    else
        echo "No WRK results found." >> "$report_file"
    fi

    cat >> "$report_file" << EOF

### Package Installation (UV)
EOF

    if [ -f "$RESULTS_DIR/uv-summary-${TIMESTAMP}.csv" ]; then
        echo "Detailed results available in: \`uv-summary-${TIMESTAMP}.csv\`" >> "$report_file"
        echo "" >> "$report_file"

        echo "#### Installation Times" >> "$report_file"
        echo "| Service | Package | Cold Cache (s) | Warm Cache (s) |" >> "$report_file"
        echo "|---------|---------|---------------|---------------|" >> "$report_file"

        # Extract installation times for key packages
        if command -v awk >/dev/null 2>&1; then
            local packages=("numpy" "pandas" "polars" "fastapi")
            for pkg in "${packages[@]}"; do
                local groxpi_cold=$(awk -F',' "\$2==\"groxpi\" && \$3~\"cold\" && \$5==\"$pkg\" {print \$6}" "$RESULTS_DIR/uv-summary-${TIMESTAMP}.csv" | head -1)
                local groxpi_warm=$(awk -F',' "\$2==\"groxpi\" && \$3~\"warm\" && \$5==\"$pkg\" {print \$6}" "$RESULTS_DIR/uv-summary-${TIMESTAMP}.csv" | head -1)
                local proxpi_cold=$(awk -F',' "\$2==\"proxpi\" && \$3~\"cold\" && \$5==\"$pkg\" {print \$6}" "$RESULTS_DIR/uv-summary-${TIMESTAMP}.csv" | head -1)
                local proxpi_warm=$(awk -F',' "\$2==\"proxpi\" && \$3~\"warm\" && \$5==\"$pkg\" {print \$6}" "$RESULTS_DIR/uv-summary-${TIMESTAMP}.csv" | head -1)

                if [ -n "$groxpi_cold" ] || [ -n "$proxpi_cold" ]; then
                    echo "| groxpi | $pkg | ${groxpi_cold:-N/A} | ${groxpi_warm:-N/A} |" >> "$report_file"
                    echo "| proxpi | $pkg | ${proxpi_cold:-N/A} | ${proxpi_warm:-N/A} |" >> "$report_file"
                fi
            done
        fi
    else
        echo "No UV results found." >> "$report_file"
    fi

    cat >> "$report_file" << EOF

## Resource Usage

EOF

    if [ -f "$RESULTS_DIR/resources-${TIMESTAMP}.csv" ]; then
        echo "Resource monitoring data available in: \`resources-${TIMESTAMP}.csv\`" >> "$report_file"
        echo "" >> "$report_file"
        echo "Use the DuckDB analysis script for detailed analysis:" >> "$report_file"
        echo "\`\`\`bash" >> "$report_file"
        echo "./scripts/analyze_results_duckdb.sh $TIMESTAMP" >> "$report_file"
        echo "\`\`\`" >> "$report_file"
    else
        echo "No resource monitoring data available." >> "$report_file"
    fi

    cat >> "$report_file" << EOF

## Files Generated

### Raw Results
EOF

    # List all generated files
    local files=($(ls "$RESULTS_DIR"/*-"$TIMESTAMP".* 2>/dev/null || true))
    for file in "${files[@]}"; do
        local basename_file=$(basename "$file")
        echo "- \`$basename_file\`" >> "$report_file"
    done

    cat >> "$report_file" << EOF

### Analysis
- Run \`./scripts/analyze_results_duckdb.sh $TIMESTAMP\` for detailed DuckDB-powered analysis
- Summary CSVs can be imported into Excel, Google Sheets, or analyzed with DuckDB/pandas

## Notes

This benchmark compared groxpi (Go implementation) vs proxpi (Python implementation)
across API performance and package installation scenarios with both cold and warm cache conditions.

**Test Packages:** numpy, pandas, polars, pyspark, fastapi
**Generated by:** groxpi benchmark suite v1.0
EOF

    log_success "Consolidated report generated: $report_file"
}

# Function to cleanup on exit
cleanup() {
    log_info "Cleaning up..."
    stop_resource_monitoring
}

# Set up signal handlers
trap cleanup SIGINT SIGTERM EXIT

# Main function
main() {
    local start_time=$(date '+%Y-%m-%d %H:%M:%S')

    echo "======================================================"
    echo "=== groxpi Master Benchmark Orchestrator ==="
    echo "======================================================"
    echo ""
    echo "Start time: $start_time"
    echo ""

    # Parse arguments
    parse_arguments "$@"

    log_info "Configuration:"
    log_info "  groxpi URL: $GROXPI_URL"
    log_info "  proxpi URL: $PROXPI_URL"
    log_info "  Timestamp: $TIMESTAMP"
    log_info "  API Only: $API_ONLY"
    log_info "  UV Only: $UV_ONLY"
    log_info "  Resource Monitoring: $RESOURCE_MONITORING"
    log_info "  Results Directory: $RESULTS_DIR"
    echo ""

    # Validate environment
    validate_environment

    # Start resource monitoring
    start_resource_monitoring

    local tests_failed=false

    # Run tests based on configuration
    if [ "$UV_ONLY" = true ]; then
        if ! run_uv_tests; then
            tests_failed=true
        fi
    elif [ "$API_ONLY" = true ]; then
        if ! run_api_tests; then
            tests_failed=true
        fi
    else
        # Run all tests
        if ! run_api_tests; then
            log_warning "API tests failed, but continuing with UV tests..."
            tests_failed=true
        fi

        if ! run_uv_tests; then
            log_warning "UV tests failed"
            tests_failed=true
        fi
    fi

    # Stop resource monitoring
    stop_resource_monitoring

    # Generate consolidated report
    generate_consolidated_report

    local end_time=$(date '+%Y-%m-%d %H:%M:%S')

    echo ""
    echo "======================================================"
    echo "=== Benchmark Suite Complete ==="
    echo "======================================================"
    echo "Start time: $start_time"
    echo "End time: $end_time"
    echo "Timestamp: $TIMESTAMP"
    echo "Results directory: $RESULTS_DIR"
    echo ""

    if $tests_failed; then
        log_warning "Some tests failed, but results were generated"
        log_info "Check individual test logs for details"
        exit 1
    else
        log_success "All benchmark tests completed successfully"
    fi

    log_info "Next steps:"
    log_info "  1. Review the consolidated report: $RESULTS_DIR/benchmark-report-${TIMESTAMP}.md"
    log_info "  2. Run detailed analysis: ./scripts/analyze_results_duckdb.sh $TIMESTAMP"
    log_info "  3. Copy CSV files to your analysis environment if needed"
}

# Run main function
main "$@"