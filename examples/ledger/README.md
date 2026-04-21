# ledger — distributed double-entry ledger

A strongly-consistent, replicated ledger built on EasyRaft. Accounts and transfer records live in two separate collections that share a single Raft group. Every transfer is applied atomically: the debit, the credit, and the transfer record all commit in a single log entry or none do.

```
go build -o ledger ./examples/ledger
```

---

## What this example demonstrates

This example focuses on **`Store.Txn`** — the multi-collection atomic batch API that none of the other examples use. It shows a realistic use case where partial failure would leave data in an inconsistent state.

Additional patterns:

- **`Store.Txn`** — atomic multi-step operation across two collections (`accounts` and `transfers`). Either all three ops (Create record + Mutate debit + Mutate credit) commit or none do.
- **Idempotent writes via `ErrKeyExists`** — the transfer record is created first inside the Txn. The transfer ID is `client_id:seq`, so a retry with the same (client_id, seq) pair hits `ErrKeyExists` on the Create step before any balance mutations run — no rollback needed, and no double-debit.
- **`Collection.RegisterMutation`** — the `debit` mutation enforces the "no negative balance" invariant inside the state machine, running on every replica during Apply and log replay.
- **Deterministic mutations** — the debit amount is encoded in the args before proposing; replicas never read external state inside a mutation.
- **`WithHTTPMux`** — easyraft management routes share the application's mux; one HTTP server handles everything.

---

## HTTP API

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/accounts` | Create an account `{"id":"alice","balance":1000}` |
| `GET` | `/accounts/{id}` | Read account — linearizable |
| `GET` | `/accounts` | List all accounts — linearizable |
| `POST` | `/transfers` | Transfer funds (see below) |
| `GET` | `/transfers/{id}` | Get a transfer record |
| `GET` | `/transfers` | List all transfer records |

### Transfer request body

```json
{
  "from":      "alice",
  "to":        "bob",
  "amount":    100,
  "client_id": "cli1",
  "seq":       1
}
```

The `(client_id, seq)` pair is the idempotency key. Retrying with the same pair returns the previously committed transfer record without re-executing the transfer.

---

## Running a 3-node cluster

The quickest way to start a local cluster is with the provided script:

```bash
./cluster.sh
```

It builds the binary, wipes any previous data directories, starts all three nodes, waits for a leader to be elected, and prints a member table confirming the cluster is healthy. Press Ctrl-C to stop all nodes and clean up.

Alternatively, start the nodes manually in separate terminals:

**Terminal 1 — bootstrap node**

```bash
./ledger --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/lgr/n1
```

**Terminal 2 — join**

```bash
./ledger --id n2 --raft-addr :7002 --http-addr :8002 --data-dir /tmp/lgr/n2 \
         --join localhost:8001
```

**Terminal 3 — join**

```bash
./ledger --id n3 --raft-addr :7003 --http-addr :8003 --data-dir /tmp/lgr/n3 \
         --join localhost:8001
```

---

## Example session

**Create accounts**

```bash
curl -X POST http://localhost:8001/accounts \
     -H 'Content-Type: application/json' -d '{"id":"alice","balance":1000}'
# {"id":"alice","balance":1000}

curl -X POST http://localhost:8001/accounts \
     -H 'Content-Type: application/json' -d '{"id":"bob","balance":0}'
# {"id":"bob","balance":0}
```

**Transfer 100 from alice to bob**

```bash
curl -L -X POST http://localhost:8001/transfers \
     -H 'Content-Type: application/json' \
     -d '{"from":"alice","to":"bob","amount":100,"client_id":"cli1","seq":1}'
# {"id":"cli1:1","from":"alice","to":"bob","amount":100,"timestamp":1712345678901234567}
```

**Read balances (from any node)**

```bash
curl http://localhost:8003/accounts/alice
# {"id":"alice","balance":900}

curl http://localhost:8003/accounts/bob
# {"id":"bob","balance":100}
```

**Retry the same transfer (idempotent)**

```bash
curl -L -X POST http://localhost:8001/transfers \
     -H 'Content-Type: application/json' \
     -d '{"from":"alice","to":"bob","amount":100,"client_id":"cli1","seq":1}'
# {"id":"cli1:1","from":"alice","to":"bob","amount":100,"timestamp":1712345678901234567}
# (same record returned, alice still has 900)
```

**Insufficient funds**

```bash
curl -L -X POST http://localhost:8001/transfers \
     -H 'Content-Type: application/json' \
     -d '{"from":"alice","to":"bob","amount":99999,"client_id":"cli1","seq":2}'
# 422 Unprocessable Entity: insufficient funds: balance 900 < 99999
# (alice still has 900 — the whole Txn rolled back)
```

---

## Go Client Library

A high-level Go client is available in the [`client`](./client) package. It handles node discovery, leader redirection, and provides a type-safe API for accounts and transfers.

```go
import "github.com/brunoga/raft/examples/ledger/client"

c := client.New([]string{"http://localhost:8001", "http://localhost:8002"})

// Create accounts
alice, err := c.CreateAccount(ctx, "alice", 1000)
bob, err := c.CreateAccount(ctx, "bob", 0)

// Transfer funds (idempotent — retry with same clientID+seq is safe)
tr, err := c.Transfer(ctx, "alice", "bob", 100, "cli1", 1)
if errors.Is(err, client.ErrInsufficientFunds) {
    log.Println("not enough balance")
}

// Read current balances (linearizable)
alice, err = c.GetAccount(ctx, "alice")
fmt.Printf("alice balance: %d\n", alice.Balance)
```

## HTTP status codes

| Code | Meaning |
|------|---------|
| `200 OK` | Read succeeded |
| `201 Created` | Account or transfer record created |
| `400 Bad Request` | Malformed JSON or missing required fields |
| `404 Not Found` | Account does not exist |
| `409 Conflict` | Account with that ID already exists |
| `422 Unprocessable Entity` | Insufficient funds |
| `503 Service Unavailable` | This node is not the leader |
| `500 Internal Server Error` | Storage failure or unexpected error |

Clients should retry `503` — the node will redirect to the leader after a brief election. `409` on `POST /accounts` indicates a duplicate key; the client returns `ErrConflict`. `422` on `POST /transfers` indicates the source account has too little balance; the client returns `ErrInsufficientFunds`.

---

## Architecture notes

### How atomicity works

`Store.Txn` encodes all operations as a single Raft log entry. On every replica, `Apply` executes them in order inside the state machine lock. If any operation returns an error, the state machine rolls back all mutations from that batch by restoring collection snapshots taken before the batch began — no partial state is ever visible.

```
POST /transfers
     │
     ▼
Store.Txn (single log entry)
  ├── Create "transfers" / "cli1:1"   ← idempotency guard
  ├── Mutate "accounts" / "alice" / "debit"
  └── Mutate "accounts" / "bob"  / "credit"
     │
     ▼  (committed on all replicas atomically)
alice.balance -= amount
bob.balance   += amount
transfer record created
```

### Idempotency without MutateOnce

The transfer record is created first in the batch with the key `client_id:seq`. On a retry:

1. The Txn is proposed to Raft.
2. Every replica tries to `Create` the transfer key — it already exists.
3. `ErrKeyExists` aborts the batch before any balance mutations run.
4. The caller receives `ErrKeyExists` and the server returns the existing record.

No double-debit occurs even if the network drops the response after the first commit.

### Why the debit mutation enforces the invariant

The `debit` mutation is the only place where "insufficient funds" is checked. Because it runs on every replica inside `Apply`, the invariant is enforced even during log replay after a crash. If an old log entry for a transfer is replayed after a snapshot restore, the mutation re-checks the balance rather than blindly subtracting.
