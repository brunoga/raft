#!/usr/bin/env bash
# cluster.sh — start a 3-node ledger cluster locally for manual testing.
# Nodes join one at a time via --join (no pre-shared peer list required).
# Press Ctrl-C to stop all nodes and clean up data directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BINARY="$SCRIPT_DIR/ledger"
DATA_ROOT="/tmp/ledger"

# shellcheck source=../internal/clusterlib.sh
. "$REPO_ROOT/examples/internal/clusterlib.sh"

cluster_parse_args "$@"

echo "==> Building ledger..."
(cd "$REPO_ROOT" && go build -o "$BINARY" ./examples/ledger)

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
cluster_demo_intro 'A double-entry ledger. A transfer is a record, a debit and a credit, and
either all three happen or none of them does -- one Raft entry, so there is
no window where money has left one account and not arrived at the other.

Transfers are posted into an accounting period. Closing the books is the
thing a ledger must not have transfers racing with, and the last step shows
what stops it.'

cluster_smoke 'Open an accounting period' \
    'curl -sS -L -X POST http://localhost:8001/period -H '\''Content-Type: application/json'\'' -d '\''{"id":"2026-09"}'\'' -w '\''HTTP %{http_code}\n'\''' \
    'Transfers are posted into whichever period is open. Nothing can be posted
until one is.'

cluster_smoke 'Create two accounts' \
    'curl -sS -L -X POST http://localhost:8001/accounts -H '\''Content-Type: application/json'\'' -d '\''{"id":"alice","balance":1000}'\'' -w '\'' HTTP %{http_code}\n'\''; curl -sS -L -X POST http://localhost:8001/accounts -H '\''Content-Type: application/json'\'' -d '\''{"id":"bob","balance":0}'\'' -w '\'' HTTP %{http_code}\n'\''' \
    'Alice has 1000, Bob has nothing. Both writes are ordinary replicated
creates.'

cluster_smoke 'Move 100 from alice to bob' \
    'curl -sS -L -X POST http://localhost:8001/transfers -H '\''Content-Type: application/json'\'' -d '\''{"from":"alice","to":"bob","amount":100,"client_id":"cli1","seq":1}'\''' \
    'One transaction: create the transfer record, debit, credit. A failure at
any step rolls back the whole batch, so a half-applied transfer is not a
state this ledger can be in.'

cluster_smoke 'Read both balances from a follower' \
    'curl -sS http://localhost:8003/accounts/alice; echo; curl -sS http://localhost:8003/accounts/bob' \
    '900 and 100. Read from a node that did not take the write, because the
entry is applied on every replica before any of them answers a linearizable
read.'

cluster_smoke 'Send the identical request again' \
    'curl -sS -L -X POST http://localhost:8001/transfers -H '\''Content-Type: application/json'\'' -d '\''{"from":"alice","to":"bob","amount":100,"client_id":"cli1","seq":1}'\''' \
    'The same record comes back rather than a second transfer. The transfer ID
is derived from (client_id, seq), so the create inside the batch hits an
existing key and aborts it before any balance changes. That is what makes a
retry after a lost response safe.'

cluster_smoke 'Check the balances did not move' \
    'curl -sS http://localhost:8003/accounts/alice; echo; curl -sS http://localhost:8003/accounts/bob' \
    'Still 900 and 100. The retry was absorbed, not applied.'

cluster_smoke 'Try to overdraw' \
    'curl -sS -o /dev/null -w '\''HTTP %{http_code}\n'\'' -L -X POST http://localhost:8001/transfers -H '\''Content-Type: application/json'\'' -d '\''{"from":"bob","to":"alice","amount":999999,"client_id":"cli1","seq":2}'\''' \
    '422. The no-negative-balance rule lives inside the state machine, so it
is enforced on every replica as the entry applies rather than by whichever
node happened to take the request.'

cluster_smoke 'Close the books, then try to post into them' \
    'curl -sS -L -X POST http://localhost:8001/period/close -o /dev/null -w '\''close HTTP %{http_code}\n'\''; curl -sS -o /dev/null -w '\''transfer HTTP %{http_code}\n'\'' -L -X POST http://localhost:8001/transfers -H '\''Content-Type: application/json'\'' -d '\''{"from":"alice","to":"bob","amount":1,"client_id":"cli1","seq":3}'\''' \
    '409. Every transfer'\''s batch is guarded on the period'\''s revision, so a
transfer that was validated while the books were open still cannot commit
after they closed -- which a check alone could not prevent, because a check
is a read and the close can land after it.'

cluster_footer
