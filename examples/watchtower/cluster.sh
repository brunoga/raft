#!/usr/bin/env bash
# cluster.sh — start a 3-node watchtower cluster locally for manual testing.
# Uses static --peer flags so all addresses are known upfront.
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

# Anything passed to this script is passed on to every node, so that
#   ./cluster.sh --apply-delay 2ms
# starts a cluster whose state machine is slow on purpose.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/watchtower"
DATA_ROOT="/tmp/watchtower"

# shellcheck source=../internal/clusterlib.sh
. "$REPO_ROOT/examples/internal/clusterlib.sh"

cluster_parse_args --passthrough "$@"
# Anything this script did not recognise is a flag for the nodes themselves,
# such as --apply-delay.
EXTRA=("${CLUSTER_ARGS[@]+"${CLUSTER_ARGS[@]}"}")

echo "==> Building watchtower..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/watchtower)

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
# Ready when all three nodes answer /health and one of them leads.
cluster_ready() {
    local port up=0 elected=1
    for port in 8001 8002 8003; do
        local body
        body=$(curl -sf "http://localhost:$port/health" 2>/dev/null) || continue
        up=$((up + 1))
        printf '%s' "$body" | grep -q '"state":"Leader"' && elected=0
    done
    [[ $up -ge 3 && $elected -eq 0 ]]
}

show_status() {
    printf '==> Node status:\n'
    local port body
    for port in 8001 8002 8003; do
        body=$(curl -sf "http://localhost:$port/health" 2>/dev/null) || { printf '  :%-4s  unreachable\n' "$port"; continue; }
        if command -v jq >/dev/null 2>&1; then
            printf '%s' "$body" | jq -r '"  \(.node)  \(.state)  term=\(.term)  unreachable=\(.unreachable_peers // [] | length)  saturation=\(.apply_saturation)"'
        else
            printf '  :%s  %s\n' "$port" "$body"
        fi
    done
    printf '\n'
}

cluster_start "n1" "$BINARY" --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 --data-dir "$DATA_ROOT/n1" "${EXTRA[@]+"${EXTRA[@]}"}" \
    --peer n2=127.0.0.1:7002,127.0.0.1:8002 --peer n3=127.0.0.1:7003,127.0.0.1:8003

cluster_start "n2" "$BINARY" --id n2 --raft-addr 127.0.0.1:7002 --http-addr 127.0.0.1:8002 --data-dir "$DATA_ROOT/n2" "${EXTRA[@]+"${EXTRA[@]}"}" \
    --peer n1=127.0.0.1:7001,127.0.0.1:8001 --peer n3=127.0.0.1:7003,127.0.0.1:8003

cluster_start "n3" "$BINARY" --id n3 --raft-addr 127.0.0.1:7003 --http-addr 127.0.0.1:8003 --data-dir "$DATA_ROOT/n3" "${EXTRA[@]+"${EXTRA[@]}"}" \
    --peer n1=127.0.0.1:7001,127.0.0.1:8001 --peer n2=127.0.0.1:7002,127.0.0.1:8002

cluster_wait_ready "the cluster to elect a leader" 30 cluster_ready || cluster_give_up
show_status

echo "Endpoints:"
echo "  n1  http://127.0.0.1:8001"
echo "  n2  http://127.0.0.1:8002"
echo "  n3  http://127.0.0.1:8003"
echo ""
cluster_demo_intro 'What the library can tell you about itself. This example exposes the
engine'\''s own observations: what each node thinks of its peers, and the
metrics that say whether the cluster is healthy or merely quiet.

The middle steps freeze a node on purpose so you can watch its peers notice.'

cluster_smoke 'Ask a node what it thinks of its peers' \
    'curl -sS http://localhost:8001/observed' \
    'Per-peer state as the leader sees it: how far behind each one is, and
when it was last heard from.'

cluster_smoke 'Freeze n3 and wait' \
    'pkill -STOP -f '\''watchtower --id n3'\''; sleep 4; echo '\''n3 frozen for 4s'\''' \
    'SIGSTOP, not SIGKILL. The process still exists and still holds its TCP
connections; it simply stops answering, which is the failure that is hardest
to tell from a slow network.'

cluster_smoke 'Look at the peers again' \
    'curl -sS http://localhost:8001/observed' \
    'n3 is now visibly behind. Nothing was reported as an error -- a frozen
follower is not a failure, it is a follower that has stopped keeping up, and
the distinction is what these numbers are for.'

cluster_smoke 'Unfreeze it and watch it catch up' \
    'pkill -CONT -f '\''watchtower --id n3'\''; sleep 3; curl -sS http://localhost:8001/observed' \
    'It returns on its own. The leader keeps sending, the follower catches up,
and no operator action was needed.'

cluster_smoke 'Check apply saturation' \
    'curl -sS http://localhost:8001/metrics | grep raft_apply_saturation' \
    'How much of the time the apply loop is busy. Near one means the state
machine, not the network or the disk, is what bounds this cluster.'

cluster_footer
