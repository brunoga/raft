# EasyRaft

EasyRaft is a high-level abstraction layer over the `brunoga/raft` package. It lets you build strongly-consistent, replicated services by describing **what** data to replicate — not how. Transport, storage, state machine, snapshotting, leader routing, and peer discovery are all handled automatically.

```
go get github.com/brunoga/raft/v2/easyraft
```

> ### Read this before exposing a node
>
> Two subsystems reshape cluster membership, and both are permissive by default so that existing deployments keep working:
>
> - **Both listeners need a security decision before a node starts.** The Raft transport refuses to run without `WithTLS` unless `WithInsecureTransportAcknowledged` says plaintext is intended, and the HTTP API refuses to serve without `WithBearerTokenAuth` (or `WithHTTPAuth`) unless `WithInsecureHTTPAcknowledged` says an open API is intended. Anyone who can reach the `WithHTTPAddr` listener can `POST /join` to add a member, `DELETE /members/{id}` to shrink the cluster, transfer leadership, or write to any collection, so bind it to a management interface rather than all interfaces and use `WithHTTPTLS` when it is not on a trusted link. The acknowledgements are for exposure mitigated elsewhere — a loopback bind, a service mesh, a network policy — and a node started with one logs a warning.
> - **Peer discovery trusts its source.** `udpbroadcast` accepts any datagram on the subnet unless you give it a shared secret. Discovered peers therefore join as **non-voting learners** by default, so a rogue announcement cannot change the quorum; `WithDiscoveryAsVoter` opts out of that.
>
> The [Security](#security) section covers both in full.

---

## Two entry points

| | `easyraft.New[T]` | `easyraft.NewStore` |
|---|---|---|
| State | One typed collection | Multiple typed collections |
| Typical use | Single-domain service | Multi-entity service |
| Example | `EasyRaft[Counter]` | `AddCollection[User] + AddCollection[Session]` |

Both share the same options, the same `Collection[T]` API, and the same underlying `Store`.

Starting with `New[T]` does not close the other door. `EasyRaft[T].Store()`
returns the `Store` the wrapper is running on, so a service that later needs a
second collection or a transaction across two keys reaches for it rather than
rewriting its construction:

```go
audit := easyraft.AddCollection[AuditRecord](app.Store(), "audit")

_, err := app.Store().Txn(ctx, func(tx *easyraft.Txn) error {
    if err := tx.Update("default", "acct-1", updated); err != nil {
        return err
    }
    return tx.Create("audit", eventID, AuditRecord{Actor: who})
})
```

The wrapper's own collection is named `default`.

---

## Quick start — single collection

```go
type Counter struct {
    Value int64 `json:"value"`
}

er, err := easyraft.New[Counter](
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    // The HTTP address is advertised to peers for leader redirects, so it must
    // name a host they can dial — not a port-only ":8001".
    easyraft.WithHTTPAddr("host1:8001"),
    easyraft.WithBearerTokenAuth(os.Getenv("RAFT_TOKEN")), // see Security
    easyraft.WithDataDir("/data/n1"),
    easyraft.WithPeers(map[raft.NodeID]string{
        "n2": "host2:7001",
        "n3": "host3:7001",
    }),
)
if err := er.Start(); err != nil {
    log.Fatal(err) // e.g. this node was told to join a cluster and could not
}
defer func() {
    if err := er.Stop(); err != nil {
        log.Printf("easyraft shutdown: %v", err)
    }
}()

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

if err := store.Start(); err != nil {
    log.Fatal(err)
}
defer func() {
    if err := store.Stop(); err != nil {
        log.Printf("easyraft shutdown: %v", err)
    }
}()

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

### Named identities — `Session`, `OnceID`, `Exactly`

The positional `(clientID, seqNum)` pair sits before the key and is easy to transpose. `Exactly` takes the same identity as a named-field `OnceID`, which cannot be:

```go
session := easyraft.NewSession("client-42") // hands out increasing sequence numbers

id := session.Next()                        // one logical write
err := users.Exactly(id).Create(ctx, "alice", User{Name: "Alice"})

// Retrying? Reuse the same id — that is what makes the write exactly-once.
// Allocating a fresh one would apply the command twice.
err = users.Exactly(id).Create(ctx, "alice", User{Name: "Alice"})
```

`Exactly` offers `Create`, `Update`, `Upsert`, `Delete` and `Mutate`. The `*Once` methods are unchanged and remain supported — they now delegate to the same path.

A `Session` starts at sequence number 1, so a process that restarts and reuses a client ID must resume where it left off. Record `session.LastSeqNum()` and rebuild with `easyraft.NewSessionAt(clientID, last)`.

---

## Upsert — atomic create-or-update

`Upsert` inserts a key if it does not exist, or replaces it if it does. Unlike calling `Create` and falling back to `Update` on `ErrKeyExists`, `Upsert` is a single atomic log entry with no race window:

```go
err := configs.Upsert(ctx, "db.host", ConfigEntry{Value: "localhost"})
```

---

## Revisions — compare-and-swap

Every key carries a **revision**: the index of the log entry that last wrote it. Read it with `ReadRev`, and hand it back to a conditional write, which applies only while the key is still at that revision:

```go
value, rev, err := configs.ReadRev(ctx, "db.host")
if err != nil {
    return err
}
value.Port = 5433

err = configs.UpdateIf(ctx, "db.host", value, rev)
if errors.Is(err, easyraft.ErrRevisionMismatch) {
    // Somebody wrote the key in between. Read it again and redo the work.
}
```

That loop is a read-modify-write that loses nothing without holding a lock. A plain `Read` followed by `Update` is two log entries with a window between them, and a write that lands in the window is overwritten with no error; `UpdateIf` turns the same race into `ErrRevisionMismatch` and a retry.

The condition is checked on every replica as the entry applies, not on the leader before it proposes. It therefore holds against every other entry in the log, whatever order they commit in and whichever node proposed them.

| Method | Applies only if |
|--------|-----------------|
| `UpdateIf(ctx, key, value, rev)` | the key exists and is at `rev` |
| `UpsertIf(ctx, key, value, rev)` | the key is at `rev` — `0` means it does not exist |
| `DeleteIf(ctx, key, rev)` | the key exists and is at `rev` |
| `MutateIf(ctx, key, name, args, rev)` | the key exists and is at `rev` |

A revision of `0` is the one a caller can name without reading: the revision of a key that has never been written. `UpsertIf(ctx, key, value, 0)` is therefore create-if-absent, and unlike `Create` it also refuses a key that was deleted and recreated since.

`ReadStaleRev` returns a revision from the local replica without a leader round-trip. A stale revision is still a safe basis for a conditional write — it is either current or behind, and a conditional write on a revision that has moved is refused. Reading stale costs a retry, never a lost update.

`Store.Revision()` returns the highest revision this replica has applied, a watermark for the store as a whole rather than for one key.

### When to use a mutation instead

A registered mutation is already an atomic read-modify-write in a single entry, and needs no revision. Reach for a conditional write when the new value is computed somewhere a mutation cannot run — in a browser, in another service, from a human decision — and the write has to be refused if the world moved while that was happening.

### Guarding a transaction

`Txn.CheckRev` adds a condition on a key the transaction does not write. It is how a batch is made conditional on something outside itself — a lease still held, a configuration not yet superseded:

```go
_, err := store.Txn(ctx, func(tx *easyraft.Txn) error {
    if err := tx.CheckRev("leases", "shard-7", leaseRev); err != nil {
        return err
    }
    return tx.Upsert("work", "item-1", result)
})
```

If any condition in a transaction fails, the whole batch fails with `ErrRevisionMismatch` and none of it is written. `Txn.UpdateIf`, `Txn.UpsertIf` and `Txn.DeleteIf` carry the same condition on a key the transaction does write.

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

**Each collection gets its own dispatcher goroutine and queue**, so a handler that blocks delays only the collection it was registered for — one slow watcher can no longer starve every other collection in the store.

### Detecting dropped events — `OnChangeEvent`

A handler that falls far enough behind still loses events: the store-wide queue holds 1024 and each collection's holds 256. `OnChange` cannot tell you that happened. `OnChangeEvent` can:

```go
configs.OnChangeEvent(func(ev easyraft.ChangeEvent[ConfigEntry]) {
    if ev.Gap {
        // Events were dropped. Nothing behind the gap is recoverable —
        // re-read the collection instead of assuming you are up to date.
        resync()
        return
    }
    apply(ev.Seq, ev.Key, ev.Value, ev.Deleted)
})
```

`Seq` increases by one per event the store emits, so a handler that remembers the last one it saw can detect a discontinuity itself. A `Gap` event carries no key or value — it is a signal, not an entry.

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

`ChangeEvent[T]` carries `Seq uint64`, `Key string`, `Value *T` (nil on delete), `Deleted bool`, and `Gap bool`.

Wire `NotifyEvent` to `OnChangeEvent` instead of `Notify`/`OnChange` so that gaps reported by the store reach subscribers too:

```go
configs.OnChangeEvent(w.NotifyEvent)
```

**Subscriber channels hold 64 events.** A subscriber that falls behind loses events — but never silently: the next event it receives is preceded by one with `Gap` set, telling it to resynchronise. `Seq` is assigned by the `Watcher` and is contiguous across everything it dispatches, so a consumer can verify this for itself.

**`Unsubscribe` is mandatory.** An abandoned subscription keeps its channel and registry slot for the lifetime of the `Watcher`, and every later event pays the cost of trying to deliver to it. Use `SubscribeContext` to tie that cleanup to a context:

```go
for ev := range w.SubscribeContext(r.Context(), key) {
    // the channel is closed and the subscription removed when ctx ends
}
```

### ServeSSE — streaming events over HTTP

`ServeSSEFunc` handles the full SSE lifecycle: sets headers, reads a snapshot of existing entries as part of subscribing, then streams live events until the client disconnects:

```go
func (s *server) handleWatch(w http.ResponseWriter, r *http.Request) {
    s.watcher.ServeSSEFunc(w, r, r.PathValue("key"), s.configs.ListStale)
}
```

Passing the read rather than its result is what anchors the stream: the snapshot is taken with dispatch held off, so every event the client receives describes a change that happened *after* it. The older `ServeSSE(w, r, key, snapshot)` takes an already-read map and is still supported, but a write landing between the read and the subscription leaves a window where the client can see a change event for a key whose snapshot value is already newer.

The `key` argument scopes the stream — pass `""` to receive all keys. SSE events are JSON-encoded `ChangeEvent[T]`:

```
event: snapshot
data: {"seq":12,"key":"db.host","value":{...}}

event: change
data: {"seq":13,"key":"db.host","value":{...}}

event: delete
data: {"seq":14,"key":"db.host","deleted":true}

event: gap
data: {"seq":15,"gap":true}
```

A `gap` event means the client missed one or more changes and should re-read the collection. If the snapshot read itself fails, the stream carries a single `error` event and closes — the headers are already sent by then, so there is no status code left to use.

See [`examples/configsvc`](../examples/configsvc/) for a complete SSE watch service.

---

## Multi-Raft — sharding across groups

`easyraft.Manager` runs multiple independent Raft groups (shards) on a single physical node, sharing one gRPC transport and one HTTP server:

```go
mgr, err := easyraft.NewManager(
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithHTTPAddr("host1:8001"),
    easyraft.WithBearerTokenAuth(token),
    easyraft.WithPrometheus(reg), // one registry for every group
    easyraft.WithPeers(peers),
)

// Each store is an independent Raft group.
s1, _ := mgr.AddStore(1, easyraft.WithDataDir("/data/shard1"))
users  := easyraft.AddCollection[User](s1, "users")

s2, _ := mgr.AddStore(2, easyraft.WithDataDir("/data/shard2"))
orders := easyraft.AddCollection[Order](s2, "orders")

if err := mgr.Start(); err != nil {
    log.Fatal(err) // no group half-started: Start cleans up before returning
}
defer func() {
    if err := mgr.Stop(); err != nil {
        log.Printf("easyraft shutdown: %v", err)
    }
}()
```

`AddStore` accepts the same options as `NewStore`. Options set on the `Manager` (e.g. `WithID`, `WithPeers`) are inherited by stores; store-level options override them — including `WithPrometheus`, whose registerer every group shares. Each group's series are told apart by a `group` label.

`Manager.Start` does not hold its lock while a group joins its cluster, so `GetStore` and the HTTP handlers stay responsive even when a join is waiting out its 30-second retry budget against a seed that is still coming up.

---

### One log for every group

With a data directory per group, G groups appending at once issue G fsyncs, and fsync is the expensive part — it is what makes disk throughput rather than CPU the limit on how many writing groups a host can carry. `WithSharedWAL` puts every group on one write-ahead log, synced once per batch:

```go
mgr, _ := easyraft.NewManager(
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr("10.0.0.1:7001"),
    easyraft.WithDataDir("/var/lib/raft"),   // the log lives here
    easyraft.WithSharedWAL(),
)

// No per-group WithDataDir: each group is a partition of the one log.
s1, _ := mgr.AddStore(1)
s2, _ := mgr.AddStore(2)
```

`Manager.SharedWAL()` is the handle for what only the log knows:

```go
wal := mgr.SharedWAL()
wal.Groups()        // which groups the log holds — how a host finds them after a restart
wal.Remove(7)       // forget a decommissioned group so its space can be reclaimed
wal.Reclaim()       // run that reclamation now rather than after the next compaction
```

Removing a store from the Manager deliberately does **not** remove its log: a group taken off a host is usually coming back.

## Joining a running cluster

`WithJoinAddr` lets a new node join an existing cluster by contacting a seed node over HTTP, instead of configuring every node with the full peer list up-front. The seed calls `AddServer` on behalf of the joiner and returns the current peer list so the new node can bootstrap its transport.

```go
// Seed nodes — already running with WithHTTPAddr.
n1, _ := easyraft.New[Counter](
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithHTTPAddr("localhost:8001"),
    easyraft.WithBearerTokenAuth(token),
    easyraft.WithDataDir("/data/n1"),
)
if err := n1.Start(); err != nil {
    log.Fatal(err)
}

// Joining node — contacts n1 on startup.
n2, _ := easyraft.New[Counter](
    easyraft.WithID("n2"),
    easyraft.WithRaftAddr(":7002"),
    easyraft.WithDataDir("/data/n2"),
    easyraft.WithJoinAddr("localhost:8001"),
    easyraft.WithBearerTokenAuth(token), // same token as the seed
)
if err := n2.Start(); err != nil {
    log.Fatal(err) // the seed never answered; this node is not in the cluster
}
```

`WithJoinAddr` accepts one or more HTTP addresses; the joining node tries each in turn until one succeeds. If the contacted node is not the leader it responds with `307 Temporary Redirect` automatically. When the seeds require authorization, configure the joiner with the matching `WithBearerTokenAuth` — it is sent on the join request.

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
if err := replica.Start(); err != nil {
    log.Fatal(err)
}
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
if err := node.Start(); err != nil {
    log.Fatal(err)
}
defer func() {
    // Stop removes this node from the cluster before shutting down, and says
    // so if the departure did not happen.
    if err := node.Stop(); err != nil {
        log.Printf("did not leave the cluster cleanly: %v", err)
    }
}()
```

On the leader the membership change is proposed directly. A follower cannot commit one, so the removal is **forwarded to the leader's HTTP API** (`DELETE /members/{self}`), carrying the credential from `WithBearerTokenAuth` when one is configured. If the leader is unknown, has not advertised an HTTP address, or rejects the request, shutdown still completes — releasing the ports and file handles — and `Stop` returns the reason. The node is then still a member and an operator must remove it, so do not discard that error. Call `store.Leave(ctx)` directly if you want to handle the departure separately from shutdown.

The whole attempt is abandoned after 5 seconds. Do not use this on a bootstrap node (the first node in a brand-new cluster) — removing the sole member leaves the cluster with no voters.

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

## Witnesses

A witness votes and counts towards every quorum, keeps the shape of the log, and holds none of the data. Two full replicas and a witness survive the loss of any one member, at a third of the storage and none of the state machine a third full replica would cost — a small machine in a third location whose job is to break ties.

```go
// On the witness itself.
w, _ := easyraft.NewStore(
    easyraft.WithID("w1"),
    easyraft.WithRaftAddr("10.0.2.9:7001"),
    easyraft.WithDataDir("/var/lib/w1"),
    easyraft.WithPeers(peers),
    easyraft.WithWitnessPeers("w1"),
    easyraft.WithWitness(),
    // ...
)

// On every full replica, so that their view of the membership matches.
easyraft.WithWitnessPeers("w1")

// Or add one to a running cluster, from the leader.
err := store.AddWitness(ctx, "w1", "10.0.2.9:7001")
```

A witness holds no data, so every read against it returns `ErrWitness` rather than the empty answer its collections would otherwise give — reporting "not found" for a key the cluster holds would be worse than refusing. It never becomes leader, so writes against it are refused with the leader to redirect to, exactly as on any follower. Membership, status, health, joining and discovery all work as usual.

## Quorum, placement and timing

The knobs that decide how many replicas a write must reach, and where those replicas must be:

```go
// Writes must reach every voter before they are acknowledged; elections are
// unchanged. Group state, so this is only what the group is created with —
// change a running one with store.SetCommitQuorum(ctx, n).
easyraft.WithCommitQuorum(3)

// An acknowledged write must be in two failure domains, not just on a
// majority of machines that might all be in one.
easyraft.WithZones(map[raft.NodeID]raft.ZoneID{
    "n1": "eu-west-1a", "n2": "eu-west-1a", "n3": "eu-west-1b",
}, 2)

// Keep leadership beside the clients that write to it.
easyraft.WithPreferredLeader("n1")

// Bound what a lease read assumes about clocks (see WithLeaseReads).
easyraft.WithLeaseSafetyMargin(15 * time.Millisecond)

// Shed writes rather than queue them without limit when the event loop is
// behind; the default waits.
easyraft.WithProposalQueue(1024, raft.ProposalOverflowReject)

// Size the exactly-once table behind Session. Group state, like the commit
// quorum: store.SetMaxClientTableSize(ctx, n) changes a running group.
easyraft.WithMaxClientTableSize(100_000)
```

## Reacting to what the node does

```go
// Called once, from its own goroutine, when a committed change removes this
// node. It is not stopped for you — that decision is yours, and this is
// where it goes.
easyraft.WithOnRemoved(func() { _ = store.Stop() })

// Everything the node does: leadership changes, peers arriving and leaving,
// snapshots, a client dropped from the exactly-once table, a durable write
// that failed. Bounded and lossy; Event.Dropped says how many were missed.
events, stop := store.Events()
defer stop()
for ev := range events {
    log.Printf("%s: %+v", ev.Type, ev)
}
```

Subscribe before `Start` if you need the first leadership change: the stream carries what happens after a subscription, not what happened before it.

## Discovery

When `WithDiscovery` is configured, EasyRaft polls the discovery source periodically and:

1. Registers newly found peers with the gRPC transport (`AddPeer`).
2. Calls `AddServer` to add them to the Raft cluster membership **as non-voting learners**.

Only the current leader can commit membership changes; `AddServer` calls on followers fail and are retried on the next discovery interval.

```go
import "github.com/brunoga/raft/v2/discovery/udpbroadcast"

d, _ := udpbroadcast.New(&udpbroadcast.Config{
    NodeID: "n1",
    Addr:   "10.0.0.1:7001",
    Secret: []byte(os.Getenv("RAFT_DISCOVERY_SECRET")), // authenticate announcements
})

er, _ := easyraft.New[Counter](
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithDiscovery(d, 5*time.Second),
)
```

### Why learners, and how to promote

A discovery announcement is a claim made over the network. Honouring it as a *voting* member would let whoever made the claim change the cluster's quorum size and gain a vote in every election. Discovered peers therefore join as learners: they replicate the log but do not vote. Promote one, from the leader, once an operator has verified it:

```go
err := store.AddServer(ctx, "n4", "10.0.0.4:7001") // learner → voter
```

`WithDiscoveryAsVoter` restores the older behaviour of adding discovered peers directly as voters. Only enable it when the discovery source is authenticated — `udpbroadcast` with a shared secret on a subnet you control, for instance.

### Address changes

A peer listed in `WithPeers` is authoritative: discovery will never repoint it, because repointing a member redirects that member's Raft traffic to whoever announced the new address. Attempts are logged and ignored. A peer that discovery itself introduced *can* change address — a pod restarting with a new IP is normal — and every change is logged.

Static peers (`WithPeers`) and discovery can be combined: peers added via `WithPeers` are pre-populated in the known-member set and will not trigger redundant `AddServer` calls.

---

## Read consistency

`Read` and `List` are linearizable: before serving from local state they confirm that this node has applied everything committed cluster-wide. `ReadStale` and `ListStale` skip that and read local state directly.

By default the confirmation is a quorum heartbeat — one round-trip, and no assumption about clocks. `WithLeaseReads` lets the leader answer from its clock-based read lease instead, skipping the round-trip:

```go
easyraft.WithLeaseReads()
```

A read lease is only sound while clock drift between the leader and its followers stays below the election timeout; leave the option off if you cannot bound drift. Either way a caller never sees `raft.ErrLeaseExpired` — an expired lease falls back to the quorum-confirmed path automatically rather than failing the read.

The same applies to `GET /{collection}` and `GET /{collection}/{key}`; add `?consistency=stale` to read local state instead.

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

Both listeners are bound by `NewStore`, not by `Start`, so an address that is malformed or already in use is reported as an error you can act on instead of disappearing into a background goroutine and leaving the node up with no API:

```go
store, err := easyraft.NewStore(/* ... */, easyraft.WithHTTPAddr(":8001"))
if err != nil {
    log.Fatal(err) // "easyraft: listen http :8001: address already in use"
}
```

When no `WithLogger` is set, easyraft logs through `slog.Default()` rather than staying silent, so serve-time failures are still reported.

### Shutting down on a deadline

`Stop` finishes what it started: storage writes the node already accepted are carried out rather than abandoned, which is what makes an orderly restart keep the tail of its log instead of fetching it back from a peer, and a departure under `WithLeaveOnStop` is waited for. A disk that has hung rather than failed, or a leader that cannot be reached to accept the departure, holds it there.

A process that has to come down on a deadline uses `Shutdown` instead:

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
if err := store.Shutdown(ctx); err != nil {
    log.Printf("shutdown did not finish in time: %v", err)
}
```

Giving up does not cancel the shutdown — it carries on in the background, so the store is then neither running nor finished and its data directory must not be reopened by another process. `Manager.Shutdown` is the same for a manager.

### What `Start` and `Stop` report

`Store.Start`, `EasyRaft.Start`, `Manager.Start`, and their `Stop` counterparts all return an `error`. Handle it — a node that did not join the cluster it was pointed at is the difference between "my process started" and "my process is part of the cluster", and an application needs to tell those apart to fail its own startup, retry, or report unreadiness to an orchestrator.

`Start` fails when `WithJoinAddr` was set and no seed accepted the join before the 30-second retry budget expired. Nothing is started in that case, so the only thing left to do is call `Stop` to release the listeners `NewStore` bound. `Manager.Start` applies the same rule per group and fails the whole Manager, because a node that silently comes up missing one of its shards gives an operator nothing to notice.

The following startup failures stay out of `Start`'s return value on purpose: none of them decides whether this node participates in consensus, and none is settled by the time `Start` returns.

| Not fatal | Why |
|-----------|-----|
| Advertising this node's HTTP address | Retried in the background until it succeeds. It only affects whether other nodes can redirect clients here; a node without an advertised address replicates normally. A leader election in progress at startup makes an early failure the common case. |
| Serving the HTTP API | The listener is bound by `NewStore`, so a bind failure is already reported there. Anything left can only happen after the server is accepting, which is after `Start` has returned. |
| A discovery lookup or `AddServer` | Discovery polls on an interval and retries; a peer that is missed this round is picked up on the next one. |

`Stop` runs every shutdown step whatever the earlier ones reported — it must release the listeners and file handles either way — and returns the failures joined together. Two are worth acting on: a departure that did not happen under `WithLeaveOnStop` (this node is still in the committed membership and still counts towards quorum), and a storage close failure (the on-disk Raft log may not be intact, which decides whether this node can be restarted or has to be rebuilt from a peer). `Stop` is idempotent; a second call repeats the first call's verdict.

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

if err := store.Start(); err != nil {
    log.Fatal(err)
}
http.ListenAndServe(":8001", mux) // one server, no conflict
```

**Every route below is behind the `WithHTTPAuth` hook when one is configured, and open to anyone who can reach the listener when one is not.** See [Security](#security) before binding to anything but loopback.

### `Store` routes

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/join` | Join request from a new node (called by `WithJoinAddr`) |
| `GET` | `/members` | List all cluster members with voter and leader flags |
| `DELETE` | `/members/{id}` | Remove a member from the cluster (leader only) |
| `POST` | `/transfer-leadership` | Transfer leadership: `{"to": "nodeID"}` |
| `POST` | `/batch` | Atomic multi-collection batch (see below) |
| `POST` | `/{collection}/{key}` | Create item (201 Created) |
| `GET` | `/{collection}/{key}` | Read — linearizable, returns the key's revision as `ETag` |
| `GET` | `/{collection}/{key}?consistency=stale` | Read — local, no round-trip |
| `PUT` | `/{collection}/{key}` | Update item — honours `If-Match` |
| `PATCH` | `/{collection}/{key}` | Upsert item (create or replace) — honours `If-Match` / `If-None-Match` |
| `DELETE` | `/{collection}/{key}` | Delete item — honours `If-Match` |
| `GET` | `/{collection}` | List all — linearizable |
| `GET` | `/{collection}?consistency=stale` | List all — local |
| `POST` | `/{collection}/{key}/mutate` | Run named mutation — honours `If-Match` |
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

Valid `op` values: `create`, `update`, `upsert`, `delete`, `mutate`, `check`.

Any operation may carry an `if_rev` that makes it conditional on the key's current revision, and `check` carries nothing else — it writes nothing and only asserts that a key is at the revision given. A failed condition anywhere in the batch answers `412 Precondition Failed` and writes none of it:

```json
[
  { "op": "check",  "collection": "leases", "key": "shard-7", "if_rev": 41 },
  { "op": "upsert", "collection": "work",   "key": "item-1", "value": {"done":true} }
]
```

### Conditional requests

A `GET` of a single key returns the key's revision as an `ETag`, and both a `GET` of a key and a `GET` of a collection return the replica's own watermark as `X-Raft-Revision`. Handing an `ETag` back as `If-Match` makes the next write conditional on it:

```
$ curl -i http://host:8001/configs/db.host
HTTP/1.1 200 OK
ETag: 412
X-Raft-Revision: 419

$ curl -X PUT -H 'If-Match: 412' -d '{"port":5433}' http://host:8001/configs/db.host
HTTP/1.1 204 No Content

$ curl -X PUT -H 'If-Match: 412' -d '{"port":5434}' http://host:8001/configs/db.host
HTTP/1.1 412 Precondition Failed
```

`If-None-Match: *` requires that the key does not exist, which on `PATCH` is create-if-absent. Quoted and unquoted ETags are both accepted. Anything else — `If-Match: *`, a weak validator, a list of ETags, both headers at once — is answered with `400` rather than applied unconditionally, because a guess about which one the caller meant is a lost update.

### Leader routing

When a non-leader node receives a write or read request:

- If the leader's HTTP address is known, the node responds with `307 Temporary Redirect` to the leader, with a `Location` header.
- If the leader's HTTP address is not yet known, the node responds with `503 Service Unavailable`.

Clients that follow redirects (`curl -L`, most HTTP client libraries) are routed to the leader automatically without any special handling.

The redirect host comes only from the internal metadata collection — which HTTP clients cannot read or write — and is re-validated as a bare `host:port` before use. An advertised value that is not one (an appended path, a userinfo section, an embedded newline, or a port-only `:8001` that no client could dial) is refused and logged, and the node answers `503` instead. Nothing from the incoming request ever reaches the `Location` header except its path and query.

### Error responses

| Status | Returned for |
|--------|--------------|
| `400 Bad Request` | Malformed JSON, missing `id`/`raft_addr`/`to`, a `raft_addr` that is not `host:port` |
| `401 Unauthorized` | Missing or unusable credential (see Security) |
| `403 Forbidden` | Valid credential without permission, or any request naming a reserved `__`-prefixed collection |
| `404 Not Found` | `ErrKeyNotFound`, unknown collection, unknown group |
| `408 Request Timeout` | The request context expired before the entry committed |
| `409 Conflict` | `ErrKeyExists`, `raft.ErrObsoleteSeqNum`, a config change or leadership transfer already in progress |
| `412 Precondition Failed` | `ErrRevisionMismatch` — an `If-Match`, `If-None-Match` or batch `if_rev` did not hold |
| `503 Service Unavailable` | No leader elected, leader's address unknown, node stopped, request cancelled |

### Mutation request body

```json
{ "name": "increment", "args": 5 }
```

`args` is any valid JSON value and is passed verbatim to the mutation function as `[]byte`.

---

## Security

Three separate surfaces, each configured on its own: the Raft transport, the HTTP API, and peer discovery.

### Raft transport (gRPC)

From PEM files, which is the common case and the one that is easy to get wrong:

```go
easyraft.WithTLSFiles("node.crt", "node.key", "ca.crt")
```

That builds exactly the configuration below: the authority trusted in both directions, because every node here is both a client and a server, and client certificates required *and verified*, because anything weaker leaves the Raft port open to any client that can reach it. The files are read when the store is constructed, so a wrong path fails there rather than at the first connection between two nodes.

Or build it yourself:

```go
easyraft.WithTLS(&tls.Config{
    Certificates: []tls.Certificate{nodeCert},
    RootCAs:      pool,
    ClientCAs:    pool,
    // Every node is both a client and a server here, so require both ends
    // to prove who they are. Anything weaker leaves the Raft port open to
    // any client that can reach it.
    ClientAuth:   tls.RequireAndVerifyClientCert,
})
```

The same `*tls.Config` is applied to both the gRPC server listener and all outbound client connections. It does **not** cover the HTTP API.

**Authentication is not authorization.** TLS establishes that the peer holds a certificate your CA issued. It says nothing about *which* node that peer is, and every Raft RPC names the node it claims to come from — so without a check binding the two, any holder of any certificate from that CA can claim to be the leader and make the whole cluster step down.

A configuration with `ClientAuth: tls.RequireAndVerifyClientCert` therefore also gets a peer authorizer, matching the claimed node ID against the certificate's Common Name and DNS names. Pass `WithPeerAuthorizer` when your certificates carry identity elsewhere:

```go
easyraft.WithPeerAuthorizer(grpctransport.MTLSPeerAuthorizer(
    func(cert *x509.Certificate) []string { return cert.URIs[0:1] ... },
))
```

A TLS configuration that does **not** require client certificates gets no authorizer — installing one where no verified certificate exists would refuse every inbound RPC — and the node logs a warning at startup, because such a listener authenticates the server to its clients and leaves the clients anonymous.

Without it, `NewStore` and `NewManager` return an error unless
`WithInsecureTransportAcknowledged` is passed. A Raft peer is fully trusted, so
a plaintext transport is a cluster anyone who can reach the port can take over;
the acknowledgement is for a network trusted for reasons outside easyraft (a
loopback bind for a local cluster, a private link, a mesh that terminates TLS
in front of the process), and the transport logs a warning once when used.

### HTTP API

The HTTP API is a cluster control plane: `POST /join` adds a member, `DELETE /members/{id}` removes one, `POST /transfer-leadership` moves leadership, and the CRUD routes write to every collection. Anyone who can reach the listener can do all of that.

```go
er, _ := easyraft.New[Counter](
    // Bind to an interface only your operators and peers can reach, not to
    // every interface. This address is also what peers redirect clients to,
    // so it must be one they can dial.
    easyraft.WithHTTPAddr("10.0.0.1:8001"),
    easyraft.WithBearerTokenAuth(os.Getenv("RAFT_TOKEN")),
    // ...
)
```

`WithBearerTokenAuth` does two things: it requires `Authorization: Bearer <token>` on every inbound request, and it sends the same header on the cluster-control requests this node makes to its peers — so `WithJoinAddr` and `WithLeaveOnStop` keep working against an authenticated cluster. Use the same token on every node.

For mutual TLS instead, serve the API over TLS and authorize on the client certificate:

```go
tlsCfg := &tls.Config{
    Certificates: []tls.Certificate{serverCert},
    ClientCAs:    pool,
    ClientAuth:   tls.RequireAndVerifyClientCert,
}

easyraft.WithHTTPTLS(tlsCfg),
easyraft.WithHTTPAuth(easyraft.ClientCertAuth("admin", "n1", "n2")),
```

`WithHTTPTLS` also makes leader redirects use the `https` scheme. It is ignored with `WithHTTPMux`, where the caller owns the listener.

Any policy works — `WithHTTPAuth` takes a `func(*http.Request) error`. Wrap `easyraft.ErrUnauthorized` to answer `401` and `easyraft.ErrForbidden` to answer `403`; any other error is reported as `403` without echoing its text to the client.

```go
easyraft.WithHTTPAuth(func(r *http.Request) error {
    if !myACL.Allows(r) {
        return fmt.Errorf("%w: not on the admin allowlist", easyraft.ErrForbidden)
    }
    return nil
})
```

The hook runs before **every** easyraft route, reads included — a hook covering only writes would be a trap, since `GET /members` maps out the cluster. Routes registered on your own mux are unaffected.

**Without a hook the node refuses to start** when it would serve the API (`WithHTTPAddr` or `WithHTTPMux`), unless `WithInsecureHTTPAcknowledged` says an open API is intended because the exposure is mitigated elsewhere (a loopback bind, a service mesh, a network policy). A node started that way logs a warning once.

#### Reserved collections

Collection names beginning with `__` are easyraft's own. They hold the map of advertised HTTP addresses that leader redirects are built from, so the HTTP layer refuses them outright — reads and writes, single-key and batch, `403 Forbidden` — and `AddCollection` panics on one. A client able to write that map would choose where every follower forwards its traffic, request bodies included; one able to read it would get the cluster's internal address map.

### Peer discovery

`udpbroadcast` accepts any well-formed datagram on the subnet unless you configure a shared secret:

```go
d, _ := udpbroadcast.New(&udpbroadcast.Config{
    NodeID: "n1",
    Addr:   "10.0.0.1:7001",
    Secret: []byte(os.Getenv("RAFT_DISCOVERY_SECRET")),
})
```

With a secret set, every announcement is signed with an HMAC over `(id, addr, timestamp, nonce)`; announcements that are unsigned, wrongly signed, outside the replay window (30 s by default, which also bounds tolerated clock skew), or replaying a nonce are dropped and logged. Without one, **this is only safe on a network you fully trust** — any host on it can announce itself as a peer.

Rebinding a *known* member's address is refused and logged regardless, unless `Config.AllowAddressChange` is set; a packet reusing an existing ID with a new address would otherwise redirect that member's traffic.

`dnsdiscovery` is exactly as trustworthy as the records it reads — use a resolver you control.

And as above: discovered peers join as learners, so no discovery source can change the quorum on its own. See [Discovery](#discovery).

---

## Observability

```go
import "github.com/prometheus/client_golang/prometheus"

easyraft.WithPrometheus(prometheus.DefaultRegisterer)
```

Prometheus metrics are exposed on `GET /metrics` (behind the authorization hook, if configured). The option must be set before `Start()`.

The same registerer can be passed to every group of a `Manager`: collectors are registered once per registry and each group's series carry a `group` label holding its group ID. A single-group `Store` writes an empty `group` label, so queries that ignore the label are unaffected.

---

## Option reference

| Option | Description |
|--------|-------------|
| `WithID(id)` | Node ID — required |
| `WithRaftAddr(addr)` | gRPC listen address for Raft RPCs — required |
| `WithHTTPAddr(addr)` | HTTP listen address; enables the REST API and sets the advertised URL for leader redirects |
| `WithHTTPMux(mux)` | Register EasyRaft routes on an existing mux instead of starting a dedicated server; pair with `WithHTTPAddr` for redirect advertising |
| `WithHTTPAuth(fn)` | Authorize every HTTP request with `func(*http.Request) error` |
| `WithBearerTokenAuth(token)` | Require `Authorization: Bearer <token>` inbound, and send it on outbound join/leave requests |
| `WithHTTPTLS(tlsConfig)` | Serve the HTTP API over TLS; leader redirects then use `https` |
| `WithInsecureHTTPAcknowledged()` | Serve the HTTP API without an authorization hook, on purpose; without this or a hook the node refuses to start |
| `WithDataDir(dir)` | Persistent storage directory — required |
| `WithPeers(map[NodeID]string)` | Static initial peer list |
| `WithJoinAddr(addrs...)` | HTTP address(es) of seed nodes to join on startup |
| `WithJoinAsLearner()` | Join as a non-voting learner (requires `WithJoinAddr`) |
| `WithLeaveOnStop()` | Call `RemoveServer(self)` before shutdown for graceful departure; `Stop` reports a departure that did not happen |
| `WithDiscovery(d, interval)` | Dynamic peer discovery; wires both transport and membership. Discovered peers join as learners |
| `WithDiscoveryAsVoter()` | Add discovered peers as voters instead of learners — only with an authenticated discovery source |
| `WithLeaseReads()` | Serve linearizable reads from the leader's read lease when valid; falls back to a quorum read otherwise |
| `WithSnapCount(n)` | Log entries between automatic snapshots (default 1000) |
| `WithLogger(logger)` | Custom `*slog.Logger` |
| `WithTLS(tlsConfig)` | TLS for the gRPC transport (not the HTTP API — see `WithHTTPTLS`) |
| `WithTLSFiles(cert, key, ca)` | The same, built from PEM files as a Raft mesh needs it |
| `WithPeerAuthorizer(fn)` | Bind the node ID a Raft RPC claims to the certificate that carried it |
| `WithWitness()` | This node votes and holds no data |
| `WithWitnessPeers(ids...)` | Which peers from `WithPeers` are witnesses |
| `WithCommitQuorum(n)` | Voters a write must reach to commit; `0` is a majority |
| `WithZones(zones, n)` | Failure domains, and how many a write must reach |
| `WithPreferredLeader(id)` | Node that should hold leadership when possible |
| `WithMaxClientTableSize(n)` | Bound on the exactly-once table behind `Session` |
| `WithLeaseSafetyMargin(d)` | What a lease read may assume about clocks |
| `WithProposalQueue(size, policy)` | Writes that may wait, and what happens to the next one |
| `WithOnRemoved(fn)` | Called when a committed change removes this node |
| `WithInsecureTransportAcknowledged()` | Run the gRPC transport in plaintext, on purpose; without this or `WithTLS` the node refuses to start |
| `WithPrometheus(registerer)` | Enable Prometheus metrics |
| `WithRaftTiming(tick, heartbeat, electionMin, electionMax)` | Override Raft timing (easyraft defaults: 100 ms tick/heartbeat, 1–2 s election) |

---

## Error reference

| Error | Meaning |
|-------|---------|
| `easyraft.ErrKeyNotFound` | Key does not exist in the collection |
| `easyraft.ErrKeyExists` | Key already exists (returned by `Create`) |
| `easyraft.ErrNotLeader` | This node is not the leader; retry on the leader |
| `easyraft.ErrUnauthorized` | Request carried no usable credential — the HTTP layer answers `401` |
| `easyraft.ErrForbidden` | Credential is valid but not permitted — the HTTP layer answers `403` |
| `easyraft.ErrRevisionMismatch` | A conditional write named a revision the key is no longer at — re-read and retry |
| `easyraft.ErrReservedCollection` | Request named a `__`-prefixed internal collection |
| `raft.ErrObsoleteSeqNum` | Exactly-once sequence number is below one already recorded for that client |

---

## Examples

- [`examples/ratelimiter`](../examples/ratelimiter/) — token-bucket rate limiter with time-based refill. Demonstrates the deterministic mutation pattern.
- [`examples/configsvc`](../examples/configsvc/) — distributed configuration service with SSE watch streams. Demonstrates `Upsert`, `OnChange`, and the replicated-state vs. local-subscriber pattern.
- [`examples/ledger`](../examples/ledger/) — double-entry ledger with atomic multi-collection transactions. Demonstrates `Store.Txn`, idempotent transfers via `ErrKeyExists`, and mutation-enforced invariants.
