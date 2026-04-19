# raft

A production-grade implementation of the [Raft consensus algorithm](https://raft.github.io/) in Go.

**Raft §§ implemented:** leader election, log replication, log compaction (snapshots), cluster membership changes (single-server and joint consensus), leadership transfer, pre-vote, linearizable reads (ReadIndex and clock-based lease reads), an exactly-once client protocol, multi-raft (thousands of independent groups on shared infrastructure), and automatic leader balancing across physical nodes.

```
go get github.com/brunoga/raft
```

Requires Go 1.22+.

---

## Table of contents

1. [Quick start](#quick-start)
2. [Architecture overview](#architecture-overview)
3. [State machine interface](#state-machine-interface)
4. [Node lifecycle](#node-lifecycle)
5. [Proposing commands](#proposing-commands)
6. [Exactly-once semantics (ProposeOnce)](#exactly-once-semantics-proposeonce)
7. [Linearizable reads](#linearizable-reads)
8. [Cluster membership changes](#cluster-membership-changes)
9. [Leadership transfer](#leadership-transfer)
10. [Configuration reference](#configuration-reference)
11. [Storage backends](#storage-backends)
12. [Transport backends](#transport-backends)
13. [Observability — metrics and tracing](#observability--metrics-and-tracing)
14. [Testing utilities](#testing-utilities)
15. [Advanced features](#advanced-features)
16. [Multi-Raft — thousands of groups on shared infrastructure](#multi-raft--thousands-of-groups-on-shared-infrastructure)
17. [Caveats and known limitations](#caveats-and-known-limitations)
18. [Reference implementation](#reference-implementation)

---

## Quick start

A minimal single-node cluster (useful for tests and experiments):

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/brunoga/raft"
    "github.com/brunoga/raft/storage/memstore"
    "github.com/brunoga/raft/transport/memtransport"
)

// CounterSM is a simple integer counter state machine.
type CounterSM struct{ value int64 }

func (s *CounterSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
    s.value++
    return fmt.Appendf(nil, "%d", s.value), nil
}
func (s *CounterSM) Snapshot(_ context.Context) ([]byte, error) {
    return fmt.Appendf(nil, "%d", s.value), nil
}
func (s *CounterSM) Restore(_ context.Context, _ raft.SnapshotMeta, data []byte) error {
    _, err := fmt.Sscanf(string(data), "%d", &s.value)
    return err
}

func main() {
    net := memtransport.NewNetwork()

    cfg := raft.DefaultConfig()
    cfg.ID = "n1"
    cfg.Storage = memstore.New()
    cfg.StateMachine = &CounterSM{}
    cfg.Transport = net.NewTransport("n1")
    cfg.TickInterval = 10 * time.Millisecond // drive ticks automatically

    node, err := raft.New(cfg)
    if err != nil {
        log.Fatal(err)
    }
    net.Register("n1", node)
    node.Start()
    defer node.Stop()

    // Wait for this single node to elect itself.
    ctx := context.Background()
    for node.StateSnapshot() != raft.Leader {
        time.Sleep(10 * time.Millisecond)
    }

    result, err := node.Propose(ctx, []byte("increment"))
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("counter is now:", string(result)) // "1"
}
```

---

## Architecture overview

```
┌──────────────────────────────────────────────────────────┐
│                     Your application                     │
│  StateMachine    Propose / ReadIndex / membership        │
└────────────┬───────────────────────┬─────────────────────┘
             │                       │
      ┌──────▼───────────────────────▼──────┐
      │           raft.Manager              │  ← multi-raft
      │  GroupID → *Node routing            │
      │  StartAll / StopAll / RunTicker     │
      └──────┬───────────────────────┬──────┘
             │                       │
      ┌──────▼──────┐         ┌──────▼──────┐
      │  raft.Node  │   ...   │  raft.Node  │  G groups per machine
      │  event loop │         │  event loop │
      └──────┬──────┘         └──────┬──────┘
             │   Transport RPC       │
      ┌──────▼───────────────────────▼──────┐
      │  memtransport  │  grpctransport      │
      │                │  (batched hb: O(P)) │
      └────────────────┴─────────────────────┘
             │
      ┌──────▼────────────────────────────────┐
      │  memstore (tests)                     │
      │  filestore — groups/<id>/ per group   │
      └───────────────────────────────────────┘
```

### Per-node goroutine budget

Each `*Node` runs the following persistent goroutines:

| Goroutine | Count | Purpose |
|-----------|-------|---------|
| Event loop | 1 | Processes RPCs, timers, and proposals sequentially |
| Apply loop | 1 | Delivers committed entries to `StateMachine.Apply`, manages snapshots |
| Ticker | 0 or 1 | Fires `Tick()` at `TickInterval`; absent when `TickInterval == 0` |
| Heartbeat pump | P (one per peer) | Keeps heartbeat RPCs off the event loop; size-1 channel drops redundant sends |

**Budget at scale**: with G groups and P peers per group, each physical node runs approximately `G × (2 + P)` persistent goroutines. At G = 1,000 and P = 3, that is ~5,000 goroutines — well within Go's scheduler capacity. `Manager.RunTicker` adds one additional goroutine and a pool of `min(GOMAXPROCS, G)` workers to fan-out `Tick()` across all groups in parallel.

**Practical ceiling**: Go's M:N scheduler handles 10,000+ goroutines without issue on modern hardware. Goroutine overhead typically becomes noticeable beyond ~1,000 groups per node (~5,000 goroutines at P=3). In practice, disk I/O — not goroutines — is the binding constraint at common group counts; see [Caveats](#caveats-and-known-limitations) for details.

---

## State machine interface

Implement `raft.StateMachine` to plug in your application logic:

```go
type StateMachine interface {
    // Apply is called in log-index order for every committed entry.
    // The returned bytes are delivered to the caller of Propose/ProposeOnce.
    // Apply must be deterministic: the same entry must produce the same result
    // on every node.
    Apply(ctx context.Context, entry LogEntry) (result []byte, err error)

    // Snapshot serialises the current state. Called from the apply loop
    // after crossing SnapshotThreshold; must not block the event loop.
    Snapshot(ctx context.Context) (data []byte, err error)

    // Restore replaces the entire state with a snapshot received from the
    // leader or loaded from disk on restart.
    Restore(ctx context.Context, meta SnapshotMeta, data []byte) error
}
```

Key constraints:
- **Determinism**: `Apply` must produce the same output for the same `LogEntry` on every node. Do not read wall-clock time, random numbers, or external state inside `Apply`.
- **No cross-calls**: the library serialises all three methods — `Snapshot` and `Apply` are never called concurrently.
- **The context is `stopCtx`**: it is cancelled when `Stop()` is called. Long-running `Snapshot` or `Restore` operations should honour it.
- **Log entries for config changes are filtered**: `Apply` only receives application-level entries. Internal membership-change entries are handled by the library.

---

## Node lifecycle

```go
// 1. Build config (start from the production defaults).
cfg := raft.DefaultConfig()
cfg.ID        = "node-1"
cfg.Peers     = []raft.NodeID{"node-2", "node-3"}
cfg.Storage   = store   // raft.Storage implementation
cfg.StateMachine = sm   // your StateMachine
cfg.Transport = tr      // raft.Transport implementation

// 2. Create the node (loads persisted state; does NOT start goroutines).
node, err := raft.New(cfg)

// 3. Register with the transport so inbound RPCs are routed here.
tr.Register(cfg.ID, node) // or net.Register(cfg.ID, node) for memtransport

// 4. Start background goroutines.
node.Start()

// 5. (Optional) drive ticks manually when TickInterval == 0.
//    Useful in tests for full time control.
go func() {
    t := time.NewTicker(10 * time.Millisecond)
    for range t.C {
        node.Tick()
    }
}()

// 6. Graceful shutdown.
node.Stop()
```

### Restart

Calling `raft.New` on an existing data directory automatically restores persisted
term, vote, log, and the latest snapshot. No special restart path is needed —
the node re-joins the cluster as a follower on the next heartbeat.

---

## Proposing commands

```go
// Propose submits cmd for replication and blocks until the entry is committed
// and applied. Returns the []byte result from StateMachine.Apply.
result, err := node.Propose(ctx, cmd)
```

`Propose` returns `*raft.NotLeaderError` if this node is not the leader:

```go
result, err := node.Propose(ctx, cmd)
if err != nil {
    var nle *raft.NotLeaderError
    if errors.As(err, &nle) {
        // nle.Leader is the current leader's NodeID (may be empty if unknown).
        // Redirect the client or retry on the leader.
        fmt.Println("redirect to:", nle.Leader)
    }
    // errors.Is(err, raft.ErrNotLeader) also works.
}
```

**Concurrency**: multiple goroutines may call `Propose` simultaneously. Each call
blocks independently until its specific entry is applied.

---

## Exactly-once semantics (ProposeOnce)

`ProposeOnce` adds a client-level deduplication layer on top of Raft. If a
proposal is committed but the network drops the response, retrying with the same
`(clientID, seqNum)` returns the original result without applying the command
a second time.

```go
// seqNum must be strictly increasing per clientID.
// Retrying with the same (clientID, seqNum) returns the cached result.
result, err := node.ProposeOnce(ctx, clientID, seqNum, cmd)
```

```go
// Typical client loop with retry on leader change:
var seq uint64 = 1
for {
    result, err := node.ProposeOnce(ctx, "client-42", seq, cmd)
    if err == nil {
        seq++ // advance only on success
        break
    }
    var nle *raft.NotLeaderError
    if errors.As(err, &nle) {
        connectToLeader(nle.Leader)
        continue // same seqNum — safe to retry
    }
    if errors.Is(err, raft.ErrObsoleteSeqNum) {
        // seqNum < highest seen for this client; do NOT retry with this seqNum.
        log.Fatal("bug: seqNum went backwards")
    }
    return err
}
```

**Caveats:**
- The dedup table is persisted inside every snapshot — exactly-once is maintained across leader failovers and node restarts.
- `MaxClientTableSize` (default 100 000) caps table memory. When exceeded, the least-recently-used client entry is evicted. Use unique, stable `clientID` values.
- `ErrObsoleteSeqNum` means the submitted `seqNum` is strictly less than the one already recorded for that client. Never retry with a lower `seqNum`.

---

## Linearizable reads

### ReadIndex (always safe)

```go
// ReadIndex performs a heartbeat round-trip to confirm leadership, waits for
// the local state machine to apply up to that index, then returns. After it
// returns, the state machine reflects a linearizable snapshot.
//
// On a follower, the request is forwarded to the leader, and the follower
// waits for its own state machine to catch up before returning.
if _, err := node.ReadIndex(ctx); err != nil {
    // *NotLeaderError if no leader is known.
    return err
}
// State machine is now up-to-date; serve the read.
value := sm.Get(key)
```

### ReadIndexLease (lower latency)

When the leader holds a valid clock-based lease it can answer read queries
without a heartbeat round-trip, reducing read latency to a single-node operation:

```go
if _, err := node.ReadIndexLease(ctx); err != nil {
    if errors.Is(err, raft.ErrLeaseExpired) {
        // No valid lease — fall back to the safe path.
        _, err = node.ReadIndex(ctx)
    }
    if err != nil {
        return err
    }
}
// State machine is now up-to-date; serve the read.
```

> **§8 safety note**: a newly elected leader defers all `ReadIndex` (and
> `ReadIndexLease`) requests until it has committed at least one entry in its
> own term. The no-op entry appended on election satisfies this requirement
> automatically; reads queued before it commits are held and released as soon
> as the no-op is applied.

The lease is valid for `ElectionTimeoutMin` from the instant the last barrier
heartbeat was *sent* (not when ACKs arrived). Inject a `raft.Clock` to make
lease expiry deterministic in tests:

```go
cfg.Clock = myClock // implements raft.Clock: Now() time.Time
```

> **Warning**: lease reads rely on the system clock not jumping forward by more
> than `ElectionTimeoutMin` between nodes. They are not safe under large NTP
> corrections or VM live-migration if clocks are not well-synchronised. When in
> doubt, use `ReadIndex`.

---

## Cluster membership changes

### Single-server changes

```go
// Add a new node (blocks until committed).
err := node.AddServer(ctx, "node-4")

// Remove a node (blocks until committed).
err := node.RemoveServer(ctx, "node-2")
```

Only one membership change may be in-flight at a time; concurrent calls return
`ErrConfigChangeInProgress`.

### Joint consensus (arbitrary reconfiguration)

`ReconfigureCluster` atomically replaces the entire membership set using the
two-phase joint consensus protocol (§4.3 of the Raft dissertation):

```go
// Replace {n1, n2, n3} with {n1, n3, n4, n5}.
// Phase 1: commits a joint entry (C_old ∪ C_new both required for quorum).
// Phase 2: commits a finalise entry (C_new only) — happens automatically.
// ReconfigureCluster returns after Phase 1 commits; Phase 2 completes in
// the background (or is re-driven by the next leader if a crash occurs).
err := node.ReconfigureCluster(ctx, []raft.NodeID{"n1", "n3", "n4", "n5"})
```

`newMembers` must not contain duplicates; `ReconfigureCluster` returns an
error immediately if duplicates are detected, before touching the log.

> If the leader crashes mid-reconfiguration, the new leader automatically
> re-appends the finalise entry to complete the transition.

---

## Leadership transfer

```go
// Gracefully hand off leadership to a specific peer. Blocks until this node
// steps down (the target wins an election) or the context expires.
err := node.TransferLeadership(ctx, "node-2")
```

The leader waits for the target to catch up, then sends a `TimeoutNow` RPC
that instructs it to start an election immediately (skipping pre-vote).

---

## Configuration reference

Start from `raft.DefaultConfig()` and override only what you need:

```go
cfg := raft.DefaultConfig()
cfg.ID        = "node-1"                    // required
cfg.Peers     = []raft.NodeID{"n2", "n3"}   // required (initial peers, excluding self)
cfg.Storage   = store                       // required
cfg.StateMachine = sm                       // required
cfg.Transport = tr                          // required
```

### All fields with defaults

| Field | Default | Description |
|-------|---------|-------------|
| `ElectionTimeoutMin` | `150ms` | Lower bound of the randomised election timeout. Also the read-lease duration. |
| `ElectionTimeoutMax` | `300ms` | Upper bound. Wider spread reduces split-vote probability. Must be > Min. |
| `HeartbeatInterval` | `50ms` | How often the leader sends heartbeats. Enforced constraint: `ElectionTimeoutMin ≥ 2×HeartbeatInterval`. Recommended: `HeartbeatInterval ≤ ElectionTimeoutMin / 5`. |
| `TickInterval` | `10ms` | Wall-clock period per `Tick()`. Zero means manual ticks (recommended for tests). |
| `MaxLogEntriesPerRPC` | `64` | Maximum entries per AppendEntries RPC. |
| `MaxInflightRPCs` | `4` | Per-peer pipeline depth (concurrent unacknowledged AppendEntries RPCs). |
| `SnapshotThreshold` | `10000` | Entries past the last snapshot that trigger an automatic snapshot. `0` disables auto-snapshots. |
| `SnapshotChunkSize` | `4 MiB` | Maximum bytes per InstallSnapshot chunk. `0` sends snapshots as a single RPC. |
| `RPCTimeout` | `0` | Per-RPC deadline. `0` falls back to `ElectionTimeoutMin`. Snapshot RPCs use `4×RPCTimeout`. |
| `CheckQuorum` | `true` | Leader steps down if it doesn't hear from a quorum within one election timeout. |
| `MaxClientTableSize` | `100000` | Maximum entries in the ProposeOnce dedup table. `0` disables eviction. |
| `Logger` | `slog.Default()` | Structured logger. Set to a `slog.LevelWarn` logger to silence routine traffic. |
| `Metrics` | `nil` | Observability hook (see [Metrics and tracing](#observability--metrics-and-tracing)). |
| `Tracer` | `nil` | Per-RPC tracing hook. |
| `Clock` | `time.Now` | Injectable clock for lease reads. |
| `PreferredLeader` | `""` | NodeID that should hold leadership whenever possible. Any node that wins an election but is not the preferred node will automatically transfer leadership to it once stable. Empty means no preference. |

### Tick-based timing

All timeouts are expressed as wall-clock durations and converted internally to
logical tick counts using `TickInterval`. Setting `TickInterval = 0` gives your
tests full time control:

```go
cfg.TickInterval = 0 // manual ticks only

// In tests: advance time precisely.
for i := 0; i < 20; i++ {
    node.Tick()
}
```

---

## Storage backends

### `storage/memstore` — in-memory (tests only)

```go
import "github.com/brunoga/raft/storage/memstore"

store := memstore.New()
```

Non-durable. Data is lost on process exit. Suitable for unit tests and simulations.

### `storage/filestore` — file-backed (production)

```go
import "github.com/brunoga/raft/storage/filestore"

store, err := filestore.Open("/var/lib/myapp/raft")
defer store.Close()
```

Features:
- **CRC32 checksums** on every log entry.
- **fsync before return** on all mutating operations — safe across crashes.
- **Automatic segment rotation** at 64 MiB (configurable via `OpenWithSegmentSize`).
- **Crash recovery**: on open, the tail of the last segment is scanned and any partial write is truncated.
- **Context-aware**: pre-flight `ctx.Err()` checks at entry and between write/sync steps prevent starting new I/O when the node is shutting down.

```go
// For tests: use small segments to exercise rotation.
store, err := filestore.OpenWithSegmentSize("/tmp/test-raft", 4096)
```

**On-disk layout:**
```
/var/lib/myapp/raft/
  meta              — term + votedFor (fixed 266 bytes, overwritten in-place)
  seg-00000.log     — first log segment (binary, CRC32-protected entries)
  seg-00000.idx     — dense array of uint64 byte offsets (one per entry)
  seg-00001.log     — second segment (created after rotation)
  seg-00001.idx
  snap              — latest snapshot (written atomically via snap.tmp)
  seg-NNNNN.log.tmp — transient: new log content during log truncation; renamed
  seg-NNNNN.idx.tmp   atomically then cleaned up; both are removed on next open
                       if a crash interrupted the rename sequence
```

---

## Transport backends

### `transport/memtransport` — in-process (tests)

```go
import "github.com/brunoga/raft/transport/memtransport"

net := memtransport.NewNetwork()

// Create a transport for each node.
tr1 := net.NewTransport("n1")
tr2 := net.NewTransport("n2")

// Register nodes after calling node.Start().
net.Register("n1", node1)
net.Register("n2", node2)

// Inject network faults.
net.Drop("n1", "n2")      // drop all messages from n1 to n2
net.Restore("n1", "n2")   // restore that link
net.Partition("n3")        // isolate n3 from everyone
net.Heal("n3")             // reconnect n3
```

### `transport/grpctransport` — gRPC over TCP (production)

```go
import "github.com/brunoga/raft/transport/grpctransport"

tr, err := grpctransport.Listen(":7001")
defer tr.Close()

// Register peers before starting the node.
tr.AddPeer("n2", "10.0.0.2:7001")
tr.AddPeer("n3", "10.0.0.3:7001")
```

In multi-raft deployments, install `Manager.Lookup` as the group-lookup
function so inbound RPCs are routed to the correct group by the `GroupID`
field embedded in every proto message:

```go
mgr := raft.NewManager()
// ... add nodes to mgr ...
tr.SetGroupLookup(mgr.Lookup)
```

`SetGroupLookup` also enables **heartbeat batching** — see [Multi-Raft](#multi-raft--thousands-of-groups-on-shared-infrastructure).

**TLS / mTLS** via `WithTLSConfig`:

```go
// tlsCfg should have Certificates, RootCAs/ClientCAs, and ClientAuth set.
tr, err := grpctransport.Listen(":7001",
    grpctransport.WithTLSConfig(tlsCfg),
)
```

`WithTLSConfig` applies the same `*tls.Config` to both the server listener and
all outbound client connections. Pass separate configs to
`WithServerOptions(grpc.Creds(...))` / `WithDialOptions(grpc.WithTransportCredentials(...))`
if server and client configs must differ.

Default keepalive settings are applied automatically; override them the same way.

---

## Observability — metrics and tracing

### Metrics interface

```go
type Metrics interface {
    StateChange(id NodeID, from, to State, term Term)
    CommitAdvanced(id NodeID, commitIndex Index)
    SnapshotTaken(id NodeID, lastIncludedIndex Index, sizeBytes int)
}
```

Implementations must **not block** — they are called synchronously from the event loop.

### Prometheus (`metrics/prommetrics`)

```go
import "github.com/brunoga/raft/metrics/prommetrics"

m := prommetrics.New() // registers metrics on prometheus.DefaultRegisterer
cfg.Metrics = m
```

Exported metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `raft_node_state` | Gauge | Current state (0=Follower, 1=Candidate, 2=Leader) |
| `raft_current_term` | Gauge | Current term |
| `raft_state_transitions_total` | Counter | State transitions, labelled by from/to |
| `raft_commit_index` | Gauge | Highest committed log index |
| `raft_snapshots_total` | Counter | Snapshots taken |
| `raft_snapshot_size_bytes` | Histogram | Snapshot payload sizes |
| `raft_commits_total` | Counter | Total entries committed |

### RPC tracer (`metrics/rpctracer`)

```go
import "github.com/brunoga/raft/metrics/rpctracer"

cfg.Tracer = rpctracer.NewSlogTracer(logger)
// Logs Debug on success, Warn on failure for every outbound RPC.
```

Implement `raft.Tracer` directly for OpenTelemetry or custom tracing:

```go
type Tracer interface {
    // StartRPC is called before each outbound RPC.
    // The returned finish func is called with the RPC error (nil on success).
    StartRPC(nodeID NodeID, peer NodeID, rpcType string) (finish func(err error))
}
```

---

## Testing utilities

### In-process cluster with fault injection

```go
net := memtransport.NewNetwork()
nodes := make([]*raft.Node, 3)
ids := []raft.NodeID{"n1", "n2", "n3"}

for i, id := range ids {
    peers := slices.DeleteFunc(slices.Clone(ids), func(p raft.NodeID) bool { return p == id })
    cfg := raft.DefaultConfig()
    cfg.ID = id
    cfg.Peers = peers
    cfg.Storage = memstore.New()
    cfg.StateMachine = &MySM{}
    cfg.Transport = net.NewTransport(id)
    cfg.TickInterval = 0 // manual ticks
    nodes[i], _ = raft.New(cfg)
    net.Register(id, nodes[i])
    nodes[i].Start()
}

tick := func() {
    for _, n := range nodes {
        n.Tick()
    }
}

// Advance time manually for deterministic tests.
for i := 0; i < 50; i++ { tick() }

// Inject a partition.
net.Partition("n3")
for i := 0; i < 100; i++ { tick() }
net.Heal("n3")
```

### Manual tick mode for deterministic time

Setting `TickInterval = 0` gives tests complete control over logical time.
All timeouts (election, heartbeat) are converted to tick counts once at
`raft.New()` time using a 10 ms reference period when `TickInterval` is
zero. For example, with `ElectionTimeoutMin = 150ms`, an election fires
after ~15 `Tick()` calls. The exact count is deterministic and
wall-clock-independent.

### Benchmarks

Run the included benchmarks:

```
go test -bench=. -benchmem ./
```

Reference results on a modern laptop:

| Benchmark | Throughput | Allocations |
|-----------|-----------|-------------|
| `BenchmarkTick` | ~2.1 µs/op | 4 allocs/op |
| `BenchmarkPropose_SingleNode` | ~1.7 µs/op | 5 allocs/op |
| `BenchmarkPropose_ThreeNode` | ~1.1 ms/op | 66 allocs/op |
| `BenchmarkPropose_Pipelined` | ~96 µs/batch | 561 allocs/batch |

---

## Advanced features

### Pre-vote (always enabled)

Before incrementing its term and starting an election, a follower first runs a
pre-vote round. The pre-vote request does not increment the term; it only asks
peers whether they *would* grant a vote. This prevents isolated or partitioned
nodes from disrupting the cluster by bumping the term unnecessarily.

Pre-vote receiver logic: a node rejects a pre-vote if it has heard from a
leader within the last election timeout, preventing a healthy cluster from
being disrupted by a rejoining node.

### Check-quorum (default: enabled)

When `CheckQuorum = true` (the default), a leader tracks successful
`AppendEntries` responses from peers. If it does not hear from a quorum within
one election timeout it steps down as a follower. This prevents a partitioned
leader from indefinitely blocking reads and proposals that can never commit.

### Log compaction and chunked snapshot transfer

Snapshots are triggered automatically when:
```
lastApplied − lastSnapshotIndex ≥ SnapshotThreshold
```

Large snapshots are split into `SnapshotChunkSize`-byte chunks and sent as
sequential RPCs. The follower buffers chunks and applies the snapshot atomically
on the final chunk. A partial transfer is discarded if the follower changes
leaders.

To disable automatic snapshots and manage compaction yourself:

```go
cfg.SnapshotThreshold = 0 // disable; call Propose with compaction commands manually
```

### Exactly-once snapshot consistency

The client dedup table is deep-copied at snapshot time and bundled with the
state-machine data inside every snapshot. This ensures that after a restore,
the dedup state is exactly consistent with the restored SM state — no commands
committed before the snapshot can be double-applied.

### Backpressure and pipelining

`MaxInflightRPCs` (default 4) caps the number of concurrent unacknowledged
`AppendEntries` RPCs per follower. This keeps the network pipe full under high
latency while bounding peak memory usage per peer.

### Per-peer heartbeat write-pump

Heartbeats use a persistent goroutine per peer rather than spawning a new
goroutine on every tick. The pump uses a size-1 channel; if the previous
heartbeat hasn't been sent yet, the newer one replaces it (a missed heartbeat
only delays follower timer resets — it does not affect safety). Replication
RPCs continue to use per-goroutine sends to preserve pipelining.

---

## Multi-Raft — thousands of groups on shared infrastructure

`raft.Manager` multiplexes independent Raft groups on a single physical node.
The typical use-case is a sharded database where each shard is its own Raft
group, and each machine hosts one replica per shard.

```
Physical node A          Physical node B          Physical node C
┌──────────────┐         ┌──────────────┐         ┌──────────────┐
│ Manager      │         │ Manager      │         │ Manager      │
│  group 1 (A) │◄───────►│  group 1 (B) │◄───────►│  group 1 (C) │  shard 1
│  group 2 (A) │◄───────►│  group 2 (B) │◄───────►│  group 2 (C) │  shard 2
│  group 3 (A) │◄───────►│  group 3 (B) │◄───────►│  group 3 (C) │  shard 3
│  …           │         │  …           │         │  …           │
└──────────────┘         └──────────────┘         └──────────────┘
```

### Setting up a Manager

```go
mgr := raft.NewManager()

// Construct one Node per group. Every group must have a unique GroupID.
for _, shard := range shards {
    cfg := raft.DefaultConfig()
    cfg.GroupID = shard.ID                          // non-zero uint64
    cfg.ID      = myNodeID
    cfg.Peers   = peersForShard(shard.ID)
    cfg.Storage = filestore.Open(fmt.Sprintf("data/groups/%d", shard.ID))
    cfg.StateMachine = newShardSM(shard)
    cfg.Transport = tr                              // shared transport
    node, _ := raft.New(cfg)
    tr.Register(myNodeID, node)                     // single-group: still needed
    mgr.Add(shard.ID, node)
}

// Install the group-lookup function so inbound RPCs route by GroupID.
tr.SetGroupLookup(mgr.Lookup)

// Start all nodes and drive their tick clocks with one shared goroutine.
mgr.StartAll()
defer mgr.StopAll()

ctx, cancel := context.WithCancel(context.Background())
defer cancel()
go mgr.RunTicker(ctx, 10*time.Millisecond)
```

### Storage partitioning convention

Each group must have its own storage directory to avoid log and snapshot
collisions:

```
/var/lib/myapp/raft/
  groups/
    1/      ← FileStore for group 1
    2/      ← FileStore for group 2
    …
```

```go
store, err := filestore.Open(fmt.Sprintf("/var/lib/myapp/raft/groups/%d", groupID))
```

### Heartbeat batching

Without batching, G groups × P peers = G×P `AppendEntries` RPCs per tick
interval. At 1,000 groups × 3 peers that is 3,000 RPCs every 10 ms — a
significant fraction of a node's bandwidth.

`GRPCTransport` automatically batches all pure heartbeats (regular + read-barrier)
to the same peer into a single `BatchHeartbeats` RPC, reducing the cost to O(P)
per interval. Batching is enabled by `SetGroupLookup`; single-group deployments
are unaffected.

```
Before: 1,000 groups × 3 peers = 3,000 AppendEntries RPCs per tick
After:                            3 BatchHeartbeats RPCs per tick
```

The batcher opens a 1 ms collection window after the first heartbeat arrives so
that all groups' heartbeats (which fire together via `RunTicker`) are coalesced
before the RPC is sent. `BatchHeartbeatsServed()` on the receiving transport
returns the cumulative count for monitoring.

### Industry context

This pattern — multiple independent Raft groups co-located on each physical
node, sharing a single transport — is what the distributed-systems community
canonically refers to as **multi-raft**. The same architecture is used in:

- **CockroachDB**: each *range* (16 MiB key-range shard) is a Raft group; nodes
  host hundreds of ranges. CockroachDB coined the term *coalesced heartbeats*
  for the same O(G×P) → O(P) optimisation implemented here as heartbeat
  batching.
- **TiKV** (powers TiDB): each *region* is a Raft group; TiKV's `MultiRaft`
  component is structurally identical to `Manager`. PingCAP uses the term
  "multi-raft" explicitly in their documentation.

**Resource isolation caveat.** Co-locating groups shares CPU, disk I/O, and
network bandwidth — a write-heavy shard can delay heartbeats or slow proposals
in neighbouring groups on the same machine. Multi-raft addresses
*Raft-protocol-level* interference (independent logs, independent elections,
independent apply pipelines) but not hardware-resource contention. Production
systems address this through per-shard I/O throttling, hot-shard detection,
and partition migration — layers built above the Raft library. See
[`examples/shardkv`](../examples/shardkv/) for a working end-to-end example
and its README for a fuller discussion of the trade-offs.

### Scale boundaries

**Practical group counts**: at 10–200 groups per node the implementation runs comfortably within both goroutine and I/O budgets on typical SSD hardware. Beyond ~500 actively-writing groups, disk throughput becomes the binding constraint rather than CPU or goroutines.

**fsync amplification (filestore)**: `filestore` issues an `fsync` after every mutating operation on each group's storage. Under write load, G simultaneously-active groups can issue G fsyncs within a single tick window. On a fast NVMe device (≈200 µs per fsync), 500 concurrent fsyncs consume roughly 100 ms of disk time — enough to trigger election timeouts in groups that are waiting on their own storage. Mitigation options:

- **Stagger write load**: spread groups so that only a fraction are actively receiving proposals at any instant. Read-heavy or idle groups do not amplify fsyncs.
- **Use a shared-WAL storage backend**: the `Storage` interface is intentionally narrow (`SaveLog`, `SaveSnapshot`, `LoadLog`, `LoadSnapshot`). A production system at very high group counts should replace `filestore` with an implementation that batches writes from multiple groups into a single shared WAL and issues one `fsync` per batch. `filestore` is the reference implementation for correctness and single-group deployments, not for a 1,000-group write-heavy cluster.
- **Use `memstore` for recoverable groups**: groups whose data can be rebuilt from an external source of truth (e.g. a sharded RDBMS) can use `memstore` without durability concerns.

**No inter-group flow control**: all groups share the same gRPC connection(s) to each peer. A group under heavy replication load (large log entries, frequent snapshot installs) can consume a disproportionate share of the shared TCP bandwidth and delay heartbeats from other groups, triggering unnecessary elections. HTTP/2 multiplexing prevents TCP head-of-line blocking, but the library does not implement application-level priority scheduling or bandwidth allocation between groups.

### Observing group status

```go
statuses := mgr.StatusAll()  // []GroupStatus — point-in-time snapshot
for _, s := range statuses {
    fmt.Printf("group=%d node=%s state=%s term=%d lastApplied=%d\n",
        s.GroupID, s.NodeID, s.State, s.Term, s.LastApplied)
}
```

### Leader balancing across physical nodes

In a multi-raft deployment, Raft elections are independent per group: after a
rolling restart or a series of failovers all leaders can pile up on the same
machine, creating a hot node. `BalanceController` periodically collects the
global leader distribution from every physical node and transfers leaders from
the most-loaded node to the least-loaded until all counts differ by at most 1.

**In-process** (single binary, all managers reachable directly):

```go
providers := map[raft.NodeID]raft.NodeProvider{
    "node-A": mgrA,
    "node-B": mgrB,
    "node-C": mgrC,
}
ctrl := raft.NewBalanceController(providers, raft.LeastLeadersBalancer{}, 30*time.Second)
go ctrl.Run(ctx)
```

**Across machines** (each node exposes `Manager.Handler()` over HTTP):

```go
// On each physical node, mount the manager's balance endpoints.
mux.Handle("/raft/", http.StripPrefix("/raft", mgr.Handler()))

// On each node, build the provider map using HTTPNodeProvider for peers
// and the local Manager directly for self.
providers := map[raft.NodeID]raft.NodeProvider{
    "node-A": mgr,  // local: in-process
    "node-B": raft.NewHTTPNodeProvider("http://node-b:8001/raft", nil),
    "node-C": raft.NewHTTPNodeProvider("http://node-c:8001/raft", nil),
}
ctrl := raft.NewBalanceController(providers, raft.LeastLeadersBalancer{}, 30*time.Second)
go ctrl.Run(ctx)
```

`Manager.Handler()` exposes two endpoints:

| Endpoint | Description |
|----------|-------------|
| `GET /status` | Returns `StatusAll()` as a JSON array of `GroupStatus` |
| `POST /transfer` | Accepts `{"group_id": N, "to": "nodeID"}` and calls `TransferGroupLeadership` |

See [`examples/shardkv`](examples/shardkv/) for a complete cross-machine wiring.

---

## Caveats and known limitations

### Lease reads require well-synchronised clocks

`ReadIndexLease` assumes that the system clock does not jump forward by more
than `ElectionTimeoutMin` relative to peer clocks. Large NTP corrections or VM
live-migration with unsynchronised clocks can cause stale reads. Use `ReadIndex`
when strong linearizability is required regardless of clock quality.

### Storage methods check context but cannot interrupt in-progress syscalls

The filestore checks `ctx.Err()` at the start of every method and between
write and sync steps. However, once a `file.Sync()` (fsync) syscall has been
issued, it cannot be interrupted at the OS level. On NFS or network-attached
block storage, a hung fsync will block the event loop until the OS returns.
Use local SSDs in production for predictable latency.

### Single-node clusters and `CheckQuorum`

In a single-node cluster `Peers` is empty, so `CheckQuorum` is a no-op
(the leader is always its own quorum). No special configuration is needed.

### `Config.Peers` is mutated in-place

The event loop updates `cfg.Peers` as `AddServer`/`RemoveServer` entries are
applied. Do not read or write `cfg.Peers` after calling `Start()`.

### `ErrObsoleteSeqNum` must not be retried

If `ProposeOnce` returns `ErrObsoleteSeqNum`, the supplied `seqNum` is strictly
less than the highest seen for that client. Retrying with the same `seqNum`
will never succeed. Advance `seqNum` and retry with the new value, or
investigate why the sequence number went backwards.

### Removing a leader

`RemoveServer(ctx, leaderID)` is safe — the leader commits the removal entry,
then steps down as a follower. The remaining peers elect a new leader. However,
the removed leader does not call `Stop()` automatically; the caller is
responsible for shutting it down after the removal is committed.

For leader self-removal as part of a larger reconfiguration, prefer
`ReconfigureCluster` — it uses joint consensus, which keeps a quorum available
throughout the two-phase transition and avoids the brief window where the
single-server change could leave the cluster without a quorum.

### No dynamic `TickInterval` changes

`TickInterval` is read once in `New()` and converted to tick counts. Changing it
after `Start()` has no effect.

### fsync amplification with many groups

When using `filestore` with many simultaneously-active groups, each group issues its own `fsync` on every log append. G concurrent writers produce up to G fsyncs per replication round. On NVMe storage this is usually acceptable up to ~100–200 concurrent writers; on network-attached or spinning storage the accumulated latency spikes will cause election timeouts well below that threshold. For write-heavy deployments above ~200 groups, use a shared-WAL `Storage` implementation that amortises fsyncs across groups. See [Scale boundaries](#scale-boundaries) in the Multi-Raft section.

### No proposal-level backpressure

The internal `proposeCh` has a fixed capacity of 1,024 entries. When the leader's event loop falls behind — due to a slow state machine, a long-running snapshot, or heavy replication traffic — `Propose` blocks at channel entry until space becomes available. There is no admission-control mechanism that sheds load or returns an error proactively. Applications that need to bound proposal queue depth should implement their own semaphore or token-bucket before calling `Propose`.

---

## Reference implementation

Two fully-worked examples are provided, each targeting a different deployment pattern.

### `examples/idprovider` — single-group

See [`examples/idprovider`](examples/idprovider/) for a production-ready distributed
monotonic ID allocation service that demonstrates all major single-group features:

- **State machine**: multiple independent domains, each with a `uint64`
  counter; each allocation atomically reserves a range `[start, start+count)`.
- **Domain management**: `POST /domains/{name}` to create, `DELETE` to remove,
  `GET /domains` for a linearizable listing of all domains and their counters.
- **Exactly-once allocation**: clients supply `X-Client-ID` and `X-Seq-Num`
  headers; retrying with the same pair returns the original range.
- **Linearizable reads**: `GET /domains/{name}/current` uses `ReadIndex`.
- **Stale reads**: append `?consistency=stale` to any read endpoint to bypass
  the leader round-trip and serve directly from the local state machine.
- **Leader routing**: non-leader nodes return `503` with `X-Raft-Leader`.
- **gRPC transport** + **filestore** + graceful shutdown.

```bash
go build -o idprovider ./examples/idprovider
./idprovider --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/n1 \
             --peer n2=localhost:7002 --peer n3=localhost:7003
```

### `examples/shardkv` — multi-group (multi-raft) with leader balancing

See [`examples/shardkv`](examples/shardkv/) for a horizontally-sharded key-value
store that demonstrates the canonical multi-raft pattern: N independent Raft
groups (shards) co-located on each physical node, sharing one gRPC transport and
one `Manager` ticker, with a `BalanceController` distributing leaders evenly
across machines.

- **Multi-raft wiring**: `Manager`, `cfg.GroupID`, `tr.SetGroupLookup`, `RunTicker`.
- **FNV-hash routing**: keys are deterministically mapped to shards; the HTTP
  layer resolves the correct group and redirects writes to the shard leader.
- **Heartbeat batching**: enabled automatically by `SetGroupLookup` — O(G×P)
  heartbeats per tick are coalesced into O(P) `BatchHeartbeats` RPCs.
- **Independent fault domains**: stopping or partitioning one shard's nodes does
  not affect other shards' availability or leadership.
- **Cross-machine leader balancing**: a `BalanceController` on each node uses
  `HTTPNodeProvider` to query peers' `/raft/status` endpoints and drives
  leadership transfers to maintain ±1 leader balance across physical nodes.

```bash
go build -o shardkv ./examples/shardkv

# Node 1 — hosts one replica of each of the 4 shards
./shardkv --id p1 --shards 4 --raft-addr :7001 --http-addr :8001 \
          --data-dir /tmp/sk/p1 \
          --peer p2=localhost:7002,localhost:8002 \
          --peer p3=localhost:7003,localhost:8003

# Node 2 and 3 follow the same pattern (see examples/shardkv/README.md)
```
