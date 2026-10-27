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

fail() { echo "local_regression: $*" >&2; exit 1; }

# The warm-up must cache: a redirect or an error would leave wrk timing
# something other than a warm hit.
warm() {
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' "$1" || true)
  [ "$code" = 200 ] || fail "warm-up GET $1 returned HTTP $code, want 200"
}

# wrk counts every reply as a request, so error replies and socket errors
# would pass for throughput; either fails the run. Despite its name, wrk's
# "Non-2xx or 3xx responses" counter counts only status > 399 (4xx and 5xx).
run_wrk() {
  local out
  out=$(wrk -t4 -c64 -d10s --latency "$@") || { echo "$out"; echo "FAILED: wrk exited non-zero"; return 1; }
  echo "$out"
  if grep -q 'Non-2xx or 3xx responses:' <<<"$out"; then
    echo "FAILED: 4xx/5xx responses"
    return 1
  fi
  # "Socket errors: connect N, read N, write N, timeout N"
  if awk '/Socket errors:/ { gsub(",", ""); bad = ($4 + $6 + $8 + $10 > 0); exit } END { exit !bad }' <<<"$out"; then
    echo "FAILED: socket errors"
    return 1
  fi
}

# Set before anything can fail, so an early exit still removes the temp dirs.
trap '[ -n "${PID:-}" ] && kill "$PID" 2>/dev/null; rm -rf "$CACHE" "$(dirname "$BIN")"' EXIT
mkdir -p "$(dirname "$OUT")"  # results/ is gitignored, so absent in a fresh clone
go -C "$DIR" build -o "$BIN" ./cmd/groxpi
# Something already answering on the port would be measured instead of this build.
! curl -s -o /dev/null "http://localhost:$PORT/" || fail "port $PORT is already in use"
GROXPI_CACHE_DIR="$CACHE" PORT=$PORT GROXPI_LOGGING_LEVEL=WARN GROXPI_DOWNLOAD_TIMEOUT=30s \
  "$BIN" >/dev/null 2>&1 &
PID=$!
up=
for _ in $(seq 50); do curl -sf "http://localhost:$PORT/health" >/dev/null && up=1 && break; sleep 0.1; done
[ -n "$up" ] || fail "groxpi did not become healthy on port $PORT"
# A healthy answer while our process is gone means another one holds the port.
kill -0 "$PID" 2>/dev/null || fail "groxpi exited; is port $PORT already in use?"

warm "http://localhost:$PORT/simple/$PKG/"
warm "http://localhost:$PORT/simple/$PKG/$WHEEL"
kill -0 "$PID" 2>/dev/null || fail "groxpi exited during warm-up"

if ! {
  echo "label=$LABEL commit=$(git -C "$DIR" rev-parse --short HEAD) date=$(date -u +%FT%TZ)"
  echo "== index warm: GET /simple/$PKG/"
  run_wrk "http://localhost:$PORT/simple/$PKG/" || exit 1
  echo "== wheel warm: GET /simple/$PKG/$WHEEL"
  run_wrk "http://localhost:$PORT/simple/$PKG/$WHEEL" || exit 1
  echo "== wheel warm, range 0-65535"
  run_wrk -H "Range: bytes=0-65535" "http://localhost:$PORT/simple/$PKG/$WHEEL" || exit 1
} | tee "$OUT"; then
  mv "$OUT" "${OUT%.txt}.failed.txt"
  fail "load test failed; see ${OUT%.txt}.failed.txt"
fi
