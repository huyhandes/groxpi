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

# Convert a docker size to decimal MB (10^6 bytes); empty if unparseable.
# docker stats prints memory in binary units ("12.5MiB") and network/block IO
# in decimal ones ("3.4kB", "1.2MB", "0B"), so each suffix keeps its own base.
to_mb() {
    awk -v v="$1" 'BEGIN {
        if (!match(v, /^[0-9.]+/)) exit
        n = substr(v, 1, RLENGTH); u = substr(v, RLENGTH + 1)
        if (u == "B") m = 1
        else if (u == "kB" || u == "KB") m = 1e3
        else if (u == "MB") m = 1e6
        else if (u == "GB") m = 1e9
        else if (u == "TB") m = 1e12
        else if (u == "KiB") m = 1024
        else if (u == "MiB") m = 1024 ^ 2
        else if (u == "GiB") m = 1024 ^ 3
        else if (u == "TiB") m = 1024 ^ 4
        else exit
        printf "%.3f", n * m / 1e6
    }'
}

# Turn one container's docker stats line into a CSV row; an empty line (container
# not running, or docker stats failed) gives a row of empty fields.
stats_row() {
    local container=$1
    local timestamp=$2
    local stats=$3

    if [ -n "$stats" ]; then
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

# Sample both containers with one `docker stats --no-stream` call, so a tick's
# rows come from one sample and a tick costs one stats call, not two. The real
# cadence is still INTERVAL plus that call (docker takes a second reading to
# compute CPU%, about two seconds).
sample_containers() {
    local timestamp=$1
    local running=() container stats=""

    for container in "$GROXPI_CONTAINER" "$PROXPI_CONTAINER"; do
        if check_container "$container"; then
            running+=("$container")
        fi
    done

    # Get container stats (timeout after 5 seconds)
    if [ ${#running[@]} -gt 0 ]; then
        stats=$(with_timeout 5 docker stats --no-stream --format "{{.Name}},{{.CPUPerc}},{{.MemUsage}},{{.MemPerc}},{{.NetIO}},{{.BlockIO}}" "${running[@]}" 2>/dev/null) || stats=""
    fi

    # Split the output: each line starts with its container's name
    for container in "$GROXPI_CONTAINER" "$PROXPI_CONTAINER"; do
        stats_row "$container" "$timestamp" "$(printf '%s\n' "$stats" | awk -F, -v n="$container" '$1 == n { sub(/^[^,]*,/, ""); print; exit }')"
    done
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
    sample_containers "$timestamp" >> "$OUTPUT_FILE"

    sleep "$INTERVAL"
done