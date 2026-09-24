#!/usr/bin/env bash
# cluster.sh — start a 3-node durablekv cluster locally for manual testing.
# Uses static --peer flags so all addresses are known upfront.
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/durablekv"
DATA_ROOT="/tmp/durablekv"

# shellcheck source=../internal/clusterlib.sh
. "$REPO_ROOT/examples/internal/clusterlib.sh"

cluster_parse_args "$@"

echo "==> Building durablekv..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/durablekv)

echo "==> Cleaning data dirs under $DATA_ROOT..."
rm -rf "$DATA_ROOT"
mkdir -p "$DATA_ROOT"/{n1,n2,n3}

cleanup() {
    echo ""
    echo "==> Stopping nodes..."
    cluster_stop
    echo "==> Removing $DATA_ROOT..."
    rm -rf "$DATA_ROOT"
    echo "Done."
}
trap cleanup INT TERM

# Wait for a Raft leader to be elected, then print per-node status.
# Ready when all three nodes answer and one of them leads.
cluster_ready() {
    local port up=0 elected=1
    for port in 8001 8002 8003; do
        local body
        body=$(curl -sf "http://localhost:$port/status" 2>/dev/null) || continue
        up=$((up + 1))
        printf '%s' "$body" | grep -q '"Leader"' && elected=0
    done
    [[ $up -ge 3 && $elected -eq 0 ]]
}

show_status() {
    printf '==> Node status:\n'
    local port
    for port in 8001 8002 8003; do
        local body
        body=$(curl -sf "http://localhost:$port/status" 2>/dev/null) || { printf '  :%-4s  unreachable\n' "$port"; continue; }
        if command -v jq >/dev/null 2>&1; then
            printf '%s' "$body" | jq -r '"  \(.id)  \(.state)  leader=\(.leader)  last_applied=\(.last_applied)"'
        else
            printf '  :%s  %s\n' "$port" "$body"
        fi
    done
    printf '\n'
}

cluster_start "n1" "$BINARY" --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 --data-dir "$DATA_ROOT/n1" \
    --peer n2=127.0.0.1:7002,127.0.0.1:8002 --peer n3=127.0.0.1:7003,127.0.0.1:8003 \

cluster_start "n2" "$BINARY" --id n2 --raft-addr 127.0.0.1:7002 --http-addr 127.0.0.1:8002 --data-dir "$DATA_ROOT/n2" \
    --peer n1=127.0.0.1:7001,127.0.0.1:8001 --peer n3=127.0.0.1:7003,127.0.0.1:8003 \

cluster_start "n3" "$BINARY" --id n3 --raft-addr 127.0.0.1:7003 --http-addr 127.0.0.1:8003 --data-dir "$DATA_ROOT/n3" \
    --peer n1=127.0.0.1:7001,127.0.0.1:8001 --peer n2=127.0.0.1:7002,127.0.0.1:8002 \

cluster_wait_ready "the cluster to elect a leader" 30 cluster_ready || cluster_give_up
show_status

echo "Endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
cluster_demo_intro 'A key-value store whose state machine keeps its own state on disk rather
than in memory. That changes what a restart has to do: instead of replaying
the log from the last snapshot, the node reads what it already has and skips
ahead.

The last step shows the number that makes that possible.'

cluster_smoke 'Write a key through a follower' \
    'curl -sS -L -X PUT http://localhost:8002/keys/greeting -H '\''Content-Type: application/json'\'' -d '\''{"value":"hello"}'\'' -w '\''HTTP %{http_code}\n'\''' \
    'Sent to n2, which does not lead. The 307 redirect takes it to the leader,
and -L follows it.'

cluster_smoke 'Read it back from a third node' \
    'curl -sS http://localhost:8003/keys/greeting' \
    'Linearizable: the node confirms it is current before answering, so this
cannot be older than the write that just returned.'

cluster_smoke 'Read it without that confirmation' \
    'curl -sS '\''http://localhost:8003/keys/greeting?consistency=stale'\''' \
    'Same answer, no leader round-trip. Bounded staleness in exchange for a
cheaper read.'

cluster_smoke 'Look at what the state machine has made durable' \
    'curl -sS http://localhost:8001/status' \
    'sm_applied is how far this node'\''s on-disk state has got. On restart the
node starts from there instead of from the last snapshot, so the log between
the two never has to be replayed.'

cluster_footer
