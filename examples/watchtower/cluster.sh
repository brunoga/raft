#!/usr/bin/env bash
# cluster.sh — start a 3-node watchtower cluster locally for manual testing.
# Uses static --peer flags so all addresses are known upfront.
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

# Anything passed to this script is passed on to every node, so that
#   ./cluster.sh --apply-delay 2ms
# starts a cluster whose state machine is slow on purpose.
EXTRA=("$@")

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/watchtower"
DATA_ROOT="/tmp/watchtower"

echo "==> Building watchtower..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/watchtower)

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
            body=$(curl -sf "http://localhost:$port/health" 2>/dev/null) || continue
            (( up++ )) || true
            printf '%s' "$body" | grep -q '"state":"Leader"' && elected=true
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
        body=$(curl -sf "http://localhost:$port/health" 2>/dev/null) || { printf '  :%-4s  unreachable\n' "$port"; continue; }
        if command -v jq >/dev/null 2>&1; then
            printf '%s' "$body" | jq -r '"  \(.node)  \(.state)  term=\(.term)  unreachable=\(.unreachable_peers // [] | length)  saturation=\(.apply_saturation)"'
        else
            printf '  :%s  %s\n' "$port" "$body"
        fi
    done
    printf '\n'
}

echo "==> Starting n1..."
"$BINARY" --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 --data-dir "$DATA_ROOT/n1" "${EXTRA[@]+"${EXTRA[@]}"}" \
    --peer n2=127.0.0.1:7002,127.0.0.1:8002 --peer n3=127.0.0.1:7003,127.0.0.1:8003 \
    >"$DATA_ROOT/n1.log" 2>&1 &
PIDS+=($!)

echo "==> Starting n2..."
"$BINARY" --id n2 --raft-addr 127.0.0.1:7002 --http-addr 127.0.0.1:8002 --data-dir "$DATA_ROOT/n2" "${EXTRA[@]+"${EXTRA[@]}"}" \
    --peer n1=127.0.0.1:7001,127.0.0.1:8001 --peer n3=127.0.0.1:7003,127.0.0.1:8003 \
    >"$DATA_ROOT/n2.log" 2>&1 &
PIDS+=($!)

echo "==> Starting n3..."
"$BINARY" --id n3 --raft-addr 127.0.0.1:7003 --http-addr 127.0.0.1:8003 --data-dir "$DATA_ROOT/n3" "${EXTRA[@]+"${EXTRA[@]}"}" \
    --peer n1=127.0.0.1:7001,127.0.0.1:8001 --peer n2=127.0.0.1:7002,127.0.0.1:8002 \
    >"$DATA_ROOT/n3.log" 2>&1 &
PIDS+=($!)

wait_and_show_status

echo "Endpoints:"
echo "  n1  http://127.0.0.1:8001"
echo "  n2  http://127.0.0.1:8002"
echo "  n3  http://127.0.0.1:8003"
echo ""
echo "Quick smoke-test:"
echo "  # Live event stream"
echo "  curl -N http://localhost:8001/events"
echo "  # Freeze a follower and watch its peers notice"
echo "  pkill -STOP -f 'watchtower --id n3'; sleep 3; curl -s http://localhost:8001/observed | jq .peers"
echo "  pkill -CONT -f 'watchtower --id n3'"
echo "  # Apply saturation: near 1 means the state machine is the constraint"
echo "  curl -s http://localhost:8001/metrics | grep raft_apply_saturation"
echo ""
echo "Logs: $DATA_ROOT/n{1,2,3}.log"
echo "Press Ctrl-C to stop."
echo ""

wait
