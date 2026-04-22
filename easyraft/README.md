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

## Upsert — atomic create-or-update

`Upsert` inserts a key if it does not exist, or replaces it if it does. Unlike calling `Create` and falling back to `Update` on `ErrKeyExists`, `Upsert` is a single atomic log entry with no race window:

```go
err := configs.Upsert(ctx, "db.host", ConfigEntry{Value: "localhost"})
```

---

## Change notifications

`OnChange` registers a callback that fires on **every replica** after each committed write is applied to that collection's local state — outside the state-machine lock, in a dedicated dispatcher goroutine. This is the primitive for building watch/subscribe flows.

```go
configs.OnChange(func(key string, entry *ConfigEntry, deleted bool) {
    if deleted {
        fmt.Printf("deleted: %s\n", key)
        return
    }
    fmt.Printf("changed: %s = %s\n", key, entry.Value)
})
```

The callback is called on followers as well as the leader, and during log replay after a restart or snapshot restore. One handler per collection is supported; a second call replaces the first. Register before `Start`.

**What `OnChange` does not guarantee:** if `notifyCh` (capacity 1024) fills up because the handler is slow, events are dropped. For production watch implementations, keep the handler fast — fan out to channels and let consumers process asynchronously.

---

## Watcher[T] — channel-based subscriptions

`Watcher[T]` converts `OnChange` events into buffered Go channels, making it easy to fan out to multiple concurrent consumers (e.g. SSE connections). Wire it up before `Start`:

```go
w := easyraft.NewWatcher[ConfigEntry]()
configs.OnChange(w.Notify) // Notify matches the OnChange callback signature

// In a handler goroutine:
ch := w.Subscribe("")        // "" = all keys
defer w.Unsubscribe("", ch)

for ev := range ch {          // ev is ChangeEvent[ConfigEntry]
    fmt.Printf("%s: deleted=%v\n", ev.Key, ev.Deleted)
}

// Or for a single key:
ch = w.Subscribe("db.host")
defer w.Unsubscribe("db.host", ch)
```

`ChangeEvent[T]` carries `Key string`, `Value *T` (nil on delete), and `Deleted bool`.

Events that arrive while a subscriber's channel (capacity 64) is full are silently dropped — keep consumers fast or size the channel appropriately.

### ServeSSE — streaming events over HTTP

`ServeSSE` handles the full SSE lifecycle: sets headers, sends a snapshot of existing entries on connect, then streams live events until the client disconnects:

```go
func (s *server) handleWatch(w http.ResponseWriter, r *http.Request) {
    snap, _ := s.configs.ListStale()
    s.watcher.ServeSSE(w, r, r.PathValue("key"), snap)
}
```

The `key` argument scopes the stream — pass `""` to receive all keys. SSE events are JSON-encoded `ChangeEvent[T]`:

```
event: snapshot
data: {"key":"db.host","value":{...}}

event: change
data: {"key":"db.host","value":{...}}

event: delete
data: {"key":"db.host","deleted":true}
```

See [`examples/configsvc`](../examples/configsvc/) for a complete SSE watch service.

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

## Joining a running cluster

`WithJoinAddr` lets a new node join an existing cluster by contacting a seed node over HTTP, instead of configuring every node with the full peer list up-front. The seed calls `AddServer` on behalf of the joiner and returns the current peer list so the new node can bootstrap its transport.

```go
// Seed nodes — already running with WithHTTPAddr.
n1, _ := easyraft.New[Counter](
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithHTTPAddr(":8001"),
    easyraft.WithDataDir("/data/n1"),
)
n1.Start()

// Joining node — contacts n1 on startup.
n2, _ := easyraft.New[Counter](
    easyraft.WithID("n2"),
    easyraft.WithRaftAddr(":7002"),
    easyraft.WithDataDir("/data/n2"),
    easyraft.WithJoinAddr("localhost:8001"),
)
n2.Start()
```

`WithJoinAddr` accepts one or more HTTP addresses; the joining node tries each in turn until one succeeds. If the contacted node is not the leader it responds with `307 Temporary Redirect` automatically.

You can add nodes one at a time in separate terminal sessions — no reconfiguration of the existing nodes is required.

---

## Learner nodes (non-voters)

`WithJoinAsLearner` causes a joining node to enter the cluster as a non-voting member. Learners replicate the log but do not participate in elections or count toward commit quorum. This is useful for read-replica nodes or for nodes that should be promoted to voters after they catch up.

```go
replica, _ := easyraft.New[Counter](
    easyraft.WithID("replica1"),
    easyraft.WithRaftAddr(":7010"),
    easyraft.WithDataDir("/data/replica1"),
    easyraft.WithJoinAddr("leader-host:8001"),
    easyraft.WithJoinAsLearner(),
)
replica.Start()
```

Requires `WithJoinAddr` — learner mode only applies during the join flow.

---

## Graceful departure (WithLeaveOnStop)

`WithLeaveOnStop` causes `Stop()` to call `RemoveServer(self)` before shutting down, gracefully removing this node from the cluster so remaining members do not wait for it during elections or commits.

```go
node, _ := easyraft.New[Counter](
    easyraft.WithID("n3"),
    easyraft.WithRaftAddr(":7003"),
    easyraft.WithDataDir("/data/n3"),
    easyraft.WithJoinAddr("seed:8001"),
    easyraft.WithLeaveOnStop(),
)
node.Start()
defer node.Stop() // removes self from cluster before shutting down
```

If removal does not complete within 5 seconds the node shuts down anyway. Do not use this on a bootstrap node (the first node in a brand-new cluster) — removing the sole member leaves the cluster with no voters.

---

## Cluster membership — Go API

`RemoveServer` and `TransferLeadership` are available directly on `EasyRaft[T]` and `Store`:

```go
// Remove a node from the cluster. Must be called on the leader; if called on a
// follower, returns ErrNotLeader.
err := er.RemoveServer(ctx, "n3")

// Gracefully hand off leadership to another node. Blocks until this node steps
// down or the context expires.
err := er.TransferLeadership(ctx, "n2")
```

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

When `WithHTTPAddr` is set, EasyRaft mounts a REST API automatically. If your application already runs its own HTTP server on the same address, use `WithHTTPMux` instead to register EasyRaft's routes on your mux — EasyRaft will not start a separate server:

```go
mux := http.NewServeMux()

store, _ := easyraft.NewStore(
    easyraft.WithHTTPAddr(":8001"), // advertise URL for leader redirects
    easyraft.WithHTTPMux(mux),      // register /join, /members, CRUD routes here
    // ...
)

mux.HandleFunc("GET /my-route", myHandler) // add your own routes

store.Start()
http.ListenAndServe(":8001", mux) // one server, no conflict
```

### `Store` routes

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/join` | Join request from a new node (called by `WithJoinAddr`) |
| `GET` | `/members` | List all cluster members with voter and leader flags |
| `DELETE` | `/members/{id}` | Remove a member from the cluster (leader only) |
| `POST` | `/transfer-leadership` | Transfer leadership: `{"to": "nodeID"}` |
| `POST` | `/batch` | Atomic multi-collection batch (see below) |
| `POST` | `/{collection}/{key}` | Create item (201 Created) |
| `GET` | `/{collection}/{key}` | Read — linearizable |
| `GET` | `/{collection}/{key}?consistency=stale` | Read — local, no round-trip |
| `PUT` | `/{collection}/{key}` | Update item |
| `PATCH` | `/{collection}/{key}` | Upsert item (create or replace) |
| `DELETE` | `/{collection}/{key}` | Delete item |
| `GET` | `/{collection}` | List all — linearizable |
| `GET` | `/{collection}?consistency=stale` | List all — local |
| `POST` | `/{collection}/{key}/mutate` | Run named mutation |
| `GET` | `/status` | Cluster status (JSON) |
| `GET` | `/health` | Liveness probe |
| `GET` | `/metrics` | Prometheus metrics |

### `Manager` routes (`/groups/{groupID}/…`)

Same as the `Store` routes above, prefixed with `/groups/{groupID}` (e.g. `GET /groups/1/members`).

### Members response

`GET /members` returns:

```json
{
  "members": [
    { "id": "n1", "raft_addr": "host1:7001", "voter": true, "leader": true, "self": true },
    { "id": "n2", "raft_addr": "host2:7001", "voter": true, "leader": false, "self": false },
    { "id": "n3", "raft_addr": "host3:7001", "voter": false, "leader": false, "self": false }
  ]
}
```

`voter: false` identifies a learner node. `leader: true` identifies the current Raft leader. `self: true` marks the node that served the response.

### Batch request body

`POST /batch` applies multiple operations atomically — they commit as a single log entry:

```json
[
  { "op": "create", "collection": "users",   "key": "alice", "value": {"name":"Alice"} },
  { "op": "create", "collection": "scores",  "key": "alice", "value": 100 },
  { "op": "update", "collection": "counters","key": "total", "value": 42 },
  { "op": "upsert", "collection": "configs", "key": "db.host", "value": "localhost" },
  { "op": "delete", "collection": "sessions","key": "old-session" },
  { "op": "mutate", "collection": "quotas",  "key": "alice",
    "mutate_name": "decrement", "mutate_args": 1 }
]
```

Valid `op` values: `create`, `update`, `upsert`, `delete`, `mutate`.

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
| `WithHTTPAddr(addr)` | HTTP listen address; enables the REST API and sets the advertised URL for leader redirects |
| `WithHTTPMux(mux)` | Register EasyRaft routes on an existing mux instead of starting a dedicated server; pair with `WithHTTPAddr` for redirect advertising |
| `WithDataDir(dir)` | Persistent storage directory — required |
| `WithPeers(map[NodeID]string)` | Static initial peer list |
| `WithJoinAddr(addrs...)` | HTTP address(es) of seed nodes to join on startup |
| `WithJoinAsLearner()` | Join as a non-voting learner (requires `WithJoinAddr`) |
| `WithLeaveOnStop()` | Call `RemoveServer(self)` before shutdown for graceful departure |
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
- [`examples/configsvc`](../examples/configsvc/) — distributed configuration service with SSE watch streams. Demonstrates `Upsert`, `OnChange`, and the replicated-state vs. local-subscriber pattern.
- [`examples/ledger`](../examples/ledger/) — double-entry ledger with atomic multi-collection transactions. Demonstrates `Store.Txn`, idempotent transfers via `ErrKeyExists`, and mutation-enforced invariants.
