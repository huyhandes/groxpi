#!/bin/bash

# Simple benchmark runner for groxpi vs proxpi comparison
# This is the main entry point for running benchmarks

set -euo pipefail

SCRIPTS_DIR="$(dirname "$0")/scripts"

echo "======================================"
echo "  groxpi Performance Benchmarks"
echo "======================================"
echo ""
echo "This will run:"
echo "1. API performance tests (WRK)"
echo "2. Package download tests (UV)"
echo ""
echo "Starting benchmarks..."
echo ""

# Run all benchmarks
"$SCRIPTS_DIR/run-all.sh" "$@"