#!/bin/bash

# UV S3 Integration Test Script
# Tests groxpi with S3 backend using uv package manager in Docker

set -euo pipefail

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

# Check if running in CI or locally
if [ -n "${CI:-}" ]; then
    log_info "Running in CI environment"
    GROXPI_HOST="127.0.0.1"
else
    log_info "Running locally"
    GROXPI_HOST="127.0.0.1"
fi

# Configuration
GROXPI_PORT="${PORT:-5000}"
GROXPI_URL="http://${GROXPI_HOST}:${GROXPI_PORT}"
TEST_DIR="${TEST_DIR:-/tmp/uv-test-$$}"
GROXPI_BINARY="${GROXPI_BINARY:-./groxpi}"

# Function to check if groxpi server is running
check_groxpi_server() {
    local max_attempts=30
    local attempt=1

    log_info "Waiting for groxpi server at $GROXPI_URL..."

    while [ $attempt -le $max_attempts ]; do
        if curl -sf "${GROXPI_URL}/health" >/dev/null 2>&1; then
            log_success "Groxpi server is ready!"
            return 0
        fi

        echo "  Attempt $attempt/$max_attempts: Server not ready yet..."
        sleep 1
        ((attempt++))
    done

    log_error "Groxpi server did not become ready within 30 seconds"
    return 1
}

# Function to start groxpi server (if not already running)
start_groxpi_server() {
    # Check if server is already running
    if curl -sf "${GROXPI_URL}/health" >/dev/null 2>&1; then
        log_info "Groxpi server is already running at $GROXPI_URL"
        return 0
    fi

    # Build groxpi if binary doesn't exist
    if [ ! -f "$GROXPI_BINARY" ]; then
        log_info "Building groxpi binary..."
        go build -o "$GROXPI_BINARY" cmd/groxpi/main.go || {
            log_error "Failed to build groxpi binary"
            return 1
        }
    fi

    # Start groxpi with S3 backend
    log_info "Starting groxpi server with S3 backend..."

    export GROXPI_STORAGE_TYPE=s3
    export AWS_ENDPOINT_URL="http://127.0.0.1:9000"
    export AWS_ACCESS_KEY_ID=minioadmin
    export AWS_SECRET_ACCESS_KEY=minioadmin
    export GROXPI_S3_BUCKET=groxpi-test
    export GROXPI_S3_PREFIX=uv-integration-test
    export GROXPI_S3_USE_SSL=false
    export GROXPI_S3_FORCE_PATH_STYLE=true
    export GROXPI_LOGGING_LEVEL=DEBUG
    export PORT=$GROXPI_PORT

    # Start server in background
    $GROXPI_BINARY &
    GROXPI_PID=$!

    # Store PID for cleanup
    echo $GROXPI_PID > /tmp/groxpi-test.pid

    # Wait for server to be ready
    check_groxpi_server || {
        kill $GROXPI_PID 2>/dev/null || true
        return 1
    }

    return 0
}

# Function to stop groxpi server
stop_groxpi_server() {
    if [ -f /tmp/groxpi-test.pid ]; then
        local pid=$(cat /tmp/groxpi-test.pid)
        if kill -0 $pid 2>/dev/null; then
            log_info "Stopping groxpi server (PID: $pid)..."
            kill $pid
            rm -f /tmp/groxpi-test.pid
        fi
    fi
}

# Function to test package installation with uv
test_uv_install() {
    local package_name=$1
    local test_name="${2:-$package_name}"

    log_info "Testing installation of '$package_name' with uv..."

    # Create a temporary directory for this test
    local project_dir="${TEST_DIR}/${test_name}"
    mkdir -p "$project_dir"

    # Don't create pyproject.toml manually, let uv init handle it
    # We'll configure the index after initialization

    # Initialize uv project first, then add package
    log_info "Initializing uv project..."

    if ! docker run --rm \
        --network host \
        -v "${project_dir}:/workspace" \
        -w /workspace \
        ghcr.io/astral-sh/uv:latest \
        init --no-readme --name "test-${test_name}"; then
        log_error "Failed to initialize uv project"
        return 1
    fi

    # Configure groxpi as index in pyproject.toml
    log_info "Configuring groxpi index..."
    cat >> "${project_dir}/pyproject.toml" <<EOF

[[tool.uv.index]]
name = "groxpi"
url = "${GROXPI_URL}/simple/"
default = true
EOF

    # Run uv in Docker container
    log_info "Running uv add ${package_name} in Docker..."

    if docker run --rm \
        --network host \
        -v "${project_dir}:/workspace" \
        -w /workspace \
        ghcr.io/astral-sh/uv:latest \
        add "$package_name" --no-cache; then

        log_success "Successfully installed $package_name"

        # Verify package was added to pyproject.toml
        if grep -q "$package_name" "${project_dir}/pyproject.toml"; then
            log_success "Package $package_name found in pyproject.toml"
            return 0
        else
            log_error "Package $package_name not found in pyproject.toml"
            return 1
        fi
    else
        log_error "Failed to install $package_name"
        return 1
    fi
}

# Function to test concurrent installations
test_concurrent_installs() {
    log_info "Testing concurrent package installations..."

    local packages=("click" "pyyaml" "six")
    local pids=()
    local failed=0

    # Start installations in parallel
    for pkg in "${packages[@]}"; do
        (
            test_uv_install "$pkg" "concurrent-$pkg"
        ) &
        pids+=($!)
    done

    # Wait for all installations to complete
    for pid in "${pids[@]}"; do
        if ! wait $pid; then
            ((failed++))
        fi
    done

    if [ $failed -eq 0 ]; then
        log_success "All concurrent installations completed successfully"
        return 0
    else
        log_error "$failed concurrent installations failed"
        return 1
    fi
}

# Function to test installation with dependencies
test_uv_with_dependencies() {
    log_info "Testing installation of package with dependencies..."

    if test_uv_install "fastapi" "fastapi-deps"; then
        # Check if dependencies were also installed
        local project_dir="${TEST_DIR}/fastapi-deps"
        local uv_lock="${project_dir}/uv.lock"

        if [ -f "$uv_lock" ]; then
            if grep -q "pydantic" "$uv_lock" && grep -q "starlette" "$uv_lock"; then
                log_success "Dependencies (pydantic, starlette) were correctly resolved"
                return 0
            else
                log_warning "Some expected dependencies not found in uv.lock"
                return 1
            fi
        else
            log_warning "uv.lock file not found"
            return 1
        fi
    else
        return 1
    fi
}

# Cleanup function
cleanup() {
    log_info "Cleaning up..."

    # Remove test directory
    if [ -d "$TEST_DIR" ]; then
        rm -rf "$TEST_DIR"
    fi

    # Stop groxpi server if we started it
    if [ "${GROXPI_STARTED:-false}" = "true" ]; then
        stop_groxpi_server
    fi
}

# Main test execution
main() {
    log_info "=== UV S3 Integration Test ==="
    log_info "Groxpi URL: $GROXPI_URL"
    log_info "Test directory: $TEST_DIR"

    # Set up cleanup on exit
    trap cleanup EXIT

    # Create test directory
    mkdir -p "$TEST_DIR"

    # Check if we're in CI or need to start server locally
    if [ -n "${CI:-}" ] && [ -n "${TEST_S3_ENDPOINT:-}" ]; then
        # In CI, server should be started by the test
        log_info "CI environment detected, starting groxpi server..."
        start_groxpi_server || exit 1
        GROXPI_STARTED=true
    else
        # Check if server is already running
        if ! check_groxpi_server; then
            log_error "Groxpi server is not running. Please start it first or set CI=1 to auto-start."
            exit 1
        fi
        GROXPI_STARTED=false
    fi

    # Pull UV Docker image
    log_info "Pulling UV Docker image..."
    docker pull ghcr.io/astral-sh/uv:latest || {
        log_error "Failed to pull UV Docker image"
        exit 1
    }

    # Run tests
    local test_failed=0

    # Test 1: Simple package installation
    log_info ""
    log_info "Test 1: Simple package installation"
    if ! test_uv_install "requests" "simple-requests"; then
        ((test_failed++))
    fi

    # Test 2: Package with dependencies
    log_info ""
    log_info "Test 2: Package with dependencies"
    if ! test_uv_with_dependencies; then
        ((test_failed++))
    fi

    # Test 3: Concurrent installations
    log_info ""
    log_info "Test 3: Concurrent installations"
    if ! test_concurrent_installs; then
        ((test_failed++))
    fi

    # Summary
    log_info ""
    log_info "=== Test Summary ==="
    if [ $test_failed -eq 0 ]; then
        log_success "All tests passed!"
        exit 0
    else
        log_error "$test_failed test(s) failed"
        exit 1
    fi
}

# Run main function
main "$@"