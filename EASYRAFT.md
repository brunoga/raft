# EasyRaft — Design Document

EasyRaft is a higher-level API built on top of the `raft` package. Its goal is
to let a developer build a strongly-consistent, replicated service by describing
**what** data to replicate — not **how** to replicate it.

The entire Raft plumbing (transport, storage, state machine, snapshots, peer
discovery, leader routing) is handled automatically. The developer works only
with their domain types.

---

## Motivation

A minimal three-node idprovider built directly on `raft` requires roughly:

- A hand-written `StateMachine` with `Apply`, `Snapshot`, and `Restore`
- Command encoding/decoding (JSON, protobuf, etc.)
- Transport setup (`grpctransport.Listen`, `AddPeer` for each peer)
- Storage setup (`filestore.Open`)
- Node construction and wiring (`raft.New`, `node.Start`)
- Peer discovery plumbing (`DiscoveryAgent`, `raftPeerAdder`)
- `NotLeaderError` detection and leader-redirect logic
- Snapshot threshold and tick-interval tuning

EasyRaft makes all of that an implementation detail.

---

## Two flavours

Both flavours manage **multiple values** of each type, keyed by a string.
The difference is whether the service manages one type or many.

| | `EasyRaft[T]` | `Store` + `Collection[T]` |
|---|---|---|
| Number of types | One | Many |
| State shape | `map[string]T` | `map[name]map[string]T` |
| Raft node(s) | One | One (shared) |
| Typical use | Single-domain service | Multi-entity service |
| idprovider equivalent | `EasyRaft[Counter]` | `AddCollection[Counter](store, "counters")` |

---

## Single-type: `EasyRaft[T]`

### Concept

`EasyRaft[T]` manages a replicated collection of `T` values. The state is
implicitly `map[string]T`; the developer never declares it. Items are
identified by a string key chosen by the caller.

`T` must be JSON-serializable. The framework handles command encoding,
snapshotting, and restore automatically.

### API sketch

```go
// Construction
er, err := easyraft.New[Counter](
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithHTTPAddr(":8001"),   // optional — enables auto HTTP endpoints
    easyraft.WithDataDir("/tmp/n1"),
    easyraft.WithPeers("n2=:7002", "n3=:7003"),
    // or: easyraft.WithUDPDiscovery()
    // or: easyraft.WithDNSDiscovery("svc.cluster.local")
)
er.Start()
defer er.Stop()

// Writes (proposals — must reach the leader)
err      = er.Create(ctx, "orders", Counter{})
err      = er.Update(ctx, "orders", Counter{Value: 42})
err      = er.Delete(ctx, "orders")

// Atomic read-modify-write (single proposal, no race window)
result, err := er.Mutate(ctx, "orders", func(c *Counter) (any, error) {
    start   := c.Value
    c.Value += count
    return start, nil
})

// Reads
c, err  := er.Read(ctx, "orders")   // linearizable (ReadIndex barrier)
all, err := er.List(ctx)             // linearizable
c        = er.ReadStale("orders")   // local, no leader round-trip; may lag
```

### CRUD semantics

| Operation | Linearizable | Notes |
|-----------|-------------|-------|
| `Create`  | yes | Returns `ErrKeyExists` if key already present |
| `Read`    | yes | Returns `ErrKeyNotFound` if absent |
| `Update`  | yes | Returns `ErrKeyNotFound` if absent |
| `Delete`  | yes | Returns `ErrKeyNotFound` if absent |
| `List`    | yes | Returns a copy of the full collection |
| `Mutate`  | yes | Atomic read-modify-write; `f` must be pure |
| `ReadStale` | no | Reads local state; may lag by ≤ one heartbeat |

### `Mutate` — why it's necessary

`Read` followed by `Update` is **not atomic**: another node can interleave
between the two calls. `Mutate` encodes the entire read-modify-write as a
single log entry so it commits as one unit. This is required for any operation
where the new value depends on the current value (counters, queues, accumulators,
CAS-style updates).

The function `f` is called by the state machine during `Apply`; it must be
deterministic and free of side effects.

### What gets abstracted away

- `StateMachine` implementation (Apply, Snapshot, Restore)
- Command struct definition and JSON encoding
- `filestore.Open` and `raft.Config` wiring
- `grpctransport.Listen` and `tr.AddPeer` calls
- `raft.New` / `node.Start` / `node.Stop`
- `DiscoveryAgent` + `raftPeerAdder` plumbing
- `NotLeaderError` detection (surfaces as `easyraft.ErrNotLeader`)
- Snapshot threshold defaults

### Optional HTTP layer

When `WithHTTPAddr` is provided, EasyRaft mounts the following endpoints
automatically:

```
POST   /items/{key}            Create
GET    /items/{key}            Read  (add ?consistency=stale for stale read)
PUT    /items/{key}            Update
DELETE /items/{key}            Delete
GET    /items                  List  (add ?consistency=stale)
POST   /items/{key}/mutate     Mutate (body: the mutation command, TBD)
GET    /status                 Node state, leader, last applied index
```

Non-leader nodes return `307 Temporary Redirect` to the leader when the
leader's HTTP address is known. The path prefix (`/items`) is configurable.

---

## Multi-type: `Store` + `Collection[T]`

### Concept

When a service manages several distinct entity types (e.g. `User`, `Session`,
`Config`), a single `Store` owns one Raft node and one set of resources.
Each `Collection[T]` is a typed, namespaced view into the shared state. The
collection name acts as the namespace; keys within a collection are independent
of keys in other collections.

`EasyRaft[T]` is a thin convenience wrapper over a `Store` with a single
registered collection.

### API sketch

```go
// One Raft node for the whole service.
store, err := easyraft.NewStore(
    easyraft.WithID("n1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithHTTPAddr(":8001"),
    easyraft.WithDataDir("/tmp/n1"),
    easyraft.WithPeers("n2=:7002", "n3=:7003"),
)

// Typed collections — registered before Start.
users    := easyraft.AddCollection[User](store, "users")
sessions := easyraft.AddCollection[Session](store, "sessions")
cfg      := easyraft.AddCollection[Config](store, "config")

store.Start()
defer store.Stop()

// Each collection has the same CRUD + Mutate API as EasyRaft[T].
err          = users.Create(ctx, "alice", User{Name: "Alice"})
u, err      := users.Read(ctx, "alice")
err          = sessions.Delete(ctx, "sess-xyz")
all, err    := cfg.List(ctx)
```

### Internals

The shared state machine holds:

```
map[collectionName string] map[key string] json.RawMessage
```

Every command encodes `{collection, op, key, value}`. The state machine
dispatches by collection name and stores/retrieves raw JSON. Each
`Collection[T]` marshals and unmarshals at the API boundary, so the store
core is type-agnostic.

Snapshotting serializes the full nested map. Restore deserializes it. No
per-collection snapshot logic is needed.

### HTTP layer

When `WithHTTPAddr` is provided, each registered collection gets its own
endpoint group automatically:

```
POST   /{collection}/{key}
GET    /{collection}/{key}[?consistency=stale]
PUT    /{collection}/{key}
DELETE /{collection}/{key}
GET    /{collection}[?consistency=stale]
GET    /status
```

---

## Configuration options

Both `New[T]` and `NewStore` accept the same option set:

| Option | Description |
|--------|-------------|
| `WithID(id)` | Node ID (required) |
| `WithRaftAddr(addr)` | gRPC listen address for Raft RPCs |
| `WithHTTPAddr(addr)` | HTTP listen address (enables auto endpoints) |
| `WithDataDir(dir)` | Directory for persistent log and snapshots |
| `WithPeers(peers...)` | Static peer list in `id=addr` form |
| `WithUDPDiscovery(opts...)` | UDP broadcast peer discovery |
| `WithDNSDiscovery(hostname)` | DNS A-record peer discovery |
| `WithTLS(cert, key, ca)` | mTLS for all Raft RPCs |
| `WithSnapshotThreshold(n)` | Entries between snapshots (default 1000) |
| `WithLogger(logger)` | Custom `*slog.Logger` |
| `WithPreferredLeader(id)` | Pin leadership to a specific node |

---

## Limitations and future work

**Cross-collection atomicity**: in `Store`, operations on different collections
are independent Raft proposals. An operation that must atomically touch both
`users` and `sessions` cannot be expressed with the current API. A future
`store.Txn(ctx, func(tx *Txn) error)` could batch multiple mutations into one
log entry.

**Key type**: keys are strings. A generic key type `K comparable` is possible
but complicates HTTP routing and is deferred.

**Secondary indexes / queries**: the framework manages a flat `map[key]T`.
Querying by field value (e.g. "all users with role=admin") requires the caller
to `List` and filter client-side. Index maintenance inside `Mutate` could be
added later.

**Exactly-once dedup**: the underlying `raft` package supports `ProposeOnce`
with client-ID / seq-num dedup. EasyRaft v1 does not expose this. Callers that
need idempotent retries should use `Mutate` (which is naturally idempotent when
`f` is pure) or manage seq-nums themselves.

---

## Implementation phases

### Phase 1 — `EasyRaft[T]` core
- `New[T]`, `Start`, `Stop`
- `Create`, `Read`, `Update`, `Delete`, `List`, `ReadStale`
- `Mutate`
- Static peer configuration (`WithPeers`)
- JSON serialization for commands and snapshots
- `filestore` + `grpctransport` wiring

### Phase 2 — Discovery and TLS
- `WithUDPDiscovery`, `WithDNSDiscovery`
- `WithTLS`

### Phase 3 — HTTP layer
- Auto-generated endpoints via `WithHTTPAddr`
- `307` redirect to leader
- `?consistency=stale` support

### Phase 4 — `Store` + `Collection[T]`
- `NewStore`, `AddCollection[T]`
- Multi-namespace state machine
- Per-collection HTTP endpoint groups

### Phase 5 — Future
- Cross-collection transactions (`Txn`)
- Generic key type `K`
- Exactly-once dedup surface
