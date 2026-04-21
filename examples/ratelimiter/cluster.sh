#!/usr/bin/env bash
# cluster.sh — start a 3-node ratelimiter cluster locally for manual testing.
# n1 bootstraps alone; n2 and n3 join via -join.
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/ratelimiter"
DATA_ROOT="/tmp/ratelimiter"

echo "==> Building ratelimiter..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/ratelimiter)

echo "==> Cleaning data dirs under $DATA_ROOT..."
rm -rf "$DATA_ROOT"
mkdir -p "$DATA_ROOT"/{n1,n2,n3}

PIDS=()
cleanup() {
    echo ""
    echo "==> Stopping nodes..."
    for pid in "${PIDS[@]}"; do
        kill "$pid" 2>/dev/null || true
    done
    wait 2>/dev/null || true
    echo "==> Removing $DATA_ROOT..."
    rm -rf "$DATA_ROOT"
    echo "Done."
}
trap cleanup INT TERM

echo "==> Starting n1 (bootstrap)..."
"$BINARY" -id n1 -raft :7001 -http :8001 -data "$DATA_ROOT/n1" \
    -peers n1=127.0.0.1:7001 \
    >"$DATA_ROOT/n1.log" 2>&1 &
PIDS+=($!)

sleep 0.5

echo "==> Starting n2 (joining n1)..."
"$BINARY" -id n2 -raft :7002 -http :8002 -data "$DATA_ROOT/n2" \
    -join 127.0.0.1:8001 \
    >"$DATA_ROOT/n2.log" 2>&1 &
PIDS+=($!)

sleep 0.5

echo "==> Starting n3 (joining n1)..."
"$BINARY" -id n3 -raft :7003 -http :8003 -data "$DATA_ROOT/n3" \
    -join 127.0.0.1:8001 \
    >"$DATA_ROOT/n3.log" 2>&1 &
PIDS+=($!)

echo ""
echo "Cluster is up. Endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
echo "Quick smoke-test:"
echo "  # Create a quota"
echo "  curl -X POST http://localhost:8001/quotas/premium-user -H 'Content-Type: application/json' -d '{\"max_tokens\":100,\"current_tokens\":100,\"refill_rate\":1}'"
echo "  # Take 5 tokens (follows 307 redirect to leader)"
echo "  curl -L -X POST http://localhost:8001/quotas/premium-user/mutate -H 'Content-Type: application/json' -d \"{\\\"name\\\":\\\"take\\\",\\\"args\\\":{\\\"requested\\\":5,\\\"now\\\":\$(date +%s)}}\""
echo "  # Check quota status"
echo "  curl http://localhost:8002/quotas/premium-user"
echo ""
echo "Logs: $DATA_ROOT/n{1,2,3}.log"
echo "Press Ctrl-C to stop."
echo ""

wait
