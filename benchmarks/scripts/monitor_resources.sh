#!/bin/bash

# Continuous Resource Monitoring Script
# Monitors Docker containers and logs resource usage to CSV
# Usage: ./monitor_resources.sh <output_file> [groxpi_container] [proxpi_container] [interval_seconds]
# Press Ctrl+C to stop monitoring

set -euo pipefail

# Configuration
OUTPUT_FILE=${1:-""}
GROXPI_CONTAINER=${2:-"groxpi-bench"}
PROXPI_CONTAINER=${3:-"proxpi-bench"}
INTERVAL=${4:-2}  # Default 2 second interval

# Validate parameters
if [ -z "$OUTPUT_FILE" ]; then
    echo "Usage: $0 <output_file> [groxpi_container] [proxpi_container] [interval_seconds]"
    echo "Example: $0 results/resources-20240101_120000.csv groxpi-bench proxpi-bench 2"
    echo ""
    echo "Press Ctrl+C to stop monitoring"
    exit 1
fi

# Ensure output directory exists
mkdir -p "$(dirname "$OUTPUT_FILE")"

# Function to check if container exists and is running
check_container() {
    local container=$1
    if ! docker ps --format "{{.Names}}" | grep -q "^${container}$"; then
        echo "Warning: Container '$container' not found or not running" >&2
        return 1
    fi
    return 0
}

# Run a command with a time limit. macOS ships no `timeout`; use coreutils'
# `gtimeout` if present, otherwise run unbounded rather than fail every sample.
with_timeout() {
    local secs=$1
    shift
    if command -v timeout >/dev/null 2>&1; then
        timeout "$secs" "$@"
    elif command -v gtimeout >/dev/null 2>&1; then
        gtimeout "$secs" "$@"
    else
        "$@"
    fi
}

# Convert a docker size ("12.5MiB", "3.4kB", "0B") to MB; empty if unparseable.
to_mb() {
    awk -v v="$1" 'BEGIN {
        if (!match(v, /^[0-9.]+/)) exit
        n = substr(v, 1, RLENGTH); u = substr(v, RLENGTH + 1)
        if (u ~ /^[Kk]i?B$/) n /= 1024
        else if (u ~ /^Gi?B$/) n *= 1024
        else if (u ~ /^Ti?B$/) n *= 1024 * 1024
        else if (u == "B") n /= 1024 * 1024
        else if (u !~ /^Mi?B$/) exit
        printf "%.3f", n
    }'
}

# Function to get container resource stats
get_container_stats() {
    local container=$1
    local timestamp=$2

    # Check if container is running
    if ! check_container "$container"; then
        echo "$timestamp,$container,,,,,,,,"
        return
    fi

    # Get container stats (timeout after 5 seconds)
    local stats
    if stats=$(with_timeout 5 docker stats --no-stream --format "{{.CPUPerc}},{{.MemUsage}},{{.MemPerc}},{{.NetIO}},{{.BlockIO}}" "$container" 2>/dev/null); then
        # Parse the stats
        local cpu_perc=$(echo "$stats" | cut -d',' -f1 | sed 's/%//; s/^--$//')
        local mem_usage=$(echo "$stats" | cut -d',' -f2)
        local mem_perc=$(echo "$stats" | cut -d',' -f3 | sed 's/%//; s/^--$//')
        local net_io=$(echo "$stats" | cut -d',' -f4)
        local block_io=$(echo "$stats" | cut -d',' -f5)

        # Each I/O pair is "<used> / <total>" or "<in> / <out>", e.g.
        # "123.4MiB / 2.5GiB", "1.2kB / 3.4kB". Values are kept as docker prints
        # them for the I/O columns and converted to MB for memory; anything
        # unparseable is left empty.
        local mem_used_mb="" mem_total_mb="" net_rx="" net_tx="" disk_read="" disk_write=""
        local size='([0-9.]+[kKMGT]?i?B)[[:space:]]*/[[:space:]]*([0-9.]+[kKMGT]?i?B)'
        if [[ $mem_usage =~ $size ]]; then
            mem_used_mb=$(to_mb "${BASH_REMATCH[1]}")
            mem_total_mb=$(to_mb "${BASH_REMATCH[2]}")
        fi
        if [[ $net_io =~ $size ]]; then
            net_rx="${BASH_REMATCH[1]}"
            net_tx="${BASH_REMATCH[2]}"
        fi
        if [[ $block_io =~ $size ]]; then
            disk_read="${BASH_REMATCH[1]}"
            disk_write="${BASH_REMATCH[2]}"
        fi

        # Output CSV line
        echo "$timestamp,$container,$cpu_perc,$mem_used_mb,$mem_total_mb,$mem_perc,$net_rx,$net_tx,$disk_read,$disk_write"
    else
        echo "$timestamp,$container,,,,,,,,"
    fi
}

# Function to cleanup on exit
cleanup() {
    echo ""
    echo "Stopping resource monitoring..."
    exit 0
}

# Set up signal handlers
trap cleanup SIGINT SIGTERM

echo "Starting resource monitoring..."
echo "Monitoring containers: $GROXPI_CONTAINER, $PROXPI_CONTAINER"
echo "Output file: $OUTPUT_FILE"
echo "Interval: ${INTERVAL}s"
echo "Press Ctrl+C to stop"
echo ""

# Write CSV header
echo "timestamp,container,cpu_percent,memory_used_mb,memory_total_mb,memory_percent,network_rx,network_tx,disk_read,disk_write" > "$OUTPUT_FILE"

# Monitor loop
while true; do
    timestamp=$(date '+%Y-%m-%d %H:%M:%S')

    # Get stats for both containers
    get_container_stats "$GROXPI_CONTAINER" "$timestamp" >> "$OUTPUT_FILE"
    get_container_stats "$PROXPI_CONTAINER" "$timestamp" >> "$OUTPUT_FILE"

    sleep "$INTERVAL"
done