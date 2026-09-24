#!/usr/bin/env bash
# cluster.sh — start a 3-node idprovider cluster locally for manual testing.
# Uses static --peer flags so all addresses are known upfront.
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/idprovider"
DATA_ROOT="/tmp/idprovider"

# shellcheck source=../internal/clusterlib.sh
. "$REPO_ROOT/examples/internal/clusterlib.sh"

cluster_parse_args "$@"

echo "==> Building idprovider..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/idprovider)

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

cluster_start "n1" "$BINARY" --id n1 --raft-addr :7001 --http-addr :8001 --data-dir "$DATA_ROOT/n1" \
    --peer n2=localhost:7002,localhost:8002 --peer n3=localhost:7003,localhost:8003 \

cluster_start "n2" "$BINARY" --id n2 --raft-addr :7002 --http-addr :8002 --data-dir "$DATA_ROOT/n2" \
    --peer n1=localhost:7001,localhost:8001 --peer n3=localhost:7003,localhost:8003 \

cluster_start "n3" "$BINARY" --id n3 --raft-addr :7003 --http-addr :8003 --data-dir "$DATA_ROOT/n3" \
    --peer n1=localhost:7001,localhost:8001 --peer n2=localhost:7002,localhost:8002 \

cluster_wait_ready "the cluster to elect a leader" 30 cluster_ready || cluster_give_up
show_status

echo "Endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
cluster_demo_intro 'A distributed ID allocator. Each domain is a counter, and an allocation
reserves a contiguous range from it -- so two callers never receive the same
ID, even when they ask at the same moment from different nodes.

The last two steps are the interesting ones: a retry that reuses a client'\''s
sequence number returns the range it already got, rather than burning a new
one.'

cluster_smoke 'Create a domain to allocate from' \
    'curl -sS -L -X POST http://localhost:8001/domains/orders -w '\'' HTTP %{http_code}\n'\''' \
    'A domain is just a named counter. The -L follows the redirect a follower
answers with, so this works against any node.'

cluster_smoke 'Allocate ten IDs' \
    'curl -sS -L -X POST '\''http://localhost:8001/domains/orders/next?count=10'\'' -H '\''X-Client-ID: svc1'\'' -H '\''X-Seq-Num: 1'\''' \
    'The reply is a half-open range. It was reserved by one committed Raft
entry, so no other caller can be handed any part of it.'

cluster_smoke 'Allocate ten more, from a different node' \
    'curl -sS -L -X POST '\''http://localhost:8002/domains/orders/next?count=10'\'' -H '\''X-Client-ID: svc2'\'' -H '\''X-Seq-Num: 1'\''' \
    'The range continues where the last one stopped. Different node, different
client, no overlap -- which is the whole job.'

cluster_smoke 'Repeat the first request exactly' \
    'curl -sS -L -X POST '\''http://localhost:8001/domains/orders/next?count=10'\'' -H '\''X-Client-ID: svc1'\'' -H '\''X-Seq-Num: 1'\''' \
    'The same range as step 2 comes back. The cluster remembers the answer it
gave that (client, sequence number) pair, so a caller that retries after a
lost response does not consume a second range.'

cluster_smoke 'List the domains and their counters' \
    'curl -sS http://localhost:8002/domains' \
    'Twenty allocated in total, not thirty: the retry above was absorbed. Read
from a node that took none of the writes.'

cluster_footer
