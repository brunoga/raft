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

# shellcheck source=../internal/clusterlib.sh
. "$REPO_ROOT/examples/internal/clusterlib.sh"

cluster_parse_args "$@"

echo "==> Building serviceregistry and agent..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/serviceregistry)
(cd "$REPO_ROOT" && go build -o "$AGENT" ./examples/serviceregistry/agent)

echo "==> Cleaning data dirs under $DATA_ROOT..."
rm -rf "$DATA_ROOT"
mkdir -p "$DATA_ROOT"/{n1,n2,n3}

cleanup() {
    echo ""
    echo "==> Stopping agents and nodes..."
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

cluster_start "n1" "$BINARY" --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 \
    --data-dir "$DATA_ROOT/n1" --lease-sweep 500ms \

sleep 0.5

cluster_start "n2" "$BINARY" --id n2 --raft-addr 127.0.0.1:7002 --http-addr 127.0.0.1:8002 \
    --data-dir "$DATA_ROOT/n2" --lease-sweep 500ms --join localhost:8001 \

sleep 0.5

cluster_start "n3" "$BINARY" --id n3 --raft-addr 127.0.0.1:7003 --http-addr 127.0.0.1:8003 \
    --data-dir "$DATA_ROOT/n3" --lease-sweep 500ms --join localhost:8001 \

cluster_wait_ready "the cluster to elect a leader" 30 cluster_ready || cluster_give_up
show_members

echo "==> Starting two api instances..."
for instance in api-1 api-2; do
    cluster_start "$instance" "$AGENT" --service api --id "$instance" \
        --addr "10.0.0.${instance##*-}:9000" --endpoints "$ENDPOINTS" --ttl 5s
done

sleep 1

echo ""
echo "Endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
cluster_demo_intro 'A service registry built on leases. Instances register themselves and keep
their registration alive; when one stops, nothing has to notice -- the lease
simply stops being renewed and every replica deletes the entry at the same
point in the log.

Two agents are already running. The demonstration stops them in the two ways
a process can stop, which produce different behaviour on purpose.'

cluster_smoke 'Ask any node who is up' \
    'curl -sS http://localhost:8003/services/api' \
    'Both instances, answered by a node that did not take either registration.
The entries were written by the agents themselves through the easyraft
client -- this registry has no registration endpoint at all.'

cluster_smoke 'Look at the leases holding them' \
    'curl -sS http://localhost:8001/__leases' \
    'One lease per agent, each holding the key it registered. When a lease
goes, its keys go with it.'

cluster_smoke 'Stop api-1 politely' \
    "kill ${CLUSTER_PIDS[3]}; sleep 2; curl -sS http://localhost:8003/services/api" \
    'Gone immediately. A clean shutdown revokes the lease on the way out, so
the entry is removed at once rather than waiting to be noticed.'

cluster_smoke 'Kill api-2 outright' \
    "kill -9 ${CLUSTER_PIDS[4]}; curl -sS http://localhost:8003/services/api" \
    'Still listed. SIGKILL runs no code, so nothing revoked anything -- this
is the crash case, and the registry cannot yet tell the difference between a
dead process and a slow one.'

cluster_smoke 'Wait for its lease to expire' \
    'sleep 8; curl -sS http://localhost:8003/services/api' \
    'Now empty. Nothing renewed the lease, so it fell due and the leader
proposed its revocation; every replica deleted the key at that entry. That is
the whole point of a lease: no heartbeat table, and no process anywhere had
to notice the death.'

cluster_footer
