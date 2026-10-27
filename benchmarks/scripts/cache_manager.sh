#!/bin/bash

# Cache Management Utility Script
# Empties the caches of groxpi and proxpi by recreating their containers
# Usage: ./cache_manager.sh <groxpi_url> <proxpi_url> [operation]

set -euo pipefail

# Configuration
GROXPI_URL="${1:-}"
PROXPI_URL="${2:-}"
OPERATION="${3:-clear-all}"
TIMEOUT=30

# A cold run recreates the proxies from this compose file: the containers and
# their anonymous cache volumes are removed, then fresh ones are started. Neither
# proxy's DELETE API empties its cache - groxpi's DELETE /cache/list touches no
# list, and proxpi's DELETE /cache/<package> drops only the in-memory file list,
# keeping every downloaded file on disk - so API eviction left "cold" runs warm.
COMPOSE_FILE="${BENCH_COMPOSE_FILE:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../docker" && pwd)/docker-compose.benchmark.yml}"
GROXPI_SERVICE=groxpi-bench
PROXPI_SERVICE=proxpi-bench

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
    echo "Usage: $0 <groxpi_url> <proxpi_url> [operation]"
    echo ""
    echo "Parameters:"
    echo "  groxpi_url  - groxpi server URL (e.g., http://localhost:5005)"
    echo "  proxpi_url  - proxpi server URL (e.g., http://localhost:5006)"
    echo "  operation   - Operation to perform (default: clear-all)"
    echo ""
    echo "Operations:"
    echo "  clear-all        - Recreate both proxies with empty caches"
    echo "  clear-groxpi     - Recreate only groxpi ($GROXPI_SERVICE)"
    echo "  clear-proxpi     - Recreate only proxpi ($PROXPI_SERVICE)"
    echo "  verify           - Time one index request on each server"
    echo "  test-connection  - Test connection to both servers"
    echo ""
    echo "Clearing recreates the services of BENCH_COMPOSE_FILE on this host"
    echo "(default: $COMPOSE_FILE),"
    echo "so the URLs must point at those containers."
    echo ""
    echo "Examples:"
    echo "  $0 http://localhost:5005 http://localhost:5006"
    echo "  $0 http://localhost:5005 http://localhost:5006 test-connection"
}

# Function to validate URLs
validate_url() {
    local url=$1
    local name=$2

    if [[ ! $url =~ ^https?://[a-zA-Z0-9.-]+(:[0-9]+)?/?$ ]]; then
        log_error "Invalid $name URL format: $url"
        log_error "Expected format: http://hostname:port or https://hostname:port"
        return 1
    fi
    return 0
}

# Function to test connection to server
test_connection() {
    local url=$1
    local name=$2

    log_info "Testing connection to $name ($url)..."

    # Try health endpoint first
    if curl -sf --connect-timeout 10 --max-time $TIMEOUT "$url/health" >/dev/null 2>&1; then
        log_success "$name health endpoint responded"
        return 0
    fi

    # Try root endpoint as fallback
    if curl -sf --connect-timeout 10 --max-time $TIMEOUT "$url/" >/dev/null 2>&1; then
        log_success "$name root endpoint responded"
        return 0
    fi

    log_error "$name connection failed ($url)"
    return 1
}

# Recreate services with empty caches: stop and remove the containers together
# with their anonymous volumes, then start fresh ones and wait until healthy. Any
# failure aborts: carrying on would measure whatever is left as a cold cache.
recreate_services() {
    log_info "Recreating $* with empty caches..."
    if docker compose -f "$COMPOSE_FILE" rm -sfv "$@" &&
       docker compose -f "$COMPOSE_FILE" up -d --wait "$@"; then
        log_success "$* recreated with empty caches"
        return 0
    fi
    log_error "Recreating $* failed; aborting rather than measure a warm cache as cold."
    exit 1
}

# Function to time one index request (a rough check, not a measurement)
verify_cache_clear() {
    local url=$1
    local name=$2

    log_info "Verifying cache state for $name..."

    # Use different endpoints for groxpi vs proxpi
    local endpoint
    if [[ "$name" == "groxpi" ]]; then
        endpoint="/simple/"
    else
        endpoint="/index/"
    fi

    # Make a simple request to see if cache is cold
    local start_time=$(date +%s%N)
    if curl -sf --connect-timeout 10 --max-time $TIMEOUT "$url$endpoint" >/dev/null 2>&1; then
        local end_time=$(date +%s%N)
        local duration_ms=$(( (end_time - start_time) / 1000000 ))

        log_info "$name responded to index request in ${duration_ms}ms"
        if [ $duration_ms -gt 100 ]; then
            log_success "$name appears to have cold cache (slow response)"
        else
            log_warning "$name responded quickly (cache might still be warm)"
        fi
    else
        log_error "$name verification request failed"
        return 1
    fi
}

# Main function
main() {
    # Parse arguments
    if [ $# -lt 2 ]; then
        show_usage
        exit 1
    fi

    # Validate URLs
    if ! validate_url "$GROXPI_URL" "groxpi"; then
        exit 1
    fi

    if ! validate_url "$PROXPI_URL" "proxpi"; then
        exit 1
    fi

    # Remove trailing slashes
    GROXPI_URL="${GROXPI_URL%/}"
    PROXPI_URL="${PROXPI_URL%/}"

    local timestamp=$(date '+%Y-%m-%d %H:%M:%S')
    echo "=== Cache Management Operation ==="
    echo "Timestamp: $timestamp"
    echo "Operation: $OPERATION"
    echo "groxpi URL: $GROXPI_URL"
    echo "proxpi URL: $PROXPI_URL"
    echo ""

    case "$OPERATION" in
        test-connection)
            log_info "Testing connections..."
            groxpi_ok=true
            proxpi_ok=true

            if ! test_connection "$GROXPI_URL" "groxpi"; then
                groxpi_ok=false
            fi

            if ! test_connection "$PROXPI_URL" "proxpi"; then
                proxpi_ok=false
            fi

            if $groxpi_ok && $proxpi_ok; then
                log_success "Both servers are reachable"
                exit 0
            else
                log_error "One or more servers are not reachable"
                exit 1
            fi
            ;;

        clear-groxpi)
            recreate_services "$GROXPI_SERVICE"
            ;;

        clear-proxpi)
            recreate_services "$PROXPI_SERVICE"
            ;;

        verify)
            log_info "Verifying cache state..."

            # Test connections first
            if ! test_connection "$GROXPI_URL" "groxpi" || ! test_connection "$PROXPI_URL" "proxpi"; then
                exit 1
            fi

            verify_cache_clear "$GROXPI_URL" "groxpi"
            verify_cache_clear "$PROXPI_URL" "proxpi"
            ;;

        clear-all|*)
            # No verification request afterwards: it would warm the caches just emptied.
            recreate_services "$GROXPI_SERVICE" "$PROXPI_SERVICE"
            ;;
    esac

    echo ""
    log_success "Cache management operation completed"
}

# Run main function
main "$@"
