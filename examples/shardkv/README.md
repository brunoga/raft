# shardkv — sharded key-value store (multi-raft + leader balancing example)

`shardkv` is a horizontally-sharded key-value store built on the `raft` package.
It is the canonical **multi-raft** example: N independent Raft groups (shards)
run on each physical node, all sharing one gRPC transport and driven by one
central `Manager` ticker. A `BalanceController` runs on every node and
automatically redistributes shard leaders across physical nodes using the
`HTTPNodeProvider` to build a cross-machine view.

## What multi-raft means here

In a multi-raft deployment every physical server hosts **one replica from every
shard**. The `Manager` on each server groups all those co-located nodes and
drives them from a single `RunTicker` goroutine.

```
Physical Server 1        Physical Server 2        Physical Server 3
(one shardkv process)    (one shardkv process)    (one shardkv process)
┌───────────────────┐    ┌───────────────────┐    ┌───────────────────┐
│ GRPCTransport     │    │ GRPCTransport     │    │ GRPCTransport     │
│ SetGroupLookup ───┤    │ SetGroupLookup ───┤    │ SetGroupLookup ───┤
│ Manager+RunTicker │    │ Manager+RunTicker │    │ Manager+RunTicker │
│                   │    │                   │    │                   │
│ g1-p1  Shard 1 ★ │◄──►│ g1-p2  Shard 1   │◄──►│ g1-p3  Shard 1   │
│ g2-p1  Shard 2   │◄──►│ g2-p2  Shard 2 ★ │◄──►│ g2-p3  Shard 2   │
│ g3-p1  Shard 3 ★ │◄──►│ g3-p2  Shard 3   │◄──►│ g3-p3  Shard 3   │
│ g4-p1  Shard 4   │◄──►│ g4-p2  Shard 4 ★ │◄──►│ g4-p3  Shard 4   │
└───────────────────┘    └───────────────────┘    └───────────────────┘
```

Each row is one independent Raft consensus group. The `★` marks which server
currently holds the shard leader — each shard elects its own leader
independently.

The key multi-raft wiring in this example:

```go
// One gRPC port for all shards on this physical node.
tr, _ := grpctransport.Listen(":7001")

// Route inbound RPCs to the correct shard node by GroupID.
// Also enables heartbeat batching: O(G×P) → O(P) RPCs per tick.
tr.SetGroupLookup(mgr.Lookup)

// Each shard node stamps its GroupID on every outbound RPC.
cfg.GroupID = gid     // non-zero uint64, unique per shard
cfg.Transport = tr    // same transport for all shards

// One ticker drives all shard nodes on this machine.
go mgr.RunTicker(ctx, 10*time.Millisecond)
```

The leader balancing wiring:

```go
// Local node contributes its Manager directly; peers are reached via HTTP.
providers := map[raft.NodeID]raft.NodeProvider{
    raft.NodeID("p1"): mgr,  // in-process, no HTTP hop
    raft.NodeID("p2"): raft.NewHTTPNodeProvider("http://peer2:8002/raft", nil),
    raft.NodeID("p3"): raft.NewHTTPNodeProvider("http://peer3:8003/raft", nil),
}

// LeastLeadersBalancer moves leaders from the busiest node to the least-busy
// until all physical nodes differ by at most 1 leader.
ctrl := raft.NewBalanceController(providers, raft.LeastLeadersBalancer{}, 30*time.Second)
go ctrl.Run(ctx)
```

The `/raft/status` and `/raft/transfer` HTTP endpoints are mounted automatically
from `Manager.Handler()` and are the only cross-machine surface the balance
controller needs.

### What multi-raft does and does not provide

Multi-raft gives each shard **protocol-level isolation**: independent Raft logs,
independent leader elections, and independent apply pipelines. A slow state
machine in shard 2 cannot block proposals in shard 3.

It does **not** provide hardware-resource isolation. All shards on the same
physical server share CPU, disk I/O, and network bandwidth. A write-heavy shard
can delay heartbeats or slow proposals in its neighbours. Production systems
(CockroachDB, TiKV) address this with per-shard I/O throttling, hot-shard
detection, and partition migration — layers built above the Raft library.

If strict resource isolation between shards is required, run each shard replica
as a separate container (with cgroup CPU/memory/IO limits) or on separate
physical machines, and use plain `raft.Node` without `Manager`.

## Three-node quick-start

```bash
# Build
go build -o shardkv ./examples/shardkv

# Node 1  (hosts one replica of each shard)
./shardkv --id p1 --shards 4 --raft-addr :7001 --http-addr :8001 \
          --data-dir /tmp/sk/p1 \
          --peer p2=localhost:7002,localhost:8002 \
          --peer p3=localhost:7003,localhost:8003

# Node 2  (separate terminal)
./shardkv --id p2 --shards 4 --raft-addr :7002 --http-addr :8002 \
          --data-dir /tmp/sk/p2 \
          --peer p1=localhost:7001,localhost:8001 \
          --peer p3=localhost:7003,localhost:8003

# Node 3  (separate terminal)
./shardkv --id p3 --shards 4 --raft-addr :7003 --http-addr :8003 \
          --data-dir /tmp/sk/p3 \
          --peer p1=localhost:7001,localhost:8001 \
          --peer p2=localhost:7002,localhost:8002
```

After shard leaders are elected (~300 ms) any node's HTTP endpoint can be used.
All nodes accept reads and writes; writes to a follower for a given key's shard
are automatically redirected (307) to the shard leader.

```bash
# Write a key (the client follows the 307 redirect to the leader automatically)
curl -s -X PUT http://localhost:8001/keys/order-123 \
     -d 'pending'
# 204 No Content

# Read it back — linearizable, works from any node
curl -s http://localhost:8002/keys/order-123
# pending

# Update it
curl -s -X PUT http://localhost:8001/keys/order-123 \
     -d 'shipped'

# Delete it
curl -s -X DELETE http://localhost:8001/keys/order-123
# 204 No Content

# Confirm deletion
curl -s http://localhost:8001/keys/order-123
# 404 Not Found

# Inspect shard states on node 1
curl -s http://localhost:8001/shards | jq
# [
#   {"group_id":1,"state":"Leader","term":1,"last_applied":3},
#   {"group_id":2,"state":"Follower","term":1,"last_applied":3},
#   {"group_id":3,"state":"Leader","term":1,"last_applied":2},
#   {"group_id":4,"state":"Follower","term":1,"last_applied":2}
# ]
```

## HTTP API

### `GET /raft/status`

Returns a JSON array of `GroupStatus` objects — one per shard hosted on this
node. Used by `HTTPNodeProvider` to build the cross-machine view for the
`BalanceController`.

```bash
curl -s http://localhost:8001/raft/status | jq
# [
#   {"group_id":1,"node_id":"g1-p1","state":"Leader","term":2,"last_applied":15},
#   {"group_id":2,"node_id":"g2-p1","state":"Follower","term":1,"last_applied":15},
#   ...
# ]
```

### `POST /raft/transfer`

Transfers leadership of a shard to a specific node. Called by the
`BalanceController` when it decides to move a leader.

```bash
curl -s -X POST http://localhost:8001/raft/transfer \
     -H 'Content-Type: application/json' \
     -d '{"group_id": 1, "to": "g1-p2"}'
# 204 No Content
```

Returns `204 No Content` on success, `404 Not Found` when the group is not
registered on this node, and `400 Bad Request` for malformed JSON.

### `PUT /keys/{key}`

Sets `key` to the request body (up to 1 MiB, treated as text).

```bash
curl -s -X PUT http://localhost:8001/keys/user-42 \
     -d '{"name":"alice"}'
# 204 No Content
```

Returns `307 Temporary Redirect` when this node is a follower for the key's
shard; the `Location` header points at the shard leader's HTTP address. Standard
HTTP clients follow redirects automatically.

### `GET /keys/{key}`

Returns the current value for `key`. Uses `ReadIndex` for linearizability —
reads reflect all writes that completed before this request, regardless of which
physical node serves it.

```bash
curl -s http://localhost:8002/keys/user-42
# {"name":"alice"}
```

Returns `404 Not Found` when the key is absent.

### `DELETE /keys/{key}`

Removes `key`. Returns `204 No Content` on success, `404 Not Found` when the
key is absent. Redirects to the shard leader when this node is a follower.

```bash
curl -s -X DELETE http://localhost:8001/keys/user-42
# 204 No Content
```

### `GET /shards`

Returns a JSON array with the live status of every shard on this physical node.

```bash
curl -s http://localhost:8001/shards | jq
# [
#   {"group_id":1,"state":"Leader","term":2,"last_applied":15},
#   {"group_id":2,"state":"Follower","term":1,"last_applied":15},
#   ...
# ]
```

| Field | Description |
|-------|-------------|
| `group_id` | Shard number (1 … `--shards`) |
| `state` | `Leader`, `Follower`, or `Candidate` |
| `term` | Current Raft term for this shard |
| `last_applied` | Highest log index applied to the state machine |

### `GET /status`

Returns this physical node's static configuration.

```bash
curl -s http://localhost:8001/status | jq
# {
#   "id": "p1",
#   "num_shards": 4,
#   "shard_ids": ["g1-p1","g2-p1","g3-p1","g4-p1"]
# }
```

## HTTP status codes

| Code | Meaning |
|------|---------|
| `204 No Content` | Write succeeded |
| `200 OK` | Read succeeded |
| `307 Temporary Redirect` | This node is not the shard leader; `Location` points at the leader |
| `404 Not Found` | Key does not exist (GET or DELETE) |
| `503 Service Unavailable` | Shard leader unknown (e.g. during an election) |
| `500 Internal Server Error` | Storage failure or unexpected error |

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--id` | required | This physical node's unique ID |
| `--shards` | `4` | Number of Raft shard groups — must be identical on every node |
| `--raft-addr` | `:7001` | gRPC listen address shared by all shards on this node |
| `--http-addr` | `:8001` | HTTP listen address for client requests |
| `--data-dir` | required | Root directory for persistent shard state; each shard writes to `<data-dir>/shards/<id>/` |
| `--balance-interval` | `30s` | How often the `BalanceController` checks and adjusts leader distribution; `0` disables balancing |
| `--peer physID=raftAddr[,httpAddr]` | (repeatable) | Peer physical node: ID, its Raft gRPC address, and optionally its HTTP address for write redirects and leader-balance queries |

All nodes must use the same `--shards` value and list the same peers. Nodes may
be started in any order.

## Key routing

Keys are mapped to shards by FNV-32a hash:

```
shardID = fnv32a(key) % numShards + 1   (1-indexed)
```

The routing is stateless and consistent: the same key always lands on the same
shard. There is no rebalancing or range migration in this example — those are
left as application-layer concerns.

## Operational notes

- **Leader balancing**: the `BalanceController` runs on every node and calls
  `GET /raft/status` on all peers every `--balance-interval` to build a
  cross-machine view. It then uses `LeastLeadersBalancer` to move leaders from
  the most-loaded node to the least-loaded until all nodes differ by at most 1.
  At most one transfer per shard is in-flight at a time; failed transfers are
  retried on the next interval. Set `--balance-interval=0` to disable.
- **Shard failure isolation**: if one shard's nodes lose quorum (e.g. two of the
  three replicas are unreachable), that shard stops accepting writes but the
  other three shards continue normally.
- **Data directory layout**: each shard writes to its own `filestore` under
  `<data-dir>/shards/<id>/`. Never share a directory between two shards.
- **Changing shard count**: `--shards` must be the same on every node and cannot
  be changed after the cluster is started. Changing it would reassign keys to
  different shards, losing existing data.
- **Hot shards**: because all shards share one process, a write-heavy shard can
  affect neighbours. See the multi-raft section above for mitigation strategies.
