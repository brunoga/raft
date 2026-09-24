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
# script then tried to execute a directory.
BINARY="$SCRIPT_DIR/tenants-bin"
CTL="$SCRIPT_DIR/tenantctl-bin"
DATA_ROOT="/tmp/tenants"

# NUM_GROUPS, not GROUPS: bash's GROUPS is a special array holding the
# current user's group IDs, and an assignment to it is silently ignored. The
# script then passed --groups "$GROUPS", which expands to the user's primary
# group ID -- 1000 on a typical Linux box.
NUM_GROUPS=9
PEERS="n1=127.0.0.1:7001,n2=127.0.0.1:7002,n3=127.0.0.1:7003"
BALANCE="n1=127.0.0.1:8001,n2=127.0.0.1:8002,n3=127.0.0.1:8003"
TENANTS="acme,globex,initech,umbrella,hooli"
ENDPOINTS="127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003"

# shellcheck source=../internal/clusterlib.sh
. "$REPO_ROOT/examples/internal/clusterlib.sh"

cluster_parse_args "$@"

echo "==> Building tenants and tenantctl..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/tenants)
(cd "$REPO_ROOT" && go build -o "$CTL" ./examples/tenants/tenantctl)

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

# Ready when every group has a leader somewhere.
leaders_total() {
    local n total=0
    for n in 1 2 3; do
        local body
        body=$(curl -sf "http://127.0.0.1:800$n/__balance/status" 2>/dev/null) || continue
        total=$((total + $(printf '%s' "$body" | count_occurrences '"state":"Leader"')))
    done
    printf '%s' "$total"
}
all_groups_led() {
    [[ "$(leaders_total)" -ge "$NUM_GROUPS" ]]
}

for n in 1 2 3; do
    cluster_start "n$n" "$BINARY" --id "n$n" \
        --raft-addr "127.0.0.1:700$n" --http-addr "127.0.0.1:800$n" \
        --data-dir "$DATA_ROOT/n$n" --groups "$NUM_GROUPS" \
        --peers "$PEERS" --balance-peers "$BALANCE" --balance-every 5s \
        --tenants "$TENANTS"
done

cluster_wait_ready "$NUM_GROUPS groups to elect leaders" 60 all_groups_led || cluster_give_up

echo "==> Leaders per host:"
for n in 1 2 3; do
    body=$(curl -sf "http://127.0.0.1:800$n/__balance/status" 2>/dev/null) || body=""
    count=$(printf '%s' "$body" | count_occurrences '"state":"Leader"')
    printf '  n%s  %s of %s groups\n' "$n" "$count" "$NUM_GROUPS"
done

echo ""
echo "==> Where each tenant lives:"
if command -v jq >/dev/null 2>&1; then
    curl -sf "http://127.0.0.1:8001/tenants" | jq -r '
        .tenants[] | "  \(.tenant)\tgroup \(.group)\tleader \(.leader)"' || true
else
    curl -sf "http://127.0.0.1:8001/tenants" || true
    echo ""
fi

echo ""
echo "Endpoints:"
echo "  n1  http://localhost:8001"
echo "  n2  http://localhost:8002"
echo "  n3  http://localhost:8003"
echo ""
cluster_demo_intro 'A multi-tenant store where every tenant is its own Raft group, many
groups to a node. Tenants are isolated by construction: one tenant'\''s writes
queue behind that tenant'\''s log, not behind everybody'\''s.

Nine groups are running across three nodes, with leader balancing on.'

cluster_smoke 'Which group holds a tenant' \
    "$CTL --tenant acme --groups $NUM_GROUPS where" \
    'No cluster round-trip at all: the mapping is a pure function of the name,
so a client can work out where a tenant lives without asking anyone. That is
also why the cluster and its clients cannot drift apart.'

cluster_smoke 'Write into one tenant' \
    "$CTL --tenant acme --groups $NUM_GROUPS --endpoints $ENDPOINTS put greeting hello && echo written" \
    'The client is pointed at that tenant'\''s Raft group, and from there behaves
exactly as it would against a single-group store -- including finding the
leader of that group and following it when balancing moves it.'

cluster_smoke 'Read it back' \
    "$CTL --tenant acme --groups $NUM_GROUPS --endpoints $ENDPOINTS get greeting" \
    'The value, from whichever node currently leads that group.'

cluster_smoke 'Write the same key in a different tenant' \
    "$CTL --tenant globex --groups $NUM_GROUPS --endpoints $ENDPOINTS put greeting elsewhere && $CTL --tenant globex --groups $NUM_GROUPS --endpoints $ENDPOINTS get greeting" \
    'The same key name, a different value, because it is a different Raft
group. Tenants are isolated by construction rather than by a prefix somebody
has to remember to apply.'

cluster_smoke 'List each tenant separately' \
    "echo acme:; $CTL --tenant acme --groups $NUM_GROUPS --endpoints $ENDPOINTS list; echo globex:; $CTL --tenant globex --groups $NUM_GROUPS --endpoints $ENDPOINTS list" \
    'Neither listing contains the other'\''s data.'

cluster_smoke 'Count leaders per host' \
    'for n in 1 2 3; do printf "n$n: "; curl -sS "http://127.0.0.1:800$n/__balance/status" | grep -o '\''"state":"Leader"'\'' | wc -l; done' \
    'Leadership is spread rather than concentrated. Groups elect
independently, so without the balance controller a host that stayed up while
others restarted would end up leading most of them -- doing all the
replication, all the linearizable reads and all the writes.'

cluster_footer
