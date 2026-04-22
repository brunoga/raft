#!/usr/bin/env bash
# cluster.sh — start a 3-node shardkv cluster locally for manual testing.
# Each physical node hosts one replica of each shard (4 shards × 3 nodes).
# Uses static --peer flags so all addresses are known upfront.
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/shardkv"
DATA_ROOT="/tmp/shardkv"
SHARDS=4   # must be identical on every node; change here to try different shard counts

echo "==> Building shardkv..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/shardkv)

echo "==> Cleaning data dirs under $DATA_ROOT..."
rm -rf "$DATA_ROOT"
mkdir -p "$DATA_ROOT"/{p1,p2,p3}

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

# Wait for all shards to elect leaders, then print the shard table from p1.
wait_and_show_status() {
    local seed="http://localhost:8001"
    local timeout=15

    printf "==> Waiting for %d shards to elect leaders (up to %ds)..." "$SHARDS" "$timeout"
    local deadline=$((SECONDS + timeout))
    while [[ $SECONDS -lt $deadline ]]; do
        local body
        body=$(curl -sf "$seed/shards" 2>/dev/null) || { sleep 0.3; continue; }
        local leader_count
        leader_count=$(printf '%s' "$body" | grep -c '"state":"Leader"' 2>/dev/null || true)
        if [[ "$leader_count" -ge "$SHARDS" ]]; then
            printf ' done.\n'
            break
        fi
        sleep 0.3
    done
    if [[ $SECONDS -ge $deadline ]]; then
        printf ' timed out (check %s/p*.log).\n' "$DATA_ROOT"
        return
    fi

    printf '==> Shard status (via p1):\n'
    local body
    body=$(curl -sf "$seed/shards" 2>/dev/null)
    if command -v jq >/dev/null 2>&1; then
        printf '%s' "$body" | jq -r 'sort_by(.group_id)[] |
            "  shard=\(.group_id)  \(.state)  term=\(.term)  last_applied=\(.last_applied)"'
    else
        printf '%s\n' "$body"
    fi
    printf '\n'
}

echo "==> Starting p1..."
"$BINARY" --id p1 --shards "$SHARDS" --raft-addr :7001 --http-addr :8001 \
    --data-dir "$DATA_ROOT/p1" \
    --peer p2=localhost:7002,localhost:8002 \
    --peer p3=localhost:7003,localhost:8003 \
    >"$DATA_ROOT/p1.log" 2>&1 &
PIDS+=($!)

echo "==> Starting p2..."
"$BINARY" --id p2 --shards "$SHARDS" --raft-addr :7002 --http-addr :8002 \
    --data-dir "$DATA_ROOT/p2" \
    --peer p1=localhost:7001,localhost:8001 \
    --peer p3=localhost:7003,localhost:8003 \
    >"$DATA_ROOT/p2.log" 2>&1 &
PIDS+=($!)

echo "==> Starting p3..."
"$BINARY" --id p3 --shards "$SHARDS" --raft-addr :7003 --http-addr :8003 \
    --data-dir "$DATA_ROOT/p3" \
    --peer p1=localhost:7001,localhost:8001 \
    --peer p2=localhost:7002,localhost:8002 \
    >"$DATA_ROOT/p3.log" 2>&1 &
PIDS+=($!)

wait_and_show_status

echo "Endpoints ($SHARDS shards × 3 physical nodes):"
echo "  p1  http://localhost:8001"
echo "  p2  http://localhost:8002"
echo "  p3  http://localhost:8003"
echo ""
echo "Quick smoke-test:"
echo "  # Write (follows 307 redirect to shard leader automatically)"
echo "  curl -s -L -X PUT http://localhost:8001/keys/hello -d world"
echo "  # Read (linearizable, any node)"
echo "  curl -s http://localhost:8002/keys/hello"
echo "  # Delete"
echo "  curl -s -L -X DELETE http://localhost:8001/keys/hello"
echo "  # Shard status on p1"
echo "  curl -s http://localhost:8001/shards | jq"
echo "  # Cross-node leader-balance view"
echo "  curl -s http://localhost:8001/raft/status | jq"
echo ""
echo "Logs: $DATA_ROOT/p{1,2,3}.log"
echo "Press Ctrl-C to stop."
echo ""

wait
