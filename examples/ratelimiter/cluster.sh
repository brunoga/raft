#!/usr/bin/env bash
# cluster.sh — start a 3-node ratelimiter cluster locally for manual testing.
# n1 bootstraps alone; n2 and n3 join via -join.
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/ratelimiter"
DATA_ROOT="/tmp/ratelimiter"

# shellcheck source=../internal/clusterlib.sh
. "$REPO_ROOT/examples/internal/clusterlib.sh"

cluster_parse_args "$@"

echo "==> Building ratelimiter..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/ratelimiter)

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

# Ready when the seed lists three members and one of them leads.
cluster_ready() {
    local body
    body=$(curl -sf "http://localhost:8001/members" 2>/dev/null) || return 1
    [[ "$(printf '%s' "$body" | count_occurrences '"leader":')" -ge 3 ]] &&
        printf '%s' "$body" | grep -q '"leader":true'
}

show_members() {
    printf '==> Cluster members:\n'
    local body
    body=$(curl -sf "http://localhost:8001/members" 2>/dev/null) || return 0
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

cluster_start "n1" "$BINARY" -id n1 -raft 127.0.0.1:7001 -http 127.0.0.1:8001 -data "$DATA_ROOT/n1" \
    -peers n1=127.0.0.1:7001 \

sleep 0.5

cluster_start "n2" "$BINARY" -id n2 -raft 127.0.0.1:7002 -http 127.0.0.1:8002 -data "$DATA_ROOT/n2" \
    -join 127.0.0.1:8001 \

sleep 0.5

cluster_start "n3" "$BINARY" -id n3 -raft 127.0.0.1:7003 -http 127.0.0.1:8003 -data "$DATA_ROOT/n3" \
    -join 127.0.0.1:8001 \

cluster_wait_ready "the cluster to elect a leader" 30 cluster_ready || cluster_give_up
show_members

echo "Endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
cluster_demo_intro 'A distributed token bucket. The interesting part is not the counting but
the clock: refill depends on time, and a state machine may not read one --
every replica applies the same entry and must reach the same number.

The timestamp therefore travels inside the request, fixed before the entry is
proposed.'

cluster_smoke 'Create a quota with 100 tokens' \
    'curl -sS -L -X POST http://localhost:8001/quotas/premium-user -H '\''Content-Type: application/json'\'' -d '\''{"max_tokens":100,"current_tokens":100,"refill_rate":1}'\'' -w '\'' HTTP %{http_code}\n'\''' \
    'A bucket that holds 100 tokens and refills at one per second.'

cluster_smoke 'Take five tokens' \
    'curl -sS -L -X POST http://localhost:8001/quotas/premium-user/mutate -H '\''Content-Type: application/json'\'' -d "{\"name\":\"take\",\"args\":{\"requested\":5,\"now\":$(date +%s)}}"' \
    'The `now` is supplied by the caller, not read inside the state machine.
That is what lets every replica compute the same refill: the mutation is a
pure function of the entry, clock reading included.'

cluster_smoke 'Read the quota from another node' \
    'curl -sS http://localhost:8002/quotas/premium-user' \
    '95 tokens, on a node that did not serve the request. Every replica ran
the same mutation and got the same answer.'

cluster_smoke 'Ask for more than the bucket holds' \
    'curl -sS -L -X POST http://localhost:8001/quotas/premium-user/mutate -H '\''Content-Type: application/json'\'' -d "{\"name\":\"take\",\"args\":{\"requested\":1000,\"now\":$(date +%s)}}"' \
    'Refused by the mutation itself, so the refusal is agreed by the cluster
rather than decided by whichever node was asked.'

cluster_smoke 'Confirm nothing was taken' \
    'curl -sS http://localhost:8002/quotas/premium-user' \
    'Still 95. A refused take leaves the bucket alone.'

cluster_footer
