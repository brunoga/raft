#!/usr/bin/env bash
# cluster.sh — start a 3-node configsvc cluster locally for manual testing.
# Nodes join one at a time via --join (no pre-shared peer list required).
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/configsvc"
DATA_ROOT="/tmp/configsvc"

# shellcheck source=../internal/clusterlib.sh
. "$REPO_ROOT/examples/internal/clusterlib.sh"

cluster_parse_args "$@"

echo "==> Building configsvc..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/configsvc)

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

cluster_start "n1" "$BINARY" --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 --data-dir "$DATA_ROOT/n1" \

sleep 0.5

cluster_start "n2" "$BINARY" --id n2 --raft-addr 127.0.0.1:7002 --http-addr 127.0.0.1:8002 --data-dir "$DATA_ROOT/n2" \
    --join localhost:8001 \

sleep 0.5

cluster_start "n3" "$BINARY" --id n3 --raft-addr 127.0.0.1:7003 --http-addr 127.0.0.1:8003 --data-dir "$DATA_ROOT/n3" \
    --join localhost:8001 \

cluster_wait_ready "the cluster to elect a leader" 30 cluster_ready || cluster_give_up
show_members

echo "Endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
cluster_demo_intro 'A replicated configuration store. Values are agreed by Raft, so every node
returns the same answer, and a write sent to a follower is redirected to the
leader rather than refused.

The part worth watching is the last three steps: a read returns the key'\''s
revision, and handing it back makes the next write conditional on nothing
having changed in between.'

cluster_smoke 'Set a value, from whichever node you happen to ask' \
    'curl -sS -L -X PUT http://localhost:8001/configs/db.host -H '\''Content-Type: application/json'\'' -d '\''{"value":"localhost"}'\'' -w '\''%{http_code}\n'\''' \
    'The -L matters. Only the leader can take a write; a follower answers 307
with the leader'\''s address, and curl follows it. That is why any endpoint
works and why a client never has to know who leads.'

cluster_smoke 'Read it back from a different node' \
    'curl -sS http://localhost:8002/configs/db.host' \
    'A linearizable read. The node confirms it is current with the leader
before answering, so this cannot return a value older than the write that
just succeeded -- even though it is a different node.'

cluster_smoke 'Read it again without that confirmation' \
    'curl -sS '\''http://localhost:8003/configs/db.host?consistency=stale'\''' \
    'The same value, no round-trip to the leader. A stale read may lag by up
to a heartbeat, which is the trade: cheaper, and bounded rather than
arbitrary.'

cluster_smoke 'Take the revision the key is at' \
    'etag=$(curl -sS -D- -o /dev/null http://localhost:8002/configs/db.host | awk '\''/[Ee][Tt]ag:/ {print $2}'\'' | tr -d '\''\r'\''); echo "revision $etag"' \
    'The ETag is the index of the Raft entry that last wrote this key. Not the
Version field in the body: that is a timestamp, and two writers in the same
nanosecond would get the same one.'

cluster_smoke 'Write conditionally on that revision' \
    'curl -sS -o /dev/null -w '\''HTTP %{http_code}\n'\'' -L -X PUT -H "If-Match: $etag" -H '\''Content-Type: application/json'\'' -d '\''{"value":"db.internal"}'\'' http://localhost:8001/configs/db.host' \
    '204: the key had not moved, so the write applied. This is the safe form
of read-modify-write -- no lock, and no window in which somebody else'\''s
write is silently overwritten.'

cluster_smoke 'Write again with the same, now stale, revision' \
    'curl -sS -o /dev/null -w '\''HTTP %{http_code}\n'\'' -L -X PUT -H "If-Match: $etag" -H '\''Content-Type: application/json'\'' -d '\''{"value":"lost"}'\'' http://localhost:8001/configs/db.host' \
    '412, and nothing is written. Without the condition this write would have
succeeded and thrown away the previous one, with no error anywhere. Re-read,
redo the work, retry.'

cluster_smoke 'Confirm the refused write changed nothing' \
    'curl -sS http://localhost:8003/configs/db.host' \
    'Still db.internal, from a third node. The refusal was a refusal, not a
partial write.'

cluster_footer
