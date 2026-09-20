#!/usr/bin/env bash
# cluster.sh — start a 3-node durablekv cluster locally for manual testing.
# Uses static --peer flags so all addresses are known upfront.
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/durablekv"
DATA_ROOT="/tmp/durablekv"

echo "==> Building durablekv..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/durablekv)

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

# Wait for a Raft leader to be elected, then print per-node status.
wait_and_show_status() {
    local ports=(8001 8002 8003)
    local timeout=15

    printf "==> Waiting for cluster (up to %ds)..." "$timeout"
    local deadline=$((SECONDS + timeout))
    while [[ $SECONDS -lt $deadline ]]; do
        local up=0
        local elected=false
        for port in "${ports[@]}"; do
            local body
            body=$(curl -sf "http://localhost:$port/status" 2>/dev/null) || continue
            (( up++ )) || true
            printf '%s' "$body" | grep -q '"Leader"' && elected=true
        done
        if [[ $up -ge 3 ]] && $elected; then
            printf ' done.\n'
            break
        fi
        sleep 0.3
    done
    if [[ $SECONDS -ge $deadline ]]; then
        printf ' timed out (check %s/n*.log).\n' "$DATA_ROOT"
        return
    fi

    printf '==> Node status:\n'
    for port in "${ports[@]}"; do
        local body
        body=$(curl -sf "http://localhost:$port/status" 2>/dev/null) || { printf '  :%-4s  unreachable\n' "$port"; continue; }
        if command -v jq >/dev/null 2>&1; then
            printf '%s' "$body" | jq -r '"  \(.node_id)  \(.state)  leader=\(.leader)  raft_applied=\(.last_applied)  sm_applied=\(.sm_applied)"'
        else
            printf '  :%s  %s\n' "$port" "$body"
        fi
    done
    printf '\n'
}

echo "==> Starting n1..."
"$BINARY" --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 --data-dir "$DATA_ROOT/n1" \
    --peer n2=127.0.0.1:7002,127.0.0.1:8002 --peer n3=127.0.0.1:7003,127.0.0.1:8003 \
    >"$DATA_ROOT/n1.log" 2>&1 &
PIDS+=($!)

echo "==> Starting n2..."
"$BINARY" --id n2 --raft-addr 127.0.0.1:7002 --http-addr 127.0.0.1:8002 --data-dir "$DATA_ROOT/n2" \
    --peer n1=127.0.0.1:7001,127.0.0.1:8001 --peer n3=127.0.0.1:7003,127.0.0.1:8003 \
    >"$DATA_ROOT/n2.log" 2>&1 &
PIDS+=($!)

echo "==> Starting n3..."
"$BINARY" --id n3 --raft-addr 127.0.0.1:7003 --http-addr 127.0.0.1:8003 --data-dir "$DATA_ROOT/n3" \
    --peer n1=127.0.0.1:7001,127.0.0.1:8001 --peer n2=127.0.0.1:7002,127.0.0.1:8002 \
    >"$DATA_ROOT/n3.log" 2>&1 &
PIDS+=($!)

wait_and_show_status

echo "Endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
echo "Quick smoke-test:"
echo "  # Write (follows the 307 redirect to the leader)"
echo "  curl -s -L -X PUT http://localhost:8002/keys/greeting -H 'Content-Type: application/json' -d '{\"value\":\"hello\"}'"
echo "  # Linearizable read from any node"
echo "  curl -s http://localhost:8003/keys/greeting"
echo "  # Stale read: no round trip to the leader"
echo "  curl -s 'http://localhost:8003/keys/greeting?consistency=stale'"
echo "  # sm_applied is what the state machine has durable; it is what a restart skips"
echo "  curl -s http://localhost:8001/status | jq"
echo ""
echo "Logs: $DATA_ROOT/n{1,2,3}.log"
echo "Press Ctrl-C to stop."
echo ""

wait
