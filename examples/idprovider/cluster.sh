#!/usr/bin/env bash
# cluster.sh — start a 3-node idprovider cluster locally for manual testing.
# Uses static --peer flags so all addresses are known upfront.
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/idprovider"
DATA_ROOT="/tmp/idprovider"

echo "==> Building idprovider..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/idprovider)

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

echo "==> Starting n1..."
"$BINARY" --id n1 --raft-addr :7001 --http-addr :8001 --data-dir "$DATA_ROOT/n1" \
    --peer n2=localhost:7002 --peer n3=localhost:7003 \
    >"$DATA_ROOT/n1.log" 2>&1 &
PIDS+=($!)

echo "==> Starting n2..."
"$BINARY" --id n2 --raft-addr :7002 --http-addr :8002 --data-dir "$DATA_ROOT/n2" \
    --peer n1=localhost:7001 --peer n3=localhost:7003 \
    >"$DATA_ROOT/n2.log" 2>&1 &
PIDS+=($!)

echo "==> Starting n3..."
"$BINARY" --id n3 --raft-addr :7003 --http-addr :8003 --data-dir "$DATA_ROOT/n3" \
    --peer n1=localhost:7001 --peer n2=localhost:7002 \
    >"$DATA_ROOT/n3.log" 2>&1 &
PIDS+=($!)

echo ""
echo "Cluster is up. Endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
echo "Quick smoke-test:"
echo "  # Create a domain"
echo "  curl -s -X POST http://localhost:8001/domains/orders"
echo "  # Allocate IDs"
echo "  curl -s -X POST 'http://localhost:8001/domains/orders/next?count=10' -H 'X-Client-ID: svc1' -H 'X-Seq-Num: 1'"
echo "  # List domains"
echo "  curl -s http://localhost:8002/domains | jq"
echo "  # Node status"
echo "  curl -s http://localhost:8001/status | jq"
echo ""
echo "Logs: $DATA_ROOT/n{1,2,3}.log"
echo "Press Ctrl-C to stop."
echo ""

wait
