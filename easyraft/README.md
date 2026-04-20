# EasyRaft

EasyRaft is a high-level abstraction layer over the `brunoga/raft` package. It lets you build strongly-consistent, replicated services by describing **what** data to replicate — not how. Transport, storage, state machine, snapshotting, leader routing, and peer discovery are all handled automatically.

```
go get github.com/brunoga/raft/easyraft
```

---

## Two entry points

| | `easyraft.New[T]` | `easyraft.NewStore` |
|---|---|---|
| State | One typed collection | Multiple typed collections |
| Typical use | Single-domain service | Multi-entity service |
| Example | `EasyRaft[Counter]` | `AddCollection[User] + AddCollection[Session]` |

Both share the same options, the same `Collection[T]` API, and the same underlying `Store`.

---

## Quick start — single collection

```go
type Counter struct {
    Value int64 `json:"value"`
}

er, err := easyraft.New[Counter](
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithHTTPAddr(":8001"),
    easyraft.WithDataDir("/data/n1"),
    easyraft.WithPeers(map[raft.NodeID]string{
        "n2": "host2:7001",
        "n3": "host3:7001",
    }),
)
er.Start()
defer er.Stop()

ctx := context.Background()

// Wait until the cluster has elected a leader and this node is caught up.
if err := er.Ready(ctx); err != nil {
    log.Fatal(err)
}

// CRUD
er.Create(ctx, "alice", Counter{Value: 0})

c, err := er.Read(ctx, "alice")     // linearizable
c, err  = er.ReadStale("alice")     // local, no round-trip

er.Update(ctx, "alice", Counter{Value: 99})
er.Delete(ctx, "alice")

all, err := er.List(ctx)            // linearizable map[string]Counter
all, err  = er.ListStale()          // local
```

---

## Quick start — multiple collections

```go
store, err := easyraft.NewStore(
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithDataDir("/data/n1"),
    easyraft.WithPeers(peers),
)

users    := easyraft.AddCollection[User](store, "users")
sessions := easyraft.AddCollection[Session](store, "sessions")

store.Start()
defer store.Stop()

ctx := context.Background()
users.Create(ctx, "alice", User{Name: "Alice"})
sessions.Create(ctx, "sess-1", Session{UserID: "alice"})
```

Collection names starting with `__` are reserved for internal use.

---

## Mutations — atomic read-modify-write

A `Mutation` is the correct way to update a value based on its current state.
A `Read` followed by an `Update` is **not atomic**: another node can interleave between the two calls.
A mutation encodes the entire read-modify-write as a single log entry so it commits as one unit.

```go
er.RegisterMutation("increment", func(c *Counter, args []byte) (*Counter, []byte, error) {
    var delta int64 = 1
    if len(args) > 0 {
        json.Unmarshal(args, &delta)
    }
    c.Value += delta
    return c, nil, nil
})

delta, _ := json.Marshal(int64(5))
result, err := er.Mutate(ctx, "alice", "increment", delta)
```

### Determinism requirement

**Mutation functions run inside `Apply`, which executes on every node during log replay. They must be purely deterministic.**

- Do not call `time.Now()`, `rand`, or any external I/O inside a mutation.
- Do not read process state or environment variables.

If your mutation needs the current time (e.g. for a rate limiter refill), encode the timestamp in the `args` before proposing — all nodes will then apply the same value:

```go
type TakeArgs struct {
    Requested int64 `json:"requested"`
    Now       int64 `json:"now"` // Unix timestamp, supplied by the caller
}

quotas.RegisterMutation("take", func(q *Quota, args []byte) (*Quota, []byte, error) {
    var a TakeArgs
    json.Unmarshal(args, &a)

    // a.Now is the same on every node — deterministic
    if q.LastRefill > 0 && a.Now > q.LastRefill {
        q.Tokens += (a.Now - q.LastRefill) * q.Rate
        q.LastRefill = a.Now
    }
    // ...
    return q, nil, nil
})

// At the call site, encode the timestamp before proposing:
args, _ := json.Marshal(TakeArgs{Requested: 1, Now: time.Now().Unix()})
quotas.Mutate(ctx, "premium-user", "take", args)
```

---

## Cross-collection transactions

`Store.Txn` batches operations across multiple collections into a single log entry, making them atomic:

```go
results, err := store.Txn(ctx, func(tx *easyraft.Txn) error {
    tx.Create("users", "alice", User{Name: "Alice"})
    tx.Create("scores", "alice", 100)
    return nil
})
```

If any operation inside the transaction fails during apply, the whole batch is rolled back.

---

## Exactly-once semantics

Use the `*Once` variants to make proposals idempotent across retries. If the response is lost due to a network timeout, retrying with the same `(clientID, seqNum)` pair returns the cached result without re-applying the command:

```go
// seqNum must increase monotonically per clientID.
err := users.CreateOnce(ctx, "client-42", seqNum, "alice", User{Name: "Alice"})

_, err = counts.MutateOnce(ctx, "client-42", seqNum, "k1", "inc", nil)
```

Available for all write operations: `CreateOnce`, `UpdateOnce`, `DeleteOnce`, `MutateOnce`.

---

## Multi-Raft — sharding across groups

`easyraft.Manager` runs multiple independent Raft groups (shards) on a single physical node, sharing one gRPC transport and one HTTP server:

```go
mgr, err := easyraft.NewManager(
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithHTTPAddr(":8001"),
    easyraft.WithPeers(peers),
)

// Each store is an independent Raft group.
s1, _ := mgr.AddStore(1, easyraft.WithDataDir("/data/shard1"))
users  := easyraft.AddCollection[User](s1, "users")

s2, _ := mgr.AddStore(2, easyraft.WithDataDir("/data/shard2"))
orders := easyraft.AddCollection[Order](s2, "orders")

mgr.Start()
defer mgr.Stop()
```

`AddStore` accepts the same options as `NewStore`. Options set on the `Manager` (e.g. `WithID`, `WithPeers`) are inherited by stores; store-level options override them.

---

## Discovery

When `WithDiscovery` is configured, EasyRaft polls the discovery source periodically and:

1. Registers newly found peers with the gRPC transport (`AddPeer`).
2. Calls `AddServer` to add them to the Raft cluster membership.

Only the current leader can commit membership changes; `AddServer` calls on followers fail silently and are retried on the next discovery interval.

```go
import "github.com/brunoga/raft/discovery/udpbroadcast"

d, _ := udpbroadcast.New(&udpbroadcast.Config{
    NodeID: "n1",
    Addr:   ":9001",
})

er, _ := easyraft.New[Counter](
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithDiscovery(d, 5*time.Second),
)
```

Static peers (`WithPeers`) and discovery can be combined — peers added via `WithPeers` are pre-populated in the known-member set and will not trigger redundant `AddServer` calls.

---

## Startup synchronization

`Ready` blocks until the node has a known leader and has applied at least one entry from the current term. Call it after `Start` to avoid issuing writes before the cluster is operational:

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()

if err := store.Ready(ctx); err != nil {
    log.Fatal("cluster not ready:", err)
}
```

---

## HTTP API

When `WithHTTPAddr` is set, EasyRaft mounts a REST API automatically.

### `Store` routes (`/{collection}/{key}`)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/{collection}/{key}` | Create item (201 Created) |
| `GET` | `/{collection}/{key}` | Read — linearizable |
| `GET` | `/{collection}/{key}?consistency=stale` | Read — local, no round-trip |
| `PUT` | `/{collection}/{key}` | Update item |
| `DELETE` | `/{collection}/{key}` | Delete item |
| `GET` | `/{collection}` | List all — linearizable |
| `GET` | `/{collection}?consistency=stale` | List all — local |
| `POST` | `/{collection}/{key}/mutate` | Run named mutation |
| `GET` | `/status` | Cluster status (JSON) |
| `GET` | `/health` | Liveness probe |
| `GET` | `/metrics` | Prometheus metrics |

### `Manager` routes (`/groups/{groupID}/…`)

Same as above, prefixed with `/groups/{groupID}`.

### Leader routing

When a non-leader node receives a write or read request:

- If the leader's HTTP address is known, the node responds with `307 Temporary Redirect` to the leader, with a `Location` header.
- If the leader's HTTP address is not yet known, the node responds with `503 Service Unavailable`.

Clients that follow redirects (`curl -L`, most HTTP client libraries) are routed to the leader automatically without any special handling.

### Mutation request body

```json
{ "name": "increment", "args": 5 }
```

`args` is any valid JSON value and is passed verbatim to the mutation function as `[]byte`.

---

## Security

TLS for all gRPC Raft traffic:

```go
easyraft.WithTLS(tlsConfig)
```

The same `*tls.Config` is applied to both the gRPC server listener and all outbound client connections.

---

## Observability

```go
import "github.com/prometheus/client_golang/prometheus"

easyraft.WithPrometheus(prometheus.DefaultRegisterer)
```

Prometheus metrics are exposed on `GET /metrics`. The option must be set before `Start()`.

---

## Option reference

| Option | Description |
|--------|-------------|
| `WithID(id)` | Node ID — required |
| `WithRaftAddr(addr)` | gRPC listen address for Raft RPCs — required |
| `WithHTTPAddr(addr)` | HTTP listen address; enables the REST API |
| `WithDataDir(dir)` | Persistent storage directory — required |
| `WithPeers(map[NodeID]string)` | Static initial peer list |
| `WithDiscovery(d, interval)` | Dynamic peer discovery; wires both transport and membership |
| `WithSnapCount(n)` | Log entries between automatic snapshots (default 1000) |
| `WithLogger(logger)` | Custom `*slog.Logger` |
| `WithTLS(tlsConfig)` | TLS for gRPC transport |
| `WithPrometheus(registerer)` | Enable Prometheus metrics |
| `WithRaftTiming(tick, heartbeat, electionMin, electionMax)` | Override Raft timing (easyraft defaults: 100 ms tick/heartbeat, 1–2 s election) |

---

## Error reference

| Error | Meaning |
|-------|---------|
| `easyraft.ErrKeyNotFound` | Key does not exist in the collection |
| `easyraft.ErrKeyExists` | Key already exists (returned by `Create`) |
| `easyraft.ErrNotLeader` | This node is not the leader; retry on the leader |

---

## Examples

- [`examples/ratelimiter`](../examples/ratelimiter/) — token-bucket rate limiter with time-based refill. Demonstrates the deterministic mutation pattern.
