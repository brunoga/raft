#!/usr/bin/env bash
# cluster.sh — start a 3-node tenants cluster with 9 Raft groups and leader
# balancing, then show where the leaders ended up.
#
# Every node hosts every group, so each tenant's data is replicated three
# times and leadership for the nine groups is spread across the three hosts.
#
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# -bin suffixes because this example has directories named `tenants` and
# `tenantctl` beside this script. `go build -o <existing dir>` does not fail:
# it writes the binary *inside* that directory and reports success, so the
# script then tried to execute a directory and every node died before it
# started. Nothing said so -- the failure surfaced only as an election that
# never happened.
BINARY="$SCRIPT_DIR/tenants-bin"
CTL="$SCRIPT_DIR/tenantctl-bin"
DATA_ROOT="/tmp/tenants"
# NUM_GROUPS, not GROUPS: bash's GROUPS is a special array holding the
# current user's group IDs, and an assignment to it is silently ignored. The
# script then passed --groups "$GROUPS", which expands to the user's primary
# group ID -- 1000 on a typical Linux box. Every node started a thousand Raft
# groups and the wait loop sat there needing a thousand leaders, which is
# what "stuck waiting for every group to elect a leader" looked like.
NUM_GROUPS=9
PEERS="n1=127.0.0.1:7001,n2=127.0.0.1:7002,n3=127.0.0.1:7003"
BALANCE="n1=127.0.0.1:8001,n2=127.0.0.1:8002,n3=127.0.0.1:8003"
TENANTS="acme,globex,initech,umbrella,hooli"

echo "==> Building tenants and tenantctl..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/tenants)
(cd "$REPO_ROOT" && go build -o "$CTL" ./examples/tenants/tenantctl)

echo "==> Cleaning data dirs under $DATA_ROOT..."
rm -rf "$DATA_ROOT"
mkdir -p "$DATA_ROOT"/{n1,n2,n3}

PIDS=()
cleanup() {
    echo ""
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

for n in 1 2 3; do
    echo "==> Starting n$n..."
    "$BINARY" --id "n$n" \
        --raft-addr "127.0.0.1:700$n" --http-addr "127.0.0.1:800$n" \
        --data-dir "$DATA_ROOT/n$n" --groups "$NUM_GROUPS" \
        --peers "$PEERS" --balance-peers "$BALANCE" --balance-every 5s \
        --tenants "$TENANTS" \
        >"$DATA_ROOT/n$n.log" 2>&1 &
    PIDS+=($!)
done

printf "==> Waiting for every group to elect a leader"
deadline=$((SECONDS + 60))
elected=0
while [[ $SECONDS -lt $deadline ]]; do
    # A node that died takes its groups with it, and waiting out the timeout
    # to discover that wastes a minute and says nothing. Check first.
    for i in "${!PIDS[@]}"; do
        if ! kill -0 "${PIDS[$i]}" 2>/dev/null; then
            printf '\n'
            echo "==> Node n$((i + 1)) exited. Its log says:"
            sed 's/^/    /' "$DATA_ROOT/n$((i + 1)).log" | tail -20
            exit 1
        fi
    done

    led=0
    for n in 1 2 3; do
        body=$(curl -sf "http://127.0.0.1:800$n/__balance/status" 2>/dev/null) || continue
        led=$((led + $(printf '%s' "$body" | grep -o '"state":"Leader"' | wc -l)))
    done
    if [[ $led -ge $NUM_GROUPS ]]; then
        printf ' done.\n'
        elected=1
        break
    fi
    printf '.'
    sleep 0.5
done
if [[ $elected -eq 0 ]]; then
    printf '\n'
    echo "==> Only $led of $NUM_GROUPS groups have a leader after 60s. Check $DATA_ROOT/n*.log."
    exit 1
fi
echo ""

echo "==> Leaders per host:"
for n in 1 2 3; do
    body=$(curl -sf "http://127.0.0.1:800$n/__balance/status" 2>/dev/null || echo '[]')
    count=$(printf '%s' "$body" | grep -o '"state":"Leader"' | wc -l)
    printf '  n%s  %s of %s groups\n' "$n" "$count" "$NUM_GROUPS"
done

echo ""
echo "==> Where each tenant lives:"
curl -sf "http://127.0.0.1:8001/tenants" || true
echo ""

echo ""
echo "Try it:"
echo "  $CTL --tenant acme --groups $NUM_GROUPS --endpoints 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 put greeting hello"
echo "  $CTL --tenant acme --groups $NUM_GROUPS --endpoints 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 get greeting"
echo "  $CTL --tenant acme --groups $NUM_GROUPS where"
echo ""
echo "Watch balancing do its job. Kill a node and restart it; its groups"
echo "re-elect elsewhere, and the controllers hand some back:"
echo "  kill ${PIDS[0]}"
echo "  curl -s 127.0.0.1:8002/__balance/status | grep -o '\"state\":\"Leader\"' | wc -l"
echo ""
echo "Logs: $DATA_ROOT/n{1,2,3}.log"
echo "Press Ctrl-C to stop."
echo ""

wait
