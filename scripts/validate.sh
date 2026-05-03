#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

cleanup() {
    [ -n "${PID1:-}" ] && kill -9 "$PID1" 2>/dev/null || true
    [ -n "${PID2:-}" ] && kill -9 "$PID2" 2>/dev/null || true
}
trap cleanup EXIT

pkill -9 -f otelcol-retrosampling 2>/dev/null || true
rm -f data/retrosampling*.ring
mkdir -p data

./bin/otelcol-retrosampling --config example/otelcol1.yaml >/tmp/otelcol1.log 2>&1 &
PID1=$!
./bin/otelcol-retrosampling --config example/otelcol2.yaml >/tmp/otelcol2.log 2>&1 &
PID2=$!

wait_for_port() {
    local port=$1
    local deadline=$((SECONDS + 5))
    while ! nc -z localhost "$port" 2>/dev/null; do
        if [ "$SECONDS" -ge "$deadline" ]; then
            echo "timed out waiting for :$port — collector logs:" >&2
            tail -20 /tmp/otelcol1.log /tmp/otelcol2.log >&2 || true
            exit 1
        fi
        sleep 0.2
    done
}

wait_for_port 4317
wait_for_port 4318

./bin/tracegen -run 60 "$@"
