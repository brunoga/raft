#!/usr/bin/env bash
# cluster.sh — start a 3-node serviceregistry cluster plus two agents.
#
# The registry nodes join one at a time via --join. The agents register
# themselves through the easyraft client and hold their leases; kill one and
# its entry disappears from the registry a few seconds later, on every node,
# with nothing having been told about it.
#
# Press Ctrl-C to stop everything and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/serviceregistry"
AGENT="$SCRIPT_DIR/agent-bin"
DATA_ROOT="/tmp/serviceregistry"
ENDPOINTS="localhost:8001,localhost:8002,localhost:8003"

echo "==> Building serviceregistry and agent..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/serviceregistry)
(cd "$REPO_ROOT" && go build -o "$AGENT" ./examples/serviceregistry/agent)

echo "==> Cleaning data dirs under $DATA_ROOT..."
rm -rf "$DATA_ROOT"
mkdir -p "$DATA_ROOT"/{n1,n2,n3}

PIDS=()
AGENT_PIDS=()
cleanup() {
    echo ""
    echo "==> Stopping agents..."
    for pid in "${AGENT_PIDS[@]:-}"; do
        kill "$pid" 2>/dev/null || true
    done
    echo "==> Stopping nodes..."
    for pid in "${PIDS[@]:-}"; do
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
            return
        fi
        sleep 0.3
    done
    printf ' timed out (check %s/n*.log).\n' "$DATA_ROOT"
}

echo "==> Starting n1 (bootstrap)..."
"$BINARY" --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 \
    --data-dir "$DATA_ROOT/n1" --lease-sweep 500ms \
    >"$DATA_ROOT/n1.log" 2>&1 &
PIDS+=($!)

sleep 0.5

echo "==> Starting n2 (joining n1)..."
"$BINARY" --id n2 --raft-addr 127.0.0.1:7002 --http-addr 127.0.0.1:8002 \
    --data-dir "$DATA_ROOT/n2" --lease-sweep 500ms --join localhost:8001 \
    >"$DATA_ROOT/n2.log" 2>&1 &
PIDS+=($!)

sleep 0.5

echo "==> Starting n3 (joining n1)..."
"$BINARY" --id n3 --raft-addr 127.0.0.1:7003 --http-addr 127.0.0.1:8003 \
    --data-dir "$DATA_ROOT/n3" --lease-sweep 500ms --join localhost:8001 \
    >"$DATA_ROOT/n3.log" 2>&1 &
PIDS+=($!)

wait_and_show_status

echo "==> Starting two api instances..."
for instance in api-1 api-2; do
    "$AGENT" --service api --id "$instance" --addr "10.0.0.${instance##*-}:9000" \
        --endpoints "$ENDPOINTS" --ttl 5s \
        >"$DATA_ROOT/$instance.log" 2>&1 &
    AGENT_PIDS+=($!)
done

sleep 1

echo ""
echo "Registry endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
echo "Who is up? Any node answers:"
echo "  curl -s http://localhost:8003/services/api"
echo ""
echo "Watch an instance leave. Kill one agent and ask again after ~5s:"
echo "  kill ${AGENT_PIDS[0]}    # SIGTERM: revokes, so it goes at once"
echo "  kill -9 ${AGENT_PIDS[1]} # SIGKILL: nothing runs, so it goes when the lease expires"
echo "  curl -s http://localhost:8003/services/api"
echo ""
echo "The leases themselves, through easyraft's own endpoint:"
echo "  curl -s http://localhost:8001/__leases"
echo ""
echo "Logs: $DATA_ROOT/{n1,n2,n3,api-1,api-2}.log"
echo "Press Ctrl-C to stop."
echo ""

wait
