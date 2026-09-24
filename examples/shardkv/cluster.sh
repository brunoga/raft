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

# shellcheck source=../internal/clusterlib.sh
. "$REPO_ROOT/examples/internal/clusterlib.sh"

cluster_parse_args "$@"

echo "==> Building shardkv..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/shardkv)

echo "==> Cleaning data dirs under $DATA_ROOT..."
rm -rf "$DATA_ROOT"
mkdir -p "$DATA_ROOT"/{p1,p2,p3}

cleanup() {
    echo ""
    echo "==> Stopping nodes..."
    cluster_stop
    echo "==> Removing $DATA_ROOT..."
    rm -rf "$DATA_ROOT"
    echo "Done."
}
trap cleanup INT TERM

# Wait for all shards to elect leaders, then print the shard table from p1.
# Ready when every shard has a leader on some node.
cluster_ready() {
    local port leaders
    leaders=$(
        for port in 8001 8002 8003; do
            curl -sf "http://localhost:$port/shards" 2>/dev/null || true
        done | grep -o '"group_id":[0-9]*,"state":"Leader"' | grep -o '[0-9]*' | sort -un || true
    )
    [[ "$(printf '%s' "$leaders" | count_matches '[0-9]')" -ge "$SHARDS" ]]
}

show_shards() {
    printf '==> Shard leaders:\n'
    local port body
    for port in 8001 8002 8003; do
        body=$(curl -sf "http://localhost:$port/shards" 2>/dev/null) || continue
        if command -v jq >/dev/null 2>&1; then
            printf '%s' "$body" | jq -r --arg p ":$port" '
                sort_by(.group_id)[] | select(.state == "Leader") |
                "  shard=\(.group_id)  leader on \($p)  term=\(.term)"'
        else
            printf '%s\n' "$body"
        fi
    done
    printf '\n'
}

cluster_start "p1" "$BINARY" --id p1 --shards "$SHARDS" --raft-addr :7001 --http-addr :8001 \
    --data-dir "$DATA_ROOT/p1" \
    --peer p2=localhost:7002,localhost:8002 \
    --peer p3=localhost:7003,localhost:8003 \

cluster_start "p2" "$BINARY" --id p2 --shards "$SHARDS" --raft-addr :7002 --http-addr :8002 \
    --data-dir "$DATA_ROOT/p2" \
    --peer p1=localhost:7001,localhost:8001 \
    --peer p3=localhost:7003,localhost:8003 \

cluster_start "p3" "$BINARY" --id p3 --shards "$SHARDS" --raft-addr :7003 --http-addr :8003 \
    --data-dir "$DATA_ROOT/p3" \
    --peer p1=localhost:7001,localhost:8001 \
    --peer p2=localhost:7002,localhost:8002 \

cluster_wait_ready "$SHARDS shards to elect leaders" 30 cluster_ready || cluster_give_up
show_shards

echo "Endpoints ($SHARDS shards × 3 physical nodes):"
echo "  p1  http://localhost:8001"
echo "  p2  http://localhost:8002"
echo "  p3  http://localhost:8003"
echo ""
echo "Endpoints:"
echo "  p1  http://localhost:8001"
echo "  p2  http://localhost:8002"
echo "  p3  http://localhost:8003"
echo ""
cluster_demo_intro 'A sharded key-value store: several independent Raft groups on the same
three machines, with each key belonging to one shard.

One group per shard means one tenant'\''s writes do not queue behind another'\''s,
and leadership for the shards can be spread across the machines rather than
piling onto whichever one stayed up longest.'

cluster_smoke 'Write a key' \
    'curl -sS -L -X PUT http://localhost:8001/keys/hello -d world -w '\'' HTTP %{http_code}\n'\''' \
    'The key is hashed to a shard, and the request is redirected to whichever
node leads *that* shard -- which is not necessarily the node asked, and not
necessarily the same node for the next key.'

cluster_smoke 'Read it from a different machine' \
    'curl -sS http://localhost:8002/keys/hello' \
    'Linearizable, and served after the same redirect to the shard'\''s leader.'

cluster_smoke 'Write a few more keys' \
    'for k in alpha beta gamma delta; do curl -sS -L -X PUT "http://localhost:8001/keys/$k" -d "value-$k" -o /dev/null -w "$k %{http_code}  "; done; echo' \
    'Different keys, hashed across the shards.'

cluster_smoke 'See which shards this node leads' \
    'curl -sS http://localhost:8001/shards' \
    'Each shard is a separate Raft group with its own term and its own leader.
One machine leading all of them would be a bottleneck; the balance controller
exists to stop that.'

cluster_smoke 'See the whole cluster'\''s leadership' \
    'curl -sS http://localhost:8001/raft/status' \
    'The view a balance controller plans over. Leadership is spread across the
three machines rather than concentrated.'

cluster_smoke 'Delete the key' \
    'curl -sS -L -X DELETE http://localhost:8001/keys/hello -o /dev/null -w '\''HTTP %{http_code}\n'\''; curl -sS -o /dev/null -w '\''after delete: HTTP %{http_code}\n'\'' http://localhost:8002/keys/hello' \
    '404 afterwards, from a node that did not take the delete.'

cluster_footer
