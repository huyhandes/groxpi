#!/usr/bin/env bash
# Local before/after regression check: build the current tree, warm one index
# and one wheel through real PyPI, then wrk the warm paths. Writes
# benchmarks/results/local-<label>.txt. Compare two labels by eye; no figures
# go into docs.
set -euo pipefail
LABEL="${1:?usage: local_regression.sh <label>}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$DIR/benchmarks/results/local-$LABEL.txt"
BIN="$(mktemp -d)/groxpi"
CACHE="$(mktemp -d)"
PORT=5099
PKG=requests
WHEEL=requests-2.32.3-py3-none-any.whl

go -C "$DIR" build -o "$BIN" ./cmd/groxpi
GROXPI_CACHE_DIR="$CACHE" PORT=$PORT GROXPI_LOGGING_LEVEL=WARN GROXPI_DOWNLOAD_TIMEOUT=30s \
  "$BIN" >/dev/null 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null; rm -rf "$CACHE" "$(dirname "$BIN")"' EXIT
for _ in $(seq 50); do curl -sf "http://localhost:$PORT/health" >/dev/null && break; sleep 0.1; done

curl -sf "http://localhost:$PORT/simple/$PKG/" >/dev/null
curl -sf -o /dev/null "http://localhost:$PORT/simple/$PKG/$WHEEL"

{
  echo "label=$LABEL commit=$(git -C "$DIR" rev-parse --short HEAD) date=$(date -u +%FT%TZ)"
  echo "== index warm: GET /simple/$PKG/"
  wrk -t4 -c64 -d10s --latency "http://localhost:$PORT/simple/$PKG/"
  echo "== wheel warm: GET /simple/$PKG/$WHEEL"
  wrk -t4 -c64 -d10s --latency "http://localhost:$PORT/simple/$PKG/$WHEEL"
  echo "== wheel warm, range 0-65535"
  wrk -t4 -c64 -d10s --latency -H "Range: bytes=0-65535" "http://localhost:$PORT/simple/$PKG/$WHEEL"
} | tee "$OUT"
