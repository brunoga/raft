# Changelog

Notable changes, newest first. This project follows
[semantic versioning](https://semver.org/); what a version number promises is
spelled out in [`docs/compatibility.md`](docs/compatibility.md).

## v1.0.1

### Fixed

- The published `v1.0.0` module zip contained a 21MB example binary that had
  been committed by mistake. The package's own source is about 2.5MB; the zip
  was 12MB compressed and 24MB unpacked, so anyone depending on `v1.0.0` was
  downloading and caching roughly ten times what the module is. The binary is
  gone from the tree, and CI now fails on any tracked file that `.gitignore`
  names or that exceeds 1MB.

  `v1.0.0` cannot be repaired in place — the module proxy and checksum database
  have it pinned — so `v1.0.1` is the same code, correctly packaged. No source
  change, no API change; upgrading is a `go get` and nothing else.

## v1.0.0

First stable release. The API of every package listed in
[`docs/compatibility.md`](docs/compatibility.md) is now covered by the
compatibility promise, as are the on-disk and wire formats: a `v1.x` node reads
a directory written by any other `v1.x`, and two nodes on different `v1.x`
versions interoperate in either direction.

### The algorithm

Leader election, log replication, and log compaction with chunked snapshot
transfer. Membership changes both ways the dissertation describes — single
server (§4.1) and joint consensus (§4.3). Leadership transfer (§3.10), pre-vote
(§9.6), check-quorum (§6.2), linearizable reads through `ReadIndex` and
clock-based lease reads (§6.4), and an exactly-once client protocol (§6.3).

Every deliberate departure from the paper, and every limitation the
implementation knows about, is listed in
[`docs/divergence.md`](docs/divergence.md).

### Persistence off the event loop

Log writes are queued and carried out behind the event loop, so a slow disk
delays only what depends on the disk rather than stopping the node from
counting election ticks or answering its peers. What still waits for the disk
is anything another node could rely on: a vote, an acknowledgement a leader may
count towards a commit, a commit index.

A node that cannot complete a durable write stops rather than continuing with
state that may not survive a restart (`ErrNodeFailed`), and
`Config.MaxUnstableLogBytes` bounds how much unwritten log a leader will hold
before refusing proposals with `ErrWriteBacklogFull`.

### Optional interfaces

A `Storage` or `StateMachine` that can do better than the minimum says so by
implementing an extra interface, which the engine detects. None is required,
and none changes the contract of the one it extends.

- `BatchWriter` — write the hard state and a run of entries as one durable
  operation.
- `CommitRecorder` — remember how far the log committed, so recovering from a
  permanent quorum loss has more to go on than the snapshot boundary.
- `DurableStateMachine` — report the index already durable in the state
  machine's own storage, so a restart replays nothing it has already applied.
- `BatchApplier` — apply a run of committed entries in one call.
- `SnapshotCapturer` — hand over a cheap point-in-time copy, so serialising a
  snapshot does not hold up the apply loop.

### Beyond one group

`Manager` runs many groups on shared infrastructure — one transport, one
ticker, heartbeats between the same pair of nodes coalesced into one RPC.
`BalanceController` keeps their leaders spread evenly across machines.

`Config.Zones` and `Config.MinCommitZones` require a write to reach more than
one failure domain before it commits.

### Recovering a cluster that has lost its quorum

`RecoverCluster` rewrites a stopped node's durable state so that it restarts as
a member of a membership the operator names, which is the only way back for a
cluster that has permanently lost a majority. It is not a consensus operation
and it cannot be made one, so the API is built around bounding and reporting
what it risks: `InspectStorage` and `RecoveryInfo.UncommittedBand` show which
entries are in doubt before anything is written, `WithKnownCommitted` narrows
that band, `DiscardUncommitted` takes the other side of the trade, and
`RecoveryReport` records exactly which indices were promoted or discarded.

### Observability

`Node.Events` reports what happened, to whom, and when — which peer joined,
which follower stopped answering, which snapshot failed and why. Delivery is
bounded and lossy so the node never blocks on a slow consumer, and
`Event.Dropped` says how much was missed.

`Config.Metrics` and its optional extensions report state changes, commit
progress, snapshots, durable-write latency, proposal latency, and apply-loop
saturation. `metrics/prommetrics` implements all of them;
`metrics/rpctracer` covers per-RPC tracing.

### Storage, transport and discovery

`storage/filestore` is the durable backend: CRC32 per entry, fsync before
return, segment rotation, crash recovery, and an exclusive lock so two stores
cannot open one directory. `storage/memstore` is for tests.

`transport/grpctransport` is the production transport, with TLS and peer
authorization, heartbeat batching for multi-raft, and a wire contract pinned by
a test. `transport/memtransport` runs a cluster in one process.

`discovery` with `udpbroadcast` and `dnsdiscovery` implementations.

### easyraft

A batteries-included layer over the core package: transport, storage,
snapshotting, leader routing, cluster joining, typed collections, atomic
multi-collection transactions, change watching, and an HTTP API.

### Examples

Seven worked services and an operator tool, each covering a different part of
the library: `idprovider`, `ratelimiter`, `configsvc`, `ledger`, `shardkv`,
`durablekv`, `watchtower`, and `raftctl`.

### Testing

Alongside the ordinary tests, the root package runs the cluster against a
seeded simulated network that drops, reorders, delays and partitions, checking
the Raft invariants after every step, plus a linearizability check over the
resulting history. A scheduled job runs that at length and repeats the fast
suite to surface flakes.
