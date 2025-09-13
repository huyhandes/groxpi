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
        echo "Warning: Container '$container' not found or not running"
        return 1
    fi
    return 0
}

# Function to get container resource stats
get_container_stats() {
    local container=$1
    local timestamp=$2

    # Check if container is running
    if ! check_container "$container"; then
        echo "$timestamp,$container,N/A,N/A,N/A,N/A,N/A,N/A,N/A"
        return
    fi

    # Get container stats (timeout after 5 seconds)
    local stats
    if stats=$(timeout 5 docker stats --no-stream --format "{{.CPUPerc}},{{.MemUsage}},{{.MemPerc}},{{.NetIO}},{{.BlockIO}}" "$container" 2>/dev/null); then
        # Parse the stats
        local cpu_perc=$(echo "$stats" | cut -d',' -f1 | sed 's/%//')
        local mem_usage=$(echo "$stats" | cut -d',' -f2)
        local mem_perc=$(echo "$stats" | cut -d',' -f3 | sed 's/%//')
        local net_io=$(echo "$stats" | cut -d',' -f4)
        local block_io=$(echo "$stats" | cut -d',' -f5)

        # Extract memory in MB (format: "123.4MiB / 2.5GiB")
        local mem_used_mb="N/A"
        local mem_total_mb="N/A"
        if [[ $mem_usage =~ ([0-9.]+)([GMK]i?B)[[:space:]]*\/[[:space:]]*([0-9.]+)([GMK]i?B) ]]; then
            local used_val="${BASH_REMATCH[1]}"
            local used_unit="${BASH_REMATCH[2]}"
            local total_val="${BASH_REMATCH[3]}"
            local total_unit="${BASH_REMATCH[4]}"

            # Convert to MB
            case $used_unit in
                GiB|GB) mem_used_mb=$(echo "$used_val * 1024" | bc -l 2>/dev/null || echo "$used_val") ;;
                MiB|MB) mem_used_mb="$used_val" ;;
                KiB|KB) mem_used_mb=$(echo "$used_val / 1024" | bc -l 2>/dev/null || echo "$used_val") ;;
                *) mem_used_mb="$used_val" ;;
            esac

            case $total_unit in
                GiB|GB) mem_total_mb=$(echo "$total_val * 1024" | bc -l 2>/dev/null || echo "$total_val") ;;
                MiB|MB) mem_total_mb="$total_val" ;;
                KiB|KB) mem_total_mb=$(echo "$total_val / 1024" | bc -l 2>/dev/null || echo "$total_val") ;;
                *) mem_total_mb="$total_val" ;;
            esac
        fi

        # Extract network I/O (format: "1.2kB / 3.4kB")
        local net_rx="N/A"
        local net_tx="N/A"
        if [[ $net_io =~ ([0-9.]+[GMK]?i?B)[[:space:]]*\/[[:space:]]*([0-9.]+[GMK]?i?B) ]]; then
            net_rx="${BASH_REMATCH[1]}"
            net_tx="${BASH_REMATCH[2]}"
        fi

        # Extract block I/O (format: "5.6kB / 7.8kB")
        local disk_read="N/A"
        local disk_write="N/A"
        if [[ $block_io =~ ([0-9.]+[GMK]?i?B)[[:space:]]*\/[[:space:]]*([0-9.]+[GMK]?i?B) ]]; then
            disk_read="${BASH_REMATCH[1]}"
            disk_write="${BASH_REMATCH[2]}"
        fi

        # Output CSV line
        echo "$timestamp,$container,$cpu_perc,$mem_used_mb,$mem_total_mb,$mem_perc,$net_rx,$net_tx,$disk_read,$disk_write"
    else
        echo "$timestamp,$container,N/A,N/A,N/A,N/A,N/A,N/A,N/A,N/A"
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