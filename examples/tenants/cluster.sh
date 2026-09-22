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
BINARY="$SCRIPT_DIR/tenants"
CTL="$SCRIPT_DIR/tenantctl"
DATA_ROOT="/tmp/tenants"
GROUPS=9
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
        --data-dir "$DATA_ROOT/n$n" --groups "$GROUPS" \
        --peers "$PEERS" --balance-peers "$BALANCE" --balance-every 5s \
        --tenants "$TENANTS" \
        >"$DATA_ROOT/n$n.log" 2>&1 &
    PIDS+=($!)
done

printf "==> Waiting for every group to elect a leader"
deadline=$((SECONDS + 60))
while [[ $SECONDS -lt $deadline ]]; do
    led=0
    for n in 1 2 3; do
        body=$(curl -sf "http://127.0.0.1:800$n/__balance/status" 2>/dev/null) || continue
        led=$((led + $(printf '%s' "$body" | grep -o '"state":"Leader"' | wc -l)))
    done
    if [[ $led -ge $GROUPS ]]; then
        printf ' done.\n'
        break
    fi
    printf '.'
    sleep 0.5
done
echo ""

echo "==> Leaders per host:"
for n in 1 2 3; do
    body=$(curl -sf "http://127.0.0.1:800$n/__balance/status" 2>/dev/null || echo '[]')
    count=$(printf '%s' "$body" | grep -o '"state":"Leader"' | wc -l)
    printf '  n%s  %s of %s groups\n' "$n" "$count" "$GROUPS"
done

echo ""
echo "==> Where each tenant lives:"
curl -sf "http://127.0.0.1:8001/tenants" || true
echo ""

echo ""
echo "Try it:"
echo "  $CTL --tenant acme --groups $GROUPS --endpoints 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 put greeting hello"
echo "  $CTL --tenant acme --groups $GROUPS --endpoints 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 get greeting"
echo "  $CTL --tenant acme --groups $GROUPS where"
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
