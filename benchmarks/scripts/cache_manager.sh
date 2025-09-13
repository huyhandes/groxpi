#!/bin/bash

# Cache Management Utility Script
# Clears cache for both groxpi and proxpi using DELETE API
# Usage: ./cache_manager.sh <groxpi_url> <proxpi_url> [operation]

set -euo pipefail

# Configuration
GROXPI_URL="${1:-}"
PROXPI_URL="${2:-}"
OPERATION="${3:-clear-all}"
TIMEOUT=30

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
    echo "  groxpi_url  - groxpi server URL (e.g., http://server1:5005)"
    echo "  proxpi_url  - proxpi server URL (e.g., http://server2:5006)"
    echo "  operation   - Operation to perform (default: clear-all)"
    echo ""
    echo "Operations:"
    echo "  clear-all        - Clear all caches on both servers"
    echo "  clear-groxpi     - Clear only groxpi cache"
    echo "  clear-proxpi     - Clear only proxpi cache"
    echo "  clear-package    - Clear specific package cache (requires package name)"
    echo "  verify           - Verify cache clear operations"
    echo "  test-connection  - Test connection to both servers"
    echo ""
    echo "Examples:"
    echo "  $0 http://server1:5005 http://server2:5006"
    echo "  $0 http://server1:5005 http://server2:5006 clear-all"
    echo "  $0 http://server1:5005 http://server2:5006 clear-package numpy"
    echo "  $0 http://server1:5005 http://server2:5006 test-connection"
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

# Function to clear cache list (all packages index)
clear_cache_list() {
    local url=$1
    local name=$2

    log_info "Clearing cache list for $name..."

    local response
    local http_code

    # Perform DELETE request to clear cache list
    if response=$(curl -s --connect-timeout 10 --max-time $TIMEOUT \
                      -w "HTTP_CODE:%{http_code}" \
                      -X DELETE "$url/cache/list" 2>&1); then

        http_code=$(echo "$response" | grep -o 'HTTP_CODE:[0-9]*' | cut -d':' -f2)
        response_body=$(echo "$response" | sed 's/HTTP_CODE:[0-9]*$//')

        case $http_code in
            200|204)
                log_success "$name cache list cleared successfully"
                if [ -n "$response_body" ]; then
                    echo "  Response: $response_body"
                fi
                return 0
                ;;
            404)
                log_warning "$name cache list endpoint not found (HTTP $http_code)"
                return 1
                ;;
            405)
                log_error "$name cache list endpoint does not support DELETE (HTTP $http_code)"
                return 1
                ;;
            *)
                log_error "$name cache list clear failed (HTTP $http_code)"
                if [ -n "$response_body" ]; then
                    echo "  Response: $response_body"
                fi
                return 1
                ;;
        esac
    else
        log_error "$name cache list clear request failed: $response"
        return 1
    fi
}

# Function to clear specific package cache
clear_package_cache() {
    local url=$1
    local name=$2
    local package=$3

    log_info "Clearing cache for package '$package' on $name..."

    local response
    local http_code

    # Perform DELETE request to clear specific package cache
    if response=$(curl -s --connect-timeout 10 --max-time $TIMEOUT \
                      -w "HTTP_CODE:%{http_code}" \
                      -X DELETE "$url/cache/$package" 2>&1); then

        http_code=$(echo "$response" | grep -o 'HTTP_CODE:[0-9]*' | cut -d':' -f2)
        response_body=$(echo "$response" | sed 's/HTTP_CODE:[0-9]*$//')

        case $http_code in
            200|204)
                log_success "$name package '$package' cache cleared successfully"
                if [ -n "$response_body" ]; then
                    echo "  Response: $response_body"
                fi
                return 0
                ;;
            404)
                log_warning "$name package '$package' not found in cache (HTTP $http_code)"
                return 0  # Not an error - package might not be cached
                ;;
            *)
                log_error "$name package '$package' cache clear failed (HTTP $http_code)"
                if [ -n "$response_body" ]; then
                    echo "  Response: $response_body"
                fi
                return 1
                ;;
        esac
    else
        log_error "$name package '$package' cache clear request failed: $response"
        return 1
    fi
}

# Function to verify cache has been cleared
verify_cache_clear() {
    local url=$1
    local name=$2

    log_info "Verifying cache state for $name..."

    # Make a simple request to see if cache is cold
    local start_time=$(date +%s%N)
    if curl -sf --connect-timeout 10 --max-time $TIMEOUT "$url/simple/" >/dev/null 2>&1; then
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
            if ! test_connection "$GROXPI_URL" "groxpi"; then
                exit 1
            fi
            clear_cache_list "$GROXPI_URL" "groxpi"
            ;;

        clear-proxpi)
            if ! test_connection "$PROXPI_URL" "proxpi"; then
                exit 1
            fi
            clear_cache_list "$PROXPI_URL" "proxpi"
            ;;

        clear-package)
            if [ $# -lt 4 ]; then
                log_error "Package name required for clear-package operation"
                echo "Usage: $0 <groxpi_url> <proxpi_url> clear-package <package_name>"
                exit 1
            fi

            local package_name="$4"
            log_info "Clearing cache for package: $package_name"

            # Test connections first
            groxpi_ok=true
            proxpi_ok=true

            if ! test_connection "$GROXPI_URL" "groxpi"; then
                groxpi_ok=false
            fi

            if ! test_connection "$PROXPI_URL" "proxpi"; then
                proxpi_ok=false
            fi

            # Clear package cache on both servers
            if $groxpi_ok; then
                clear_package_cache "$GROXPI_URL" "groxpi" "$package_name"
            fi

            if $proxpi_ok; then
                clear_package_cache "$PROXPI_URL" "proxpi" "$package_name"
            fi
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
            log_info "Clearing all caches..."

            # Test connections first
            groxpi_ok=true
            proxpi_ok=true

            if ! test_connection "$GROXPI_URL" "groxpi"; then
                groxpi_ok=false
            fi

            if ! test_connection "$PROXPI_URL" "proxpi"; then
                proxpi_ok=false
            fi

            if ! $groxpi_ok && ! $proxpi_ok; then
                log_error "Both servers are unreachable"
                exit 1
            fi

            # Clear caches
            success=true

            if $groxpi_ok; then
                if ! clear_cache_list "$GROXPI_URL" "groxpi"; then
                    success=false
                fi
            fi

            if $proxpi_ok; then
                if ! clear_cache_list "$PROXPI_URL" "proxpi"; then
                    success=false
                fi
            fi

            echo ""
            if $success; then
                log_success "All cache operations completed successfully"

                # Optional verification
                log_info "Performing verification..."
                if $groxpi_ok; then
                    verify_cache_clear "$GROXPI_URL" "groxpi"
                fi
                if $proxpi_ok; then
                    verify_cache_clear "$PROXPI_URL" "proxpi"
                fi
            else
                log_error "Some cache operations failed"
                exit 1
            fi
            ;;
    esac

    echo ""
    log_success "Cache management operation completed"
}

# Run main function
main "$@"