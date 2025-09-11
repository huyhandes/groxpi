#!/bin/bash

# Main Benchmark Orchestrator Script
# Runs benchmarks for groxpi performance analysis

set -euo pipefail

# Configuration
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
RESULTS_DIR="$SCRIPTS_DIR/../results"
DOCKER_DIR="$SCRIPTS_DIR/../docker"
PROJECT_ROOT="$SCRIPTS_DIR/../.."

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Function to print colored messages
print_status() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Function to check prerequisites
check_prerequisites() {
    print_status "Checking prerequisites..."
    
    local missing_tools=()
    
    # Check for required tools
    command -v docker >/dev/null 2>&1 || missing_tools+=("docker")
    command -v docker-compose >/dev/null 2>&1 || missing_tools+=("docker-compose")
    command -v wrk >/dev/null 2>&1 || missing_tools+=("wrk")
    command -v curl >/dev/null 2>&1 || missing_tools+=("curl")
    command -v bc >/dev/null 2>&1 || missing_tools+=("bc")
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        print_error "Missing required tools: ${missing_tools[*]}"
        print_error "Please install the missing tools and try again."
        exit 1
    fi
    
    # Check Docker daemon
    if ! docker info >/dev/null 2>&1; then
        print_error "Docker daemon is not running. Please start Docker and try again."
        exit 1
    fi
    
    print_success "All prerequisites satisfied"
}

# Function to clean up any existing benchmark containers/services
cleanup_existing() {
    print_status "Cleaning up existing benchmark containers..."
    
    # Stop any running benchmark services
    docker-compose -f "$DOCKER_DIR/docker-compose.benchmark.yml" down -v 2>/dev/null || true
    
    # Remove any leftover benchmark containers
    docker ps -aq --filter "label=benchmark.service" | xargs -r docker rm -f 2>/dev/null || true
    
    print_success "Cleanup completed"
}

# Function to setup benchmark environment
setup_environment() {
    print_status "Setting up benchmark environment..."
    
    # Ensure results directory exists
    mkdir -p "$RESULTS_DIR"
    
    # Build required Docker images
    print_status "Building Docker images..."
    
    # Build groxpi image
    docker build -t groxpi:latest "$PROJECT_ROOT"
    
    # Build UV test image
    docker build -t uv-tester "$DOCKER_DIR/uv/"
    
    print_success "Environment setup completed"
}

# Function to run API benchmarks
run_api_benchmarks() {
    print_status "Starting API benchmark suite..."
    
    # Start benchmark services
    docker-compose -f "$DOCKER_DIR/docker-compose.benchmark.yml" up -d
    
    # Wait a moment for startup
    sleep 10
    
    # Run API benchmarks
    if "$SCRIPTS_DIR/api-benchmark.sh"; then
        print_success "API benchmarks completed successfully"
    else
        print_error "API benchmarks failed"
        return 1
    fi
    
    # Stop benchmark services
    docker-compose -f "$DOCKER_DIR/docker-compose.benchmark.yml" down
    sleep 5
}

# Function to run download benchmarks
run_download_benchmarks() {
    print_status "Starting download benchmark suite..."
    
    # Start benchmark services
    docker-compose -f "$DOCKER_DIR/docker-compose.benchmark.yml" up -d
    
    # Wait for services to be fully ready
    sleep 15
    
    # Run download benchmarks
    if "$SCRIPTS_DIR/download-benchmark.sh"; then
        print_success "Download benchmarks completed successfully"
    else
        print_error "Download benchmarks failed"
        return 1
    fi
    
    # Stop benchmark services
    docker-compose -f "$DOCKER_DIR/docker-compose.benchmark.yml" down
    sleep 5
}

# Function to generate final report
generate_final_report() {
    print_status "Generating benchmark report..."
    
    local report_file="$RESULTS_DIR/benchmark-report-${TIMESTAMP}.md"
    
    # Generate markdown report
    {
        echo "# groxpi Benchmark Report"
        echo ""
        echo "**Generated**: $(date)"
        echo "**Timestamp**: $TIMESTAMP"
        echo ""
        echo "## Summary"
        echo ""
        echo "This report contains performance benchmarks comparing groxpi vs proxpi."
        echo ""
        echo "## Test Configuration"
        echo ""
        echo "### API Benchmarks (WRK)"
        echo "- Tool: wrk HTTP benchmarking tool"
        echo "- Duration: 15 seconds per test"
        echo "- Threads: 4"
        echo "- Connections: 50"
        echo "- Endpoints tested: /index/, /index/requests"
        echo ""
        echo "### Download Benchmarks (UV)"
        echo "- Tool: UV package installer in Docker"
        echo "- Test packages: requests, numpy, pandas"
        echo "- Tests: Fresh installs, cache performance"
        echo ""
        echo "## Result Files"
        echo ""
        echo "### API Benchmark Results"
        find "$RESULTS_DIR" -name "api-*${TIMESTAMP}.log" -exec basename {} \; 2>/dev/null | sort || true
        echo ""
        echo "### Download Benchmark Results"
        find "$RESULTS_DIR" -name "download-*${TIMESTAMP}.log" -exec basename {} \; 2>/dev/null | sort || true
        echo ""
        echo "## Analysis"
        echo ""
        echo "Review the individual log files for detailed performance metrics."
        echo ""
        echo "---"
        echo ""
        echo "*Generated by groxpi benchmark suite*"
        
    } > "$report_file"
    
    print_success "Report generated: $report_file"
}

# Function to display final results
display_results() {
    echo ""
    echo "======================================"
    echo "  BENCHMARK SUITE COMPLETED"
    echo "======================================"
    echo ""
    echo "📊 Results directory: $RESULTS_DIR"
    echo ""
    echo "🔍 Review the benchmark report:"
    echo "  benchmark-report-${TIMESTAMP}.md"
    echo ""
}

# Main execution function
main() {
    local start_time=$(date +%s)
    
    echo "======================================"
    echo "  groxpi BENCHMARK SUITE"
    echo "======================================"
    echo ""
    echo "Timestamp: $TIMESTAMP"
    echo "Results will be saved to: $RESULTS_DIR"
    echo ""
    
    # Parse command line arguments
    local run_api=true
    local run_download=true
    
    while [[ $# -gt 0 ]]; do
        case $1 in
            --api-only)
                run_download=false
                shift
                ;;
            --download-only)
                run_api=false
                shift
                ;;
            --no-api)
                run_api=false
                shift
                ;;
            --no-download)
                run_download=false
                shift
                ;;
            -h|--help)
                echo "Usage: $0 [options]"
                echo ""
                echo "Options:"
                echo "  --api-only      Run only API benchmarks"
                echo "  --download-only Run only download benchmarks"
                echo "  --no-api        Skip API benchmarks"
                echo "  --no-download   Skip download benchmarks"
                echo "  -h, --help      Show this help message"
                echo ""
                exit 0
                ;;
            *)
                print_error "Unknown option: $1"
                exit 1
                ;;
        esac
    done
    
    # Run benchmark suite
    check_prerequisites
    cleanup_existing
    setup_environment
    
    local failed_tests=()
    
    # Run selected benchmarks
    if [ "$run_api" = true ]; then
        if ! run_api_benchmarks; then
            failed_tests+=("API")
        fi
    else
        print_warning "Skipping API benchmarks"
    fi
    
    if [ "$run_download" = true ]; then
        if ! run_download_benchmarks; then
            failed_tests+=("Download")
        fi
    else
        print_warning "Skipping download benchmarks"
    fi
    
    # Final cleanup
    cleanup_existing
    
    # Generate report
    generate_final_report
    
    # Calculate total time
    local end_time=$(date +%s)
    local duration=$((end_time - start_time))
    local minutes=$((duration / 60))
    local seconds=$((duration % 60))
    
    # Display results
    display_results
    
    # Report final status
    if [ ${#failed_tests[@]} -eq 0 ]; then
        print_success "All benchmark suites completed successfully in ${minutes}m ${seconds}s"
        exit 0
    else
        print_error "Some benchmark suites failed: ${failed_tests[*]}"
        print_warning "Check individual log files for details"
        exit 1
    fi
}

# Cleanup function for script interruption
cleanup_on_exit() {
    print_warning "Benchmark interrupted, cleaning up..."
    cleanup_existing
    exit 130
}

# Set trap for cleanup
trap cleanup_on_exit INT TERM

# Run main function
main "$@"