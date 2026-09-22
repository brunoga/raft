# raft

A production-grade implementation of the [Raft consensus algorithm](https://raft.github.io/) in Go.

**Raft §§ implemented:** leader election, log replication, log compaction (snapshots), cluster membership changes (single-server and joint consensus), leadership transfer, pre-vote, linearizable reads (ReadIndex and clock-based lease reads), an exactly-once client protocol, multi-raft (thousands of independent groups on shared infrastructure), and automatic leader balancing across physical nodes.

```
go get github.com/brunoga/raft/v2
```

Requires Go 1.26 or newer.

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
10. [Reacting to leadership changes](#reacting-to-leadership-changes)
11. [Configuration reference](#configuration-reference)
12. [Storage backends](#storage-backends)
13. [Transport backends](#transport-backends)
14. [Observability — metrics and tracing](#observability--metrics-and-tracing)
15. [Testing utilities](#testing-utilities)
16. [Advanced features](#advanced-features)
17. [Multi-Raft — thousands of groups on shared infrastructure](#multi-raft--thousands-of-groups-on-shared-infrastructure)
18. [Caveats and known limitations](#caveats-and-known-limitations)
19. [EasyRaft — high-level abstraction](#easyraft--high-level-abstraction)
20. [Reference implementation](#reference-implementation)
21. [Disaster recovery from permanent quorum loss](#disaster-recovery-from-permanent-quorum-loss)

---

## Quick start

A minimal single-node cluster (useful for tests and experiments):

```go
package main

import (
    "context"
    "fmt"
    "io"
    "log"
    "time"

    "github.com/brunoga/raft/v2"
    "github.com/brunoga/raft/v2/storage/memstore"
    "github.com/brunoga/raft/v2/transport/memtransport"
)

// CounterSM is a simple integer counter state machine.
type CounterSM struct{ value int64 }

func (s *CounterSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
    s.value++
    return fmt.Appendf(nil, "%d", s.value), nil
}
func (s *CounterSM) Snapshot(_ context.Context, w io.Writer) error {
    _, err := fmt.Fprintf(w, "%d", s.value)
    return err
}
func (s *CounterSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
    _, err := fmt.Fscanf(r, "%d", &s.value)
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

    node, err := raft.New(&cfg)
    if err != nil {
        log.Fatal(err)
    }
    net.Register("n1", node.Handler())
    node.Start()
    defer node.Stop()

    // Wait for this single node to elect itself.
    ctx := context.Background()
    for node.State() != raft.Leader {
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
      ┌──────▼───────────────────────▼───────┐
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
| Storage writer | 1 | Carries out every mutating `Storage` call — log, truncations, term and vote — in order, off the event loop; started on the node's first write |

**Budget at scale**: with G groups and P peers per group, each physical node runs approximately `G × (3 + P)` persistent goroutines. At G = 1,000 and P = 3, that is ~6,000 goroutines — well within Go's scheduler capacity. `Manager.RunTicker` adds one additional goroutine and a pool of `min(GOMAXPROCS, G)` workers to fan-out `Tick()` across all groups in parallel.

**Practical ceiling**: Go's M:N scheduler handles 10,000+ goroutines without issue on modern hardware. Goroutine overhead typically becomes noticeable beyond ~1,000 groups per node (~6,000 goroutines at P=3). In practice, disk I/O — not goroutines — is the binding constraint at common group counts; see [Caveats](#caveats-and-known-limitations) for details.

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

    // Snapshot writes the current state to w. Called from the apply loop
    // after crossing SnapshotThreshold.
    Snapshot(ctx context.Context, w io.Writer) error

    // Restore replaces the entire state with the snapshot read from r,
    // received from the leader or loaded from disk on restart.
    Restore(ctx context.Context, meta SnapshotMeta, r io.Reader) error
}
```

Key constraints:
- **Determinism**: `Apply` must produce the same output for the same `LogEntry` on every node. Do not read wall-clock time, random numbers, or external state inside `Apply`.
- **Streaming, not buffering**: `Snapshot` and `Restore` take an `io.Writer` and an `io.Reader`, so a large state is written and read incrementally rather than held in memory twice. A state machine that can hand over a cheap point-in-time copy can implement [`SnapshotCapturer`](#log-compaction-and-chunked-snapshot-transfer) as well, which moves the serialisation off the apply loop.
- **No cross-calls**: the library serialises all three methods — `Snapshot` and `Apply` are never called concurrently.
- **The context is `stopCtx`**: it is cancelled when `Stop()` is called. Long-running `Snapshot` or `Restore` operations should honour it.
- **Log entries for config changes are filtered**: `Apply` only receives application-level entries. Internal membership-change entries are handled by the library.

---

## Node lifecycle

```go
// 1. Build config (start from the production defaults).
cfg := raft.DefaultConfig()
cfg.ID        = "node-1"
cfg.Peers     = []raft.PeerConfig{{ID: "node-2", Voter: true}, {ID: "node-3", Voter: true}}
cfg.Storage   = store   // raft.Storage implementation
cfg.StateMachine = sm   // your StateMachine
cfg.Transport = tr      // raft.Transport implementation

// 2. Create the node (loads persisted state; does NOT start goroutines).
node, err := raft.New(&cfg)

// 3. Register with the transport so inbound RPCs are routed here.
tr.Register(cfg.ID, node.Handler()) // or net.Register(cfg.ID, node.Handler()) for memtransport

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

**What it does and does not guarantee:**
- The dedup table is replicated through the log and persisted inside every snapshot, so exactly-once survives leader failover and restart.
- Only a *successful* apply is recorded. A command the state machine rejected is not cached, so a retry runs it again and sees the same error rather than a spurious success.
- `MaxClientTableSize` (default 100 000) bounds the table. **A client that retries after its entry has been evicted has its request executed a second time** — size the table to outlive the retry window of your slowest client, and use stable `clientID` values.
- The bound is replicated. The leader writes it into the log ahead of the first `ProposeOnce` entry and snapshots carry it, so every replica evicts the same entry at the same point whatever its own `Config` says. `Node.MaxClientTableSize()` reports the value in effect; `Node.SetMaxClientTableSize(ctx, n)` changes it for the whole group.
- The bound is on entry count, not bytes: each entry also retains the result the state machine returned.
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

A read is answered only by a leadership confirmation that began after the read
arrived. Replies to a round that was already in flight prove leadership as of a
moment that had already passed, and leadership can move in that window. Reads
that arrive together still share one round, so the cost is one round-trip per
round, not one per read.

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

The lease is valid for `ElectionTimeoutMin − LeaseSafetyMargin` from the
instant the last barrier heartbeat was *sent* (not when ACKs arrived). Inject
a `raft.Clock` to make lease expiry deterministic in tests:

```go
cfg.Clock = myClock // implements raft.Clock: Now() time.Time
```

> **Warning**: lease reads rely on time passing at the same rate on every
> node. Elapsed time is measured on the monotonic clock, so NTP corrections
> do not affect it, and `LeaseSafetyMargin` covers ordinary rate differences
> and detects a suspended or live-migrated leader — but a rate skew larger
> than the margin is still stale reads. When in doubt, use `ReadIndex`.

---

## Cluster membership changes

### Growing a cluster safely

```go
// Add a node as a voter without ever weakening the quorum on the way in.
err := node.AddVoter(ctx, "node-4", 1024)
```

`AddVoter` adds the node as a learner, waits until it is within the given
number of entries of the leader, and only then promotes it to voter.

The reason to prefer it is that the direct route is not safe. `AddServer` with
`Voter: true` makes the new node count towards every quorum from the moment the
change commits, while its log may still be empty: a three-node cluster becomes
a four-node cluster needing three votes, one of which cannot be given until the
new node has caught up. For the length of that catch-up the cluster tolerates
no failures at all, which on a large state machine is minutes, and is the worst
possible moment to have spent the cluster's redundancy.

A learner costs nothing while it catches up: it replicates the log and votes on
nothing.

### Single-server changes

```go
// Add a new node (blocks until committed).
err := node.AddServer(ctx, raft.PeerConfig{ID: "node-4", Voter: true})

// Remove a node (blocks until committed).
err := node.RemoveServer(ctx, "node-2")
```

Only one membership change may be in-flight at a time; concurrent calls return
`ErrConfigChangeInProgress`.

Adding a voter changes the quorum as soon as the change is applied. A member
added as a voter while it is still empty counts towards the larger quorum
without being able to help satisfy it: a three-node cluster that adds a fourth,
empty voter goes from tolerating one failure to tolerating none until that
member catches up.

Add it as a non-voter and promote it once it is close:

```go
// Replicates, but does not vote or count towards quorums.
err := node.AddServer(ctx, raft.PeerConfig{ID: "node-4", Voter: false})

// Refuses with ErrMemberNotCaughtUp while it is more than 100 entries behind.
err = node.PromoteMember(ctx, "node-4", 100)
```

`ReplicationProgress` shows how far each peer has kept up, which is what to
check before any change that depends on replicas keeping up — promoting,
removing a member, handing over leadership, or taking a node out for
maintenance:

```go
progress, err := node.ReplicationProgress(ctx) // leader only
for _, p := range progress {
    log.Printf("%s: %d entries behind, voter=%v, snapshot=%v",
        p.ID, p.Lag(), p.Voter, p.SendingSnapshot)
}
```

### Joint consensus (arbitrary reconfiguration)

`ReconfigureCluster` atomically replaces the entire membership set using the
two-phase joint consensus protocol (§4.3 of the Raft dissertation):

```go
// Replace {n1, n2, n3} with {n1, n3, n4, n5}.
// Phase 1: commits a joint entry (C_old ∪ C_new both required for quorum).
// Phase 2: commits a finalise entry (C_new only) — happens automatically.
// ReconfigureCluster returns after Phase 1 commits; Phase 2 completes in
// the background (or is re-driven by the next leader if a crash occurs).
err := node.ReconfigureCluster(ctx, []raft.PeerConfig{
    {ID: "n1", Voter: true},
    {ID: "n3", Voter: true},
    {ID: "n4", Voter: true},
    {ID: "n5", Voter: true},
})
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

## Reacting to leadership changes

Work that only the leader should do — driving a scheduler, running a
compaction, holding an external lease — has to start and stop on the leadership
edge. Polling `State()` answers late and cannot tell a brief leadership change
from no change at all.

```go
changes, stop := node.LeadershipChanges()
defer stop()

for change := range changes {
    if change.IsLeader {
        go startLeaderWork()
    } else {
        stopLeaderWork()
    }
}
```

The current status is delivered on subscribe, and the channel is closed when
the node stops. Delivery is coalescing rather than lossless: a subscriber that
falls behind sees the most recent status, never a stale one, which means it
cannot count transitions.

---

## Configuration reference

Start from `raft.DefaultConfig()` and override only what you need:

```go
cfg := raft.DefaultConfig()
cfg.ID        = "node-1"                    // required
cfg.Peers     = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}   // bootstrap peers, excluding self
cfg.Storage   = store                       // required
cfg.StateMachine = sm                       // required
cfg.Transport = tr                          // required
```

### All fields with defaults

| Field | Default | Description |
|-------|---------|-------------|
| `ElectionTimeoutMin` | `150ms` | Lower bound of the randomised election timeout. Also the read-lease duration. |
| `LeaseSafetyMargin` | `15ms` | Taken off the read lease to cover clock-rate skew between nodes, and the tolerance for wall-clock time running ahead of monotonic time before a lease is dropped as the mark of a suspended leader. `0` restores the bare `ElectionTimeoutMin` lease. |
| `ElectionTimeoutMax` | `300ms` | Upper bound. Wider spread reduces split-vote probability. Must be > Min. |
| `HeartbeatInterval` | `50ms` | How often the leader sends heartbeats. Enforced constraint: `ElectionTimeoutMin ≥ 2×HeartbeatInterval`. Recommended: `HeartbeatInterval ≤ ElectionTimeoutMin / 5`. |
| `TickInterval` | `10ms` | Wall-clock period per `Tick()`. Zero means manual ticks (recommended for tests). |
| `MaxLogEntriesPerRPC` | `64` | Maximum entries per AppendEntries RPC. |
| `MaxBytesPerRPC` | `1 MiB` | Maximum total payload per AppendEntries RPC. A count limit alone says nothing about message size. `0` means no byte limit. |
| `MaxInflightRPCs` | `4` | Per-peer pipeline depth (concurrent unacknowledged AppendEntries RPCs). |
| `MaxUnstableLogBytes` | `64 MiB` | Log entries a node will hold in memory waiting on storage. Past it, `Propose` returns `ErrWriteBacklogFull` on a leader, and a follower refuses an AppendEntries with it. This is the backpressure that replaces waiting for the disk. |
| `MaxProposalBytes` | `0` | Largest command `Propose` and `ProposeOnce` will accept. `0` takes the limit from the transport, less the headroom request and entry framing need; `Node.MaxProposalBytes()` reports whichever applies. A command that cannot be replicated is far worse than one that is refused: it is appended to the leader's log, rejected by the transport on every send, and retried for ever while every later proposal queues behind it. |
| `ProposalQueueSize` | `1024` | Proposals that may wait for the event loop at once. The event loop drains the queue in one go, so it is also the batch size for storage writes. |
| `ProposalOverflow` | `Wait` | What `Propose` does when the queue is full: `ProposalOverflowWait` blocks until there is room or the context is done; `ProposalOverflowReject` returns `ErrProposalQueueFull` at once. `ProposalQueueDepth()` reports the occupancy. |
| `SnapshotThreshold` | `10000` | Entries past the last snapshot that trigger an automatic snapshot. `0` disables auto-snapshots. |
| `TrailingLogs` | `1024` | Entries retained behind the snapshot point, so a slightly-behind follower catches up from the log instead of needing a full state transfer. Capped at `SnapshotThreshold-1` in use. |
| `SnapshotChunkSize` | `1 MiB` | Maximum bytes per InstallSnapshot chunk. Leave room for framing: a chunk sized at exactly the transport's message limit does not fit. `0` sends snapshots as a single RPC. |
| `RPCTimeout` | `0` | Per-RPC deadline. `0` falls back to `ElectionTimeoutMin`. Snapshot RPCs use `4×RPCTimeout`. |
| `CheckQuorum` | `true` | Leader steps down if it doesn't hear from a quorum within one election timeout. |
| `CommitQuorum` | `0` | Voters an entry must reach to commit; `0` is a majority. The election quorum is `voters − CommitQuorum + 1`, never below a majority. Group state: this is only what a group is created with; change it with `Node.SetCommitQuorum`. |
| `MaxClientTableSize` | `100000` | Maximum entries in the ProposeOnce dedup table; `0` disables eviction. The value a group is created with: it is replicated through the log, so a node whose `Config` differs from the group's uses the group's and logs a warning. Change it at run time with `Node.SetMaxClientTableSize`. |
| `OnFatal` | `nil` | Called once, from its own goroutine, if the node stops because a durable write failed. |
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
import "github.com/brunoga/raft/v2/storage/memstore"

store := memstore.New()
```

Non-durable. Data is lost on process exit. Suitable for unit tests and simulations.

### `storage/filestore` — file-backed (production)

```go
import "github.com/brunoga/raft/v2/storage/filestore"

store, err := filestore.Open("/var/lib/myapp/raft")
defer store.Close()
```

Features:
- **CRC32 checksums** on every log entry.
- **fsync before return** on all mutating operations — safe across crashes.
- **Automatic segment rotation** at 64 MiB (configurable via `OpenWithSegmentSize`).
- **Crash recovery**: on open, the tail of the last segment is scanned and any partial write is truncated.
- **Context-aware**: pre-flight `ctx.Err()` checks at entry and between write/sync steps prevent starting new I/O when the node is shutting down.
- **Exclusive directory lock**: `Open` fails with `ErrLocked` if another store already holds the directory.
- **Recorded commit index** (`raft.CommitRecorder`): a checksummed, unsynced record of how far the log had committed, so a disaster recovery has more to go on than the snapshot boundary.

```go
// For tests: use small segments to exercise rotation.
store, err := filestore.OpenWithSegmentSize("/tmp/test-raft", 4096)
```

**On-disk layout:**
```
/var/lib/myapp/raft/
  LOCK              — empty; the exclusive lock on it is what one store holds
  commit            — recorded commit index (12 bytes, CRC32, rewritten in place)
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

**One writer per directory.** Two `FileStore`s on one directory share nothing:
each keeps its own segment list, its own hard-state sequence number, and its
own idea of where the log ends. They overwrite each other's records, and what
survives is a log with two writers' entries interleaved or a hard state that
has gone backwards — which is how a node votes twice in one term. Nothing
reports it at the time; it is found on the next restart, or never.

`Open` therefore takes an exclusive lock on the directory and returns
`ErrLocked` if another store holds it. The lock lives for the life of the
store, is released by `Close`, and is dropped by the kernel if the process
dies, so a crash leaves nothing to clean up. It is advisory: it stops another
`FileStore`, not another program writing into the directory, and on NFS it may
not stop anything at all.

A node does not own its storage and does not close it, so a restart in the same
process means closing the store yourself after `Node.Stop` and opening a fresh
one. This is also what makes `RecoverCluster`'s precondition — the node stopped
and no other handle open — something the store enforces rather than something
the documentation asks for.

### `storage/sharedwal` — one log for every group on a host (multi-Raft)

```go
import "github.com/brunoga/raft/v2/storage/sharedwal"

wal, err := sharedwal.Open("/var/lib/myapp/raft")
defer wal.Close()

for _, groupID := range groupIDs {
    cfg := raft.DefaultConfig()
    cfg.Storage = wal.Storage(groupID)   // raft.Storage, BatchWriter, CommitRecorder
    // ...
}
```

Every group on the host appends to the same write-ahead log, and one goroutine
writes everything that is waiting and syncs once per batch. A burst of appends
from a hundred groups costs one `fsync` rather than a hundred, which is what
makes a write-heavy host with many groups possible on ordinary storage; see
[Scale boundaries](#scale-boundaries). Records carry their group, every record
has a checksum, and a torn tail from a crash is cut off on the next open.

Snapshot data lives outside the log in one file per group. Segments are
deleted once nothing in them is still needed, and the little that keeps an
old one alive — a group's latest hard state, or a handful of live entries —
is copied forward so that a quiet group cannot pin old segments for ever.

`Groups()` lists the groups the log holds, which is how a host finds them
again after a restart; `Remove(groupID)` forgets a decommissioned group. The
directory is locked like `filestore`'s, and the value `Storage` returns has a
no-op `Close`: the log is shared, and closing it is `wal.Close`'s job.

---

## Transport backends

### `transport/memtransport` — in-process (tests)

```go
import "github.com/brunoga/raft/v2/transport/memtransport"

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
import "github.com/brunoga/raft/v2/transport/grpctransport"

tr, err := grpctransport.Listen(":7001", grpctransport.WithTLSConfig(tlsCfg))
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

**Transport security is not optional.** `Listen` returns
`ErrNoTransportSecurity` unless told what to do about it:

```go
// tlsCfg should have Certificates, RootCAs/ClientCAs, and ClientAuth set.
tr, err := grpctransport.Listen(":7001",
    grpctransport.WithTLSConfig(tlsCfg),
    grpctransport.WithPeerAuthorizer(grpctransport.MTLSPeerAuthorizer(nil)),
)

// A local cluster or a network trusted for other reasons: say so.
tr, err := grpctransport.Listen("127.0.0.1:7001", grpctransport.WithInsecure())
```

`WithTLSConfig` applies the same `*tls.Config` to both the server listener and
all outbound client connections. Pass separate configs to
`WithServerOptions(grpc.Creds(...))` / `WithDialOptions(grpc.WithTransportCredentials(...))`
if server and client configs must differ, together with `WithCustomCredentials`
so that `Listen` adds none of its own. `WithInsecure` is the explicit choice to
run in plaintext; a transport created with it logs a warning once.

Default keepalive settings are applied automatically; override them the same way.

**Wire compatibility across versions.** Nodes are upgraded one at a time, so
every version has to talk to the one before it. Protobuf covers most of that —
a field added with a fresh number is ignored by a peer that does not know it —
but not a field number changing meaning: renumber a field, reuse a deleted
one's number, or retype one in place, and both sides still parse the message
into different values. A `leader_commit` read as a `prev_log_index` does not
fail a request; it commits the wrong entries, with nothing to see for as long
as the upgrade window lasts.

The rules are stated at the top of
[`proto/raft.proto`](transport/grpctransport/proto/raft.proto), and
`wire_compat_internal_test.go` pins the current field numbers, types and RPC
method set so that breaking any of them fails a test rather than a rolling
upgrade. The generated bindings live in an internal package: the encoding is
how this transport happens to carry a Raft RPC, not part of its API.

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
import (
    "github.com/brunoga/raft/v2/metrics/prommetrics"
    "github.com/prometheus/client_golang/prometheus"
)

m := prommetrics.New(prometheus.DefaultRegisterer) // registers metrics on prometheus.DefaultRegisterer
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
import "github.com/brunoga/raft/v2/metrics/rpctracer"

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
    var peers []raft.PeerConfig
    for _, p := range ids {
        if p != id {
            peers = append(peers, raft.PeerConfig{ID: p, Voter: true})
        }
    }
    cfg := raft.DefaultConfig()
    cfg.ID = id
    cfg.Peers = peers
    cfg.Storage = memstore.New()
    cfg.StateMachine = &MySM{}
    cfg.Transport = net.NewTransport(id)
    cfg.TickInterval = 0 // manual ticks
    nodes[i], _ = raft.New(&cfg)
    net.Register(id, nodes[i].Handler())
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

Compaction keeps `TrailingLogs` entries behind the snapshot point. Without a
retained tail, a full state transfer is the only way to catch up a follower that
was even one entry behind at the moment of compaction — and with automatic
snapshots, that moment comes round again and again.

A follower is sent a snapshot only when the entries it needs are no longer in
the leader's log, so raising `TrailingLogs` trades disk space for fewer state
transfers.

Large snapshots are split into `SnapshotChunkSize`-byte chunks and sent as
sequential RPCs. The follower streams chunks to storage rather than buffering
the whole snapshot, and applies it atomically on the final chunk. A partial
transfer is discarded if the follower changes leaders. When the snapshot
disagrees with the follower's log at its last-included index, the whole log is
discarded: entries after that point belong to a history the cluster abandoned.

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
    node, _ := raft.New(&cfg)
    tr.Register(myNodeID, node.Handler())            // single-group: still needed
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

Groups must not share a `filestore` directory. Either give each group its own:

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

or, for many groups on one host, keep them all in one shared log, which is
what [`storage/sharedwal`](#storagesharedwal--one-log-for-every-group-on-a-host-multi-raft)
is for:

```go
wal, err := sharedwal.Open("/var/lib/myapp/raft")
store := wal.Storage(groupID)
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
before the RPC is sent.

`GRPCTransport.HeartbeatStats()` reports what that path has done — batches and
entries served, dispatch errors, and sends that had to wait for space in a
per-peer channel. `EntriesServed / BatchesServed` is the average batch size,
which is the number batching exists to raise; a non-zero `SendBlocked` rate
means more groups than the channel absorbs, so raise
`WithHeartbeatChannelSize`. `ResetHeartbeatStats()` returns the same struct and
clears the counters, so each call reports one interval rather than a running
total.

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
[`examples/shardkv`](examples/shardkv/) for a working end-to-end example
and its README for a fuller discussion of the trade-offs.

### Scale boundaries

**Practical group counts**: at 10–200 groups per node the implementation runs comfortably within both goroutine and I/O budgets on typical SSD hardware. Beyond ~500 actively-writing groups, disk throughput becomes the binding constraint rather than CPU or goroutines.

**fsync amplification (filestore)**: `filestore` issues an `fsync` after every mutating operation on each group's storage. Under write load, G simultaneously-active groups can issue G fsyncs within a single tick window. On a fast NVMe device (≈200 µs per fsync), 500 concurrent fsyncs consume roughly 100 ms of disk time. `sharedwal` removes the multiplier: every group on the host appends to one log and a batch of appends costs one `fsync`, whatever the number of groups in it.

What that costs has changed. Every mutating storage call — log entries, truncations, and the term and vote — is carried out by each group's own storage-writer goroutine rather than on its event loop, so a group waiting on its own storage still counts election ticks, still reads its inbound queue, still answers pre-votes, and still replicates the entries it has accepted.

A disk backlog now delays *commits* and *replies*, rather than stopping a group from running. Entries are not counted towards a quorum until they are written, and a reply that carries this node's term waits for the term to be written — because an acknowledgement or a granted vote that did not survive a crash is how one term ends up with two leaders. What no longer happens is a healthy group looking dead to its peers and losing an election it would then have to fight.

Reads were the other half. A node used to ask storage for an entry's term on every heartbeat, on every inbound append, and once per index while working out where two logs diverge, all on the event loop. In a file-backed store those reads take the same lock the writer holds across its fsync, so a loop that no longer writes to the disk could still end up waiting for one. Terms never decrease with index, so the whole log is a short sequence of runs, one per leadership epoch; those are kept in memory and answer every one of those questions without touching storage. The only storage read left on the event loop is fetching entries for a follower that is genuinely behind, which is the data being sent rather than metadata about it.

A node whose storage falls far enough behind refuses new entries with `ErrWriteBacklogFull` rather than growing its backlog without limit; see `MaxUnstableLogBytes`. A leader returns it to the caller of `Propose`, and a follower returns it to its leader, which retries.

Disk throughput is still the binding constraint on write rate, and these remain the ways to spend less of it:

- **Stagger write load**: spread groups so that only a fraction are actively receiving proposals at any instant. Read-heavy or idle groups do not amplify fsyncs.
- **Use `storage/sharedwal`**: one write-ahead log for every group on the host, one `fsync` per batch of appends from any number of groups. `filestore` is the reference implementation for correctness and single-group deployments; `sharedwal` is for the 1,000-group write-heavy host.
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

### Lease reads assume clocks that run at the same rate

`ReadIndexLease` serves a read without contacting any follower, on the
argument that no follower can have started an election within
`ElectionTimeoutMin` of the last heartbeat. Elapsed time is measured on the
monotonic clock, so NTP stepping the wall clock does not affect it. What can
still break the argument is a clock *rate* difference between nodes, and a
leader that was suspended — a paused or live-migrated VM — whose monotonic
clock stood still while its followers' election timers ran.
`LeaseSafetyMargin` (default 15 ms) is taken off the lease to cover the
first, and is the tolerance beyond which wall-clock time running ahead of
monotonic time is treated as a suspend and the lease dropped, for the
second. A rate skew larger than the margin cannot be detected and is still
stale reads. Use `ReadIndex` when strong linearizability is required
regardless of clock quality.

### A hung fsync stalls this node's writes, not its clock

Storage writes run on their own goroutine, so the event loop keeps counting
election ticks, answering heartbeats and voting while a write is in progress.
What a slow or hung `fsync` does hold up is everything that depends on that
write having landed: this node's acknowledgements, the entries it can count
towards a commit, and — on a leader — the proposals queued behind it, which
`MaxUnstableLogBytes` bounds. Once an `fsync` syscall has been issued it
cannot be interrupted, so on NFS or network-attached block storage a hung
disk holds those writes until the OS returns. Use local SSDs in production
for predictable latency.

### Single-node clusters and `CheckQuorum`

In a single-node cluster `Peers` is empty, so `CheckQuorum` is a no-op
(the leader is always its own quorum). No special configuration is needed.

### `Config.Peers` is a bootstrap value

Membership is agreed through the log, so it is cluster state rather than
configuration. On restart it is recovered from the snapshot and the log;
`Config.Peers` applies only when there is neither. `New` copies the `Config`
and the slice, so the caller's values are never written to — but they are also
never updated. Read the membership the node actually has with `Members()`.

### A node that cannot write to storage stops

Raft's safety argument assumes a node's term, vote and log entries reach stable
storage before it acts on them. A node that cannot complete one of those writes
stops rather than continuing: carrying on could mean voting twice in one term,
or acknowledging entries a leader then counts towards a commit quorum although
they are not durable.

`FatalError()` reports the failure, every subsequent operation returns it in
place of `ErrStopped`, and `Config.OnFatal` is called once. The rest of the
cluster treats the node as an ordinary unreachable peer. Recovery is operator
action: fix the storage and restart the node.

### Exactly-once is bounded by the dedup table

`ProposeOnce` remembers the outcome of a request until its entry is evicted from
a table of `MaxClientTableSize` entries. A client that retries after its entry
has been evicted has its request executed a second time. Size the table to
outlive the retry window of the slowest client.

Eviction order is a function of the log alone, and so is the bound: the leader
writes it into the log ahead of the first `ProposeOnce` entry, and snapshots
carry it, so every replica evicts the same entry at the same point whatever
its own `Config.MaxClientTableSize` says. Resize a running group with
`Node.SetMaxClientTableSize`; a smaller bound forgets the oldest clients
everywhere at once, and a larger one recovers nothing already forgotten.

### Nothing listens open by accident, but plaintext is still a choice

A Raft peer is fully trusted: any host that can complete a connection to the
transport can claim to be the leader and force the cluster to step down. So
`grpctransport.Listen` refuses to start without `WithTLSConfig` unless
`WithInsecure` says plaintext is intended, and `Manager.Handler` answers every
request with `403` unless given `WithRequestAuthorizer` or
`WithInsecureHandlerAcknowledged`; `easyraft` applies the same rule to its
transport and its HTTP API. TLS authenticates the connection; pair it with
`WithPeerAuthorizer`, which checks the claimed node identity against the
verified certificate, or any certificate from the CA can impersonate any node.

What the library cannot judge is whether the network a plaintext transport
was asked for is really trusted. That decision, and its consequences, stay
with the operator who made it.

### `ErrObsoleteSeqNum` must not be retried

If `ProposeOnce` returns `ErrObsoleteSeqNum`, the supplied `seqNum` is strictly
less than the highest seen for that client. Retrying with the same `seqNum`
will never succeed. Advance `seqNum` and retry with the new value, or
investigate why the sequence number went backwards.

### A removed node is told, but not stopped

A node removed by `RemoveServer` or `ReconfigureCluster` learns of it: the
leader keeps replicating to it until it has acknowledged the removal entry
and a commit index covering it, at which point it steps down to a non-voting
follower and `Config.OnRemoved` fires. That is where the decision about what
to do with it belongs — shut it down, wipe its storage, keep it for
inspection — and the callback may call `Stop`. The same transition is on the
`Events` stream as `EventPeerRemoved` with `Peer` set to the node's own ID.

The courtesy is bounded. A removed node that cannot be reached within a few
election timeouts, or that is so far behind it would need a snapshot, is
given up on; if it comes back later it will find nobody replicating to it,
and `OnRemoved` will not fire. Restart it against the current membership, or
retire it.

`RemoveServer(ctx, leaderID)` works — the leader commits the removal, then
steps down — but for leader self-removal as part of a larger reconfiguration
prefer `ReconfigureCluster`, whose joint consensus keeps a quorum available
throughout the two-phase transition.

### No dynamic `TickInterval` changes

`TickInterval` is read once in `New()` and converted to tick counts. Changing it
after `Start()` has no effect.

### fsync amplification with many groups on `filestore`

When using `filestore` with many simultaneously-active groups, each group issues its own `fsync` on every log append. G concurrent writers produce up to G fsyncs per replication round. On NVMe storage this is usually acceptable up to ~100–200 concurrent writers; on network-attached or spinning storage the accumulated latency spikes will cause election timeouts well below that threshold. Above that, use `storage/sharedwal`, which keeps every group on a host in one log and syncs once per batch. See [Scale boundaries](#scale-boundaries) in the Multi-Raft section.

### Backpressure has two limits: the write backlog and the proposal queue

`Config.MaxUnstableLogBytes` caps how much log a leader will hold in memory
waiting for storage; beyond it `Propose` and `ProposeOnce` are refused with
`ErrWriteBacklogFull` rather than queued. That is the admission control for a
disk that has fallen behind, and it is what keeps a node with stalled storage
from growing its backlog until the process dies.

The proposal queue is bounded separately. `Config.ProposalQueueSize` (default
1,024) is how many proposals may wait for the event loop at once, and when the
event loop falls behind for a reason other than the disk — a slow state
machine, a long snapshot, heavy replication — `Config.ProposalOverflow`
decides what happens to the next one. The default, `ProposalOverflowWait`,
blocks `Propose` until there is room or `ctx` is done. `ProposalOverflowReject`
returns `ErrProposalQueueFull` at once, for a caller that would rather shed the
request than hold a goroutine on it. `Node.ProposalQueueDepth()` reports the
occupancy, so the queue filling up can be seen before either happens.

---

## EasyRaft — high-level abstraction

`easyraft` is a batteries-included layer on top of the core `raft` package that handles transport (gRPC), storage (filestore), snapshotting, leader routing, peer discovery, and an HTTP REST API — letting you build a strongly-consistent replicated service by describing **what** data to store rather than how.

See [`easyraft/`](easyraft/) for the full API reference, option guide, and usage examples.

---

## Reference implementation

Nine fully-worked services are provided, each targeting a different deployment pattern — from a single-group service using the core `raft` package directly, through a registry whose entries expire when nothing renews them and a state machine that keeps its own state on disk, to multi-raft deployments with a group per tenant and automatic leader balancing — plus `raftctl`, an offline operator tool for a cluster that has lost its quorum.

See [`examples/`](examples/) for the full index with build instructions and quick-start commands for each example.

## Placement-aware commits

A majority says nothing about where the replicas are. Three replicas in one
availability zone are a quorum, and losing that zone loses every write they
acknowledged.

```go
cfg.Zones = map[raft.NodeID]raft.ZoneID{
    "n1": "eu-west-1a", "n2": "eu-west-1a", "n3": "eu-west-1b",
}
cfg.MinCommitZones = 2
```

An entry now has to reach two distinct zones before it counts as committed, in
addition to reaching a majority. An acknowledged write is on hardware in two
failure domains before anyone is told it succeeded.

The cost is liveness, and it is the point rather than a side effect: if the
second zone is unreachable, nothing commits. A cluster that would rather keep
taking writes into one zone should leave `MinCommitZones` unset.

`Zones` is local to each node and never replicated. Placement is a fact about
infrastructure rather than about consensus: it changes when machines move
rather than when the cluster agrees on something, so it costs nothing on the
wire and is corrected by a restart rather than a configuration change. A node
absent from the map is in no known zone and does not count towards the spread,
because a node nobody placed cannot be evidence that a write survived the loss
of a zone.

`Validate` refuses a configuration whose voters do not span the required number
of zones, since it could never commit anything.

## Flexible quorums

Raft commits on a majority and elects on a majority, and the argument that a
new leader holds every committed entry uses one fact about those two sets:
they intersect. Any pair of sizes with that property serves it, so a group
may trade one against the other (Howard, Malkhi and Spiegelman, *Flexible
Paxos*). With `N` voters and a commit quorum of `Q`, the election quorum is
`N − Q + 1` — or a majority, whichever is larger, because Raft additionally
needs one leader per term and two election quorums below a majority need not
intersect each other.

```go
// Five voters. Writes need two acknowledgements; a leader needs four votes.
err := leader.SetCommitQuorum(ctx, 2)

// Writes need every replica; elections are unchanged. No acknowledged write
// is ever on fewer than five disks.
err = leader.SetCommitQuorum(ctx, 5)

// Back to a simple majority.
err = leader.SetCommitQuorum(ctx, 0)
```

The policy is group state, like membership: it is agreed through the log and
carried in snapshots, `Node.CommitQuorum()` reports the value in effect, and
`Config.CommitQuorum` is only what a group is *created* with — the first
leader writes it into the log, and no node ever counts by its own `Config`.
Two nodes bootstrapped with different values therefore cannot disagree about
how to count.

What the split changes is liveness, which is the point. A commit quorum below
a majority makes a write need fewer acknowledgements than a leader needs
votes, for a group whose writes are frequent and whose elections are not. A
commit quorum above a majority means no acknowledged write is ever on fewer
than that many disks — but one voter down stalls every write. Choose against
the failures the deployment expects.

The change is safe to make while the group is running. A node that holds the
new policy in its log but has not applied it yet requires the *stricter* of
the old and new sizes for every decision, so no vote is ever counted under a
pair of quorums that do not intersect. The count follows the membership: if
voters are later removed below it, it is treated as "all of them".

## Witnesses

A witness is a voter that keeps the index and term of every log entry and
never the entries themselves, and applies nothing (dissertation §11.7.2). It
votes and counts towards every quorum like any voter, so two full replicas
and a witness survive the loss of any one member at a third of the storage
and none of the state machine a third full replica would cost — a small
machine in a third location whose job is to break ties.

```go
// On the witness: no state machine, only the log's shape.
cfg := raft.DefaultConfig()
cfg.ID = "w"
cfg.Witness = true
cfg.Peers = []raft.PeerConfig{{ID: "a", Voter: true}, {ID: "b", Voter: true}}

// On the full replicas: the witness is a voting peer marked as one.
cfg.Peers = []raft.PeerConfig{{ID: "b", Voter: true}, {ID: "w", Voter: true, Witness: true}}

// Or add one to a running group.
err := leader.AddWitness(ctx, "w")
```

The leader sends a witness each entry's index and term and none of its
contents, and sends it a snapshot that is a few hundred bytes of membership
and policy. A witness cannot become leader, cannot be the target of a
leadership transfer, and cannot be the source of a state transfer, so a
group must keep at least one full voter.

**The leader prefers full replicas.** A witness's acknowledgement counts
towards a commit quorum only while some full voter that lacks the entry has
stopped answering. In a healthy group every committed entry is therefore on
every full replica, and a witness stands in for a full replica that is down
rather than for one that is merely slow. The alternative — counting the
witness whenever it answers first — commits entries that live on one full
replica, and if that replica then fails no survivor can supply them and no
full replica behind them can be elected.

The membership and the node must agree. `New` refuses a node whose recovered
membership disagrees with `Config.Witness`, and a full node that applies a
membership entry calling it a witness stops with `ErrWitnessMismatch` rather
than apply stripped entries. Upgrade every node before adding a witness: a
node on a version without witnesses reads the role as a non-voter.

## State machines that keep their own state

A state machine that is itself a database already holds, on its own disk, the
effect of every entry it has applied. Without a way to say so it is rebuilt
from a snapshot and replayed over on every restart, which is work already done
and, for operations that are not idempotent, work that must not be done twice.

`DurableStateMachine` is that way:

```go
func (sm *MySM) AppliedIndex(ctx context.Context) (raft.Index, error) {
    return sm.db.ReadAppliedIndex(ctx)   // persisted with the state itself
}
```

The node asks once while starting, skips the snapshot restore if the state
machine is already past it, and replays only what comes after.

Implementing it is a promise about durability. The index reported must be one
whose effect, and the effect of every entry before it, is on stable storage.
The engine will never replay at or below it again, so a gap there becomes
permanent divergence from every other replica with nothing in the log to
explain it. A state machine that batches its writes satisfies this by making
them durable before `ApplyBatch` returns, which is one sync per batch rather
than one per entry.

A state machine that reports an index higher than this node's log is refused
at construction: it cannot be replayed up to, and ignoring it would apply those
indices twice once the log caught up.

## Applying entries in batches

[`examples/durablekv`](examples/durablekv/) puts `DurableStateMachine`,
`BatchApplier` and `SnapshotCapturer` together in one worked state machine.

A `StateMachine` that also implements `BatchApplier` is handed runs of
committed entries in one call instead of one at a time:

```go
func (sm *MySM) ApplyBatch(ctx context.Context, entries []raft.LogEntry) ([]raft.ApplyOutcome, error) {
    tx := sm.db.Begin()
    out := make([]raft.ApplyOutcome, len(entries))
    for i, e := range entries {
        v, err := sm.applyTo(tx, e)
        out[i] = raft.ApplyOutcome{Value: v, Err: err}
    }
    return out, tx.Commit()   // one transaction, one fsync
}
```

The entries already arrive in runs: the apply loop is handed everything
committed since it last looked, which under load is dozens at a time. A state
machine backed by storage almost always has a way to group that work, and
applying one entry at a time denies it that.

The returned error is for the batch failing as a whole, such as a transaction
that would not commit, and is reported to every entry in it. A single command
the state machine rejects is not that: report it as the `Err` of its own
outcome, and the entries around it still apply.

A state machine that does not implement it is called through `Apply`, exactly
as before.

## Watching what a node does

`Node.Events` reports the things a metric cannot carry: which peer joined,
which follower stopped answering, which snapshot failed and why. A counter of
configuration changes does not name the node that joined, and a replication
histogram does not say which follower went quiet, which is the only fact that
decides whether the next failure costs the cluster its quorum.

[`examples/watchtower`](examples/watchtower/) is a working service built on
this: an SSE event stream, the peer-health state assembled from it, and apply
saturation exported to Prometheus.

```go
events, stop := node.Events()
defer stop()
for ev := range events {
    switch ev.Type {
    case raft.EventPeerUnresponsive:
        openIncident(ev.Peer)
    case raft.EventPeerResponsive:
        closeIncident(ev.Peer)
    }
}
```

Delivery is bounded and lossy on purpose: a subscriber that stops receiving
loses events rather than stalling consensus for everyone else. Nothing is lost
silently, though. The next event a lagging subscriber receives carries the
number discarded before it in `Event.Dropped`.

If `Config.Metrics` also implements `ApplyMetrics`, the node reports what
fraction of its time the apply loop spends working rather than waiting. That
is the number that says where a slow write is slow: proposal latency covers
consensus and the state machine together, and a saturation near 1 says the
state machine is the constraint and faster consensus will not help.

## Disaster recovery from permanent quorum loss

Raft keeps a cluster available while a minority is down and refuses to make
progress when a majority is. That refusal is the whole point — a cluster that
committed without a majority could lose the write — but it also means that
losing two nodes of three, for good, leaves a cluster that cannot elect a
leader, cannot commit, and so cannot commit the configuration change that would
shrink it to a size the survivor is a majority of. The data is intact and
permanently unreachable.

`RecoverCluster` is the way out. It rewrites a stopped node's durable state so
that, on restart, it belongs to a membership you name instead of the one its log
records:

```go
// With the node stopped, and its storage opened by nothing else.
info, err := raft.InspectStorage(ctx, store)   // what this node holds
...
err = raft.RecoverCluster(ctx, store, "n1", []raft.PeerConfig{
    {ID: "n1", Voter: true},
})
```

`self` must be the only voter in the new membership: a recovered node that is
not a majority by itself cannot elect a leader either. Nodes that are to rejoin
may be listed alongside it as non-voters, and are promoted with `PromoteMember`
once they have caught up.

This is not a consensus operation, and it cannot be made safe the way the rest
of the package is. Two things go wrong, and they are not the same kind of
problem.

**The first is not fixable by anything.** Entries the lost majority committed
that never reached this node are gone — nothing that remains has them, so a
client told its write succeeded may find that it did not. Raft's guarantee is
that a committed entry is on a majority; losing a majority *is* losing the
guarantee. The answer is not recovery but not needing it: more voters,
`MinCommitZones` to spread them across failure domains, backups off the
cluster.

**The second is bounded, and the API bounds it.** The log splits at the highest
index the node can prove was committed — `RecoveryInfo.KnownCommittedIndex`,
which comes from the snapshot and, for a store implementing `CommitRecorder`
(`filestore` does), from the commit index the node wrote down as it ran.
Below it, everything is certain. Above it, each entry either committed on the
majority that died or was still in flight, and nothing that survives can say
which. `RecoveryInfo.UncommittedBand()` names that range, and recovery has only
two ways to decide about it:

| | keeps | costs |
|---|---|---|
| default | the whole log | entries that were never committed become committed — a write reported as failed took effect |
| `DiscardUncommitted()` | only the proven prefix | entries that *had* committed but were not yet applied are thrown away — a write reported as succeeded did not |

Neither is safe; which is the lesser harm depends on the application, not on
Raft. What the package does is make the band small and make it visible:

```go
info, _ := raft.InspectStorage(ctx, store)
if from, to, ok := info.UncommittedBand(); ok {
    log.Printf("entries %d..%d are in doubt", from, to)  // look at them
}

// A state machine with its own durable state proves more than the snapshot
// does, which shrinks the band to what the apply loop had not reached.
applied, _ := sm.AppliedIndex(ctx)
report, err := raft.RecoverCluster(ctx, store, "n1",
    []raft.PeerConfig{{ID: "n1", Voter: true}},
    raft.WithKnownCommitted(applied))
log.Printf("recovery promoted entries %d..%d", report.PromotedFrom, report.PromotedTo)
```

Keep that report. A cluster that comes back after a recovery looks like any
other cluster, and six months later it is the only way to explain a write that
vanished or one that reappeared.

`RecoverCluster` refuses `DiscardUncommitted()` when nothing at all is proven —
no snapshot and no `WithKnownCommitted` — rather than throwing away an entire
log on the strength of an option.

It is an operator action for a cluster that is already dead, not a way round
one that is merely slow or partitioned. The procedure:

1. Stop every surviving node.
2. Run `InspectStorage` on each survivor and pick the most recent with
   `RecoveryInfo.MoreRecentThan` — the §5.4.1 up-to-date rule, higher last term
   first and longer log to break the tie. That node's history is the one kept.
   Check `UncommittedBand()` and look at what is in it: this is the last moment
   those entries can be told apart from the rest.
3. Run `RecoverCluster` on it, with `WithKnownCommitted` if the state machine
   keeps its own durable state. Keep the report.
4. **Erase the storage of every other survivor.** Their logs are now divergent
   history, and a node that restarts holding entries the recovered node does not
   have can disrupt the elections of the cluster it is no longer part of.
   Recovering more than one node has the same problem for the same reason.
5. Restart the recovered node. It elects itself and serves.
6. Add the wiped nodes back with `AddMember`. They receive a snapshot and catch
   up the ordinary way.

`InspectStorage` is read-only and safe to run on any stopped node, and
`filestore` refuses to open a directory another store holds, so running it
against a node that is still up fails rather than racing its writes.

[`examples/raftctl`](examples/raftctl/) is a working operator tool built on
this: `inspect` one node, `compare` the survivors, `recover` the one you keep.
Its README walks the whole procedure. Its
`Members` field is the membership that node would restart with — the one in its
snapshot, advanced by every configuration entry in its log, committed or not,
since §4.1 says a node uses the latest configuration it has rather than the
latest it has agreed. `MembersComplete` reports whether that could be
reconstructed at all: a cluster that has never snapshotted and never
reconfigured knows its members only from the `Config.Peers` its operator passes
to `New`, which storage has never seen.

## Divergence from the paper

Every implementation departs from the Raft paper and dissertation somewhere.
[`docs/divergence.md`](docs/divergence.md) lists every place this one does, the
reason for each, and the known limitations.

## Changelog

[`CHANGELOG.md`](CHANGELOG.md) lists what each release contains.

## Compatibility

[`docs/compatibility.md`](docs/compatibility.md) states what a version number
promises: which packages are covered, that a cluster can be upgraded one node
at a time because the on-disk and wire formats are stable within a major
version, what is deliberately not covered, and how new capability is added
without breaking what exists.

## Licence

Apache License 2.0. See [LICENSE](LICENSE).
