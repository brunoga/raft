# Examples

Five fully-worked examples are provided, each targeting a different deployment pattern. Every example ships with a `cluster.sh` script that starts a local 3-node cluster, and a REPL client for interactive exploration.

---

## `idprovider` — single-group, core `raft` package

See [`idprovider/`](idprovider/) for a production-ready distributed monotonic ID allocation service. It demonstrates all major single-group features of the core `raft` package:

- **State machine**: multiple independent domains, each with a `uint64` counter; each allocation atomically reserves a range `[start, start+count)`.
- **Domain management**: `POST /domains/{name}` to create, `DELETE` to remove, `GET /domains` for a linearizable listing of all domains and their counters.
- **Exactly-once allocation**: clients supply `X-Client-ID` and `X-Seq-Num` headers; retrying with the same pair returns the original range.
- **Linearizable reads**: `GET /domains/{name}/current` uses `ReadIndex`.
- **Stale reads**: append `?consistency=stale` to any read endpoint to bypass the leader round-trip.
- **Leader routing**: non-leader nodes return `503` with `X-Raft-Leader`.
- **gRPC transport** + **filestore** + graceful shutdown.

```bash
go build -o idprovider ./idprovider
./idprovider --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 --data-dir /tmp/n1 \
             --peer n2=localhost:7002,localhost:8002 \
             --peer n3=localhost:7003,localhost:8003
```

Each `--peer` is `id=raft_addr[,http_addr]`. The HTTP address is what lets a
follower redirect a write to the leader; leave it out and the follower answers
`503` with an `X-Raft-Leader` header naming the leader's node ID, which a
client cannot dial.

---

## `ratelimiter` — EasyRaft with deterministic mutations

See [`ratelimiter/`](ratelimiter/) for a distributed token-bucket rate limiter built on `easyraft`. It demonstrates:

- **`EasyRaft.New[T]`**: single-collection setup with full REST API.
- **Deterministic mutations**: the current timestamp is encoded into the mutation args before proposing, so the time-based refill is applied identically on every node.
- **`WithJoinAddr`**: nodes join a running cluster one at a time instead of configuring the full peer list upfront.

```bash
go build -o ratelimiter ./ratelimiter

# Node 1 — bootstrap
./ratelimiter -id n1 -raft 127.0.0.1:7001 -http 127.0.0.1:8001 -data /tmp/rl/n1

# Node 2 — joins node 1
./ratelimiter -id n2 -raft 127.0.0.1:7002 -http 127.0.0.1:8002 -data /tmp/rl/n2 \
              -join 127.0.0.1:8001
```

The addresses a joining node is given have to name a host: it hands them to the
cluster as the address to dial it back on, and `:7002` is not one. See
[Addressing](#addressing) below.

---

## `configsvc` — EasyRaft with watch/subscribe (SSE)

See [`configsvc/`](configsvc/) for a distributed configuration service built on `easyraft`. It demonstrates the watch/subscribe pattern:

- **`Collection.OnChange`**: fires on every replica after each committed write, outside the Raft lock. Used here to fan out change events to locally connected SSE clients without polling.
- **`Collection.Upsert`**: atomic create-or-update in a single log entry.
- **SSE watch streams**: `GET /watch/{key}` and `GET /watch` — any node can serve watchers; each fires independently from its own `OnChange` callback.
- **Deterministic versioning**: `ConfigEntry.Version` is set by the HTTP handler before proposing so all replicas apply the same value.
- **`WithJoinAddr`**: same one-at-a-time cluster growth as `ratelimiter`.

```bash
go build -o configsvc ./configsvc

# Node 1 — bootstrap
./configsvc --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 --data-dir /tmp/cfg/n1

# Node 2 — joins node 1; watch all keys
./configsvc --id n2 --raft-addr 127.0.0.1:7002 --http-addr 127.0.0.1:8002 --data-dir /tmp/cfg/n2 \
            --join localhost:8001

curl -N http://localhost:8002/watch   # SSE stream on node 2

# Write on node 1 — event arrives on node 2's watcher
curl -L -X PUT http://localhost:8001/configs/db.host \
     -H 'Content-Type: application/json' -d '{"value":"localhost"}'
```

---

## `ledger` — EasyRaft with atomic multi-collection transactions

See [`ledger/`](ledger/) for a distributed double-entry ledger built on `easyraft`. It demonstrates the `Store.Txn` API — the pattern none of the other examples cover:

- **`Store.Txn`**: commits a debit, a credit, and a transfer record across two collections in a single Raft log entry. Any failure rolls back the entire batch — no partial state.
- **Idempotent transfers**: the transfer record is created first inside the Txn, keyed by `client_id:seq`. A retry hits `ErrKeyExists` before any balance mutations run.
- **`Collection.RegisterMutation`**: the `debit` mutation enforces the "no negative balance" invariant inside `Apply` on every replica.
- **`WithHTTPMux`**: easyraft management routes share the application's mux.

```bash
go build -o ledger ./ledger

# Node 1 — bootstrap
./ledger --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 --data-dir /tmp/lgr/n1

# Node 2 — joins node 1
./ledger --id n2 --raft-addr 127.0.0.1:7002 --http-addr 127.0.0.1:8002 --data-dir /tmp/lgr/n2 \
         --join localhost:8001

# Create accounts and transfer funds
curl -X POST http://localhost:8001/accounts \
     -H 'Content-Type: application/json' -d '{"id":"alice","balance":1000}'
curl -L -X POST http://localhost:8001/transfers \
     -H 'Content-Type: application/json' \
     -d '{"from":"alice","to":"bob","amount":100,"client_id":"cli1","seq":1}'
```

---

## `shardkv` — multi-raft with leader balancing

See [`shardkv/`](shardkv/) for a horizontally-sharded key-value store that demonstrates the canonical multi-raft pattern: N independent Raft groups co-located on each physical node, sharing one gRPC transport and one `Manager` ticker, with a `BalanceController` distributing leaders evenly across machines.

- **Multi-raft wiring**: `Manager`, `cfg.GroupID`, `tr.SetGroupLookup`, `RunTicker`.
- **FNV-hash routing**: keys are deterministically mapped to shards; the HTTP layer resolves the correct group and redirects writes to the shard leader.
- **Heartbeat batching**: enabled automatically by `SetGroupLookup` — O(G×P) heartbeats per tick coalesced into O(P) `BatchHeartbeats` RPCs.
- **Independent fault domains**: stopping or partitioning one shard's nodes does not affect other shards' availability or leadership.
- **Cross-machine leader balancing**: a `BalanceController` on each node uses `HTTPNodeProvider` to query peers' `/raft/status` endpoints and drives leadership transfers to maintain ±1 leader balance across physical nodes.

```bash
go build -o shardkv ./shardkv

# Node 1 — hosts one replica of each of the 4 shards
./shardkv --id p1 --shards 4 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 \
          --data-dir /tmp/sk/p1 \
          --peer p2=localhost:7002,localhost:8002 \
          --peer p3=localhost:7003,localhost:8003

# Node 2 and 3 follow the same pattern
```

---

## Addressing

Every example takes an address to bind and, one way or another, tells the other
nodes where to find it. Those are not always the same string, and mixing them
up is the one configuration mistake that produces a cluster which looks up and
is not.

**A bind address may name no host.** `:7001` and `0.0.0.0:7001` both mean
"every interface on this machine", which is exactly what you want a server to
listen on.

**An advertised address must name a host.** It is handed to another machine,
which then dials it. `:7001` tells that machine nothing, and `0.0.0.0:7001`
tells it to connect to itself.

The examples that grow a cluster with `--join` (`ratelimiter`, `configsvc`,
`ledger`) advertise their `--raft-addr`/`-raft` value, so it has to be
routable: `127.0.0.1:7001` for a local cluster, a hostname or pod IP in a real
one. Passing `:7001` is refused at startup, with a message saying so — it used
to be accepted and then rejected by the node being joined, thirty seconds
later, as a timeout.

The examples that take a static peer list (`idprovider`, `shardkv`) are told
every peer's address up front with `--peer id=raft_addr[,http_addr]`, so their
own `--raft-addr` is only a bind address and `:7001` is fine there. Give each
peer its HTTP address as well, or a follower cannot redirect a write to the
leader -- it knows which node leads, but not where to send the client.

In a deployment where a node cannot know its own reachable address from what it
binds — behind a load balancer, in a container with a published port —
`easyraft.WithAdvertiseRaftAddr` and `easyraft.WithAdvertiseHTTPAddr` set the
two independently.
