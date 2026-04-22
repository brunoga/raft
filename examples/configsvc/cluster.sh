#!/usr/bin/env bash
# cluster.sh — start a 3-node configsvc cluster locally for manual testing.
# Nodes join one at a time via --join (no pre-shared peer list required).
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/configsvc"
DATA_ROOT="/tmp/configsvc"

echo "==> Building configsvc..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/configsvc)

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

# Wait for a Raft leader to be elected, then print the member table.
wait_and_show_status() {
    local seed="http://localhost:8001"
    local timeout=30

    printf "==> Waiting for cluster (up to %ds)..." "$timeout"
    local deadline=$((SECONDS + timeout))
    while [[ $SECONDS -lt $deadline ]]; do
        local body
        body=$(curl -sf "$seed/members" 2>/dev/null) || { sleep 0.3; continue; }
        member_count=$(printf '%s' "$body" | grep -o '"leader":' | wc -l)
        if [[ $member_count -ge 3 ]] && printf '%s' "$body" | grep -q '"leader":true'; then
            printf ' done.\n'
            break
        fi
        sleep 0.3
    done
    if [[ $SECONDS -ge $deadline ]]; then
        printf ' timed out (check %s/n*.log).\n' "$DATA_ROOT"
        return
    fi

    printf '==> Cluster members:\n'
    local body
    body=$(curl -sf "$seed/members" 2>/dev/null)
    if command -v jq >/dev/null 2>&1; then
        printf '%s' "$body" | jq -r '
            .members | sort_by(.id)[] |
            "  \(.id)  \(if .leader then "leader  " else "follower" end)  \(.raft_addr)"
        '
    else
        printf '%s\n' "$body"
    fi
    printf '\n'
}

echo "==> Starting n1 (bootstrap)..."
"$BINARY" --id n1 --raft-addr :7001 --http-addr :8001 --data-dir "$DATA_ROOT/n1" \
    >"$DATA_ROOT/n1.log" 2>&1 &
PIDS+=($!)

sleep 0.5

echo "==> Starting n2 (joining n1)..."
"$BINARY" --id n2 --raft-addr :7002 --http-addr :8002 --data-dir "$DATA_ROOT/n2" \
    --join localhost:8001 \
    >"$DATA_ROOT/n2.log" 2>&1 &
PIDS+=($!)

sleep 0.5

echo "==> Starting n3 (joining n1)..."
"$BINARY" --id n3 --raft-addr :7003 --http-addr :8003 --data-dir "$DATA_ROOT/n3" \
    --join localhost:8001 \
    >"$DATA_ROOT/n3.log" 2>&1 &
PIDS+=($!)

wait_and_show_status

echo "Endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
echo "Quick smoke-test:"
echo "  curl -L -X PUT http://localhost:8001/configs/db.host -H 'Content-Type: application/json' -d '{\"value\":\"localhost\"}'"
echo "  curl http://localhost:8002/configs/db.host"
echo "  curl -N http://localhost:8003/watch"
echo ""
echo "Logs: $DATA_ROOT/n{1,2,3}.log"
echo "Press Ctrl-C to stop."
echo ""

wait
