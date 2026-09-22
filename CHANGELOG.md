# Changelog

Notable changes, newest first. This project follows
[semantic versioning](https://semver.org/); what a version number promises is
spelled out in [`docs/compatibility.md`](docs/compatibility.md).

## Unreleased

### Added

- **`easyraft/easyrafttest`**, which runs real easyraft clusters inside a
  test process on an in-memory network a test can cut and heal. Every node is
  a real `Store` running the real engine; the only thing replaced is the wire
  between them, so a test of a service built on easyraft tests the service
  rather than a mock of the library under it.

  `NewCluster(t, 3)` builds the nodes, starts them, waits for a leader and
  registers the shutdown with the test; `AddCollection[T]` gives the same
  typed collection on every node, with `Leader()` for writes and `Node(i)`
  for reads from a follower. `Partition`, `Heal`, `Drop`, `Restore`,
  `StopNode` and `RestartNode` cause the failures the rest of this library
  exists to survive, and `WaitLeader`, `WaitNoLeader`, `WaitApplied` and
  `Ready` wait for the cluster to reach a state rather than leaving each test
  to poll. Default timings make an election tens of milliseconds.

- **`WithTransport`**, the seam that package is built on. A store uses the
  transport it is given instead of listening on `WithRaftAddr`, and never
  closes it: something that may outlive the store, or be shared by several,
  is the caller's to close. It is also how a transport written for an
  environment gRPC cannot reach plugs in.

- **Key leases in easyraft.** `Store.GrantLease` creates a lease with a time
  to live; keys written under it with `Collection.CreateWithLease` or
  `UpsertWithLease` are deleted together when it expires or is revoked.
  `KeepAliveLoop` renews one from a single goroutine until its context is
  done, which is the service-registry pattern: a process registers itself and
  its entry goes away on its own when it stops or is partitioned, with no
  other process having to notice that it died. Over HTTP the lease lives at
  `/__leases`, and a write attaches to one with `?lease=`.

  Expiry is a proposal by the leader, not something each replica decides for
  itself. `Apply` may not read a clock -- it runs on every replica at
  different times, and again on a replica replaying its log days later -- so
  a state machine that deleted keys when it noticed the time had passed would
  hold different state on every node. The leader proposes a revocation when a
  lease falls due and every replica removes the keys at the same point in the
  log. What that costs is precision, and the README says so plainly: a key
  may outlive its TTL by the sweep interval plus the disagreement between two
  clocks, and may go early after a leader change to a node whose clock runs
  ahead.

  A lease ID is the index of the entry that granted it: unique across the
  cluster for the life of the log, agreed by everyone, and costing neither a
  replicated counter nor a random number two nodes could collide on. A key
  belongs to exactly one lease, a write naming no lease detaches it, and a
  write under a lease that is gone writes nothing rather than leaving a key
  with nothing to remove it. Leases travel in snapshots; a snapshot written
  before this change restores as a store with none.

- **Prefix scans and pagination in easyraft.** `Collection.Scan` returns a
  collection one page at a time in ascending key order, narrowed by a prefix,
  and `ListPrefix` is the one-call form for a result small enough to hold in
  memory. Both have `Stale` variants, and both are on the `EasyRaft[T]`
  wrapper. Over HTTP, `GET /{collection}` takes `prefix`, `limit` and `after`,
  answers with the same JSON object it always did, and carries the cursor in
  `X-Raft-Next-Cursor` and a `Link` header with `rel="next"`.

  The cursor is a key rather than an offset. An offset shifts under every
  insert before it, so a page boundary would skip or repeat keys whenever the
  collection changed between pages; a key does not, so nothing already
  returned can come back. A cursor is set only when a key was actually left
  behind, so a final page that happens to fill the limit ends the scan rather
  than sending the caller back for a page that could only be empty.

- **Compare-and-swap in easyraft.** Every key now carries a revision -- the
  index of the log entry that last wrote it -- and every write can be made
  conditional on it. `Collection.ReadRev` returns a value with its revision;
  `UpdateIf`, `UpsertIf`, `DeleteIf` and `MutateIf` apply only while the key
  is still at the revision given, and return the new `ErrRevisionMismatch`
  when it has moved.

  This closes the one race a store like this leaves open: a `Read` followed
  by an `Update` is two log entries, and a write that lands between them is
  overwritten with no error. A registered mutation already avoided that, but
  only for a value computable inside the state machine. A conditional write
  covers the rest -- a value decided in a browser, in another service, or by
  a person -- without a lock.

  The condition is evaluated on every replica as the entry applies, not on
  the leader before it proposes, so it holds against every other entry in
  the log whatever order they commit in.

  - `Txn.CheckRev` guards a whole transaction on a key it does not write, and
    `Txn.UpdateIf` / `UpsertIf` / `DeleteIf` carry the same condition on keys
    it does. A failed condition fails the batch and writes none of it.
  - Over HTTP the existing headers carry it: a single-key `GET` returns the
    key's revision as an `ETag`, `If-Match` makes a `PUT`, `PATCH`, `DELETE`
    or mutation conditional on it, `If-None-Match: *` requires that the key
    does not exist, and a failed condition answers `412 Precondition Failed`.
    A batch operation may carry `if_rev`, and the new `check` operation
    carries nothing else. Anything the store cannot check exactly -- a weak
    validator, a list of ETags, `If-Match: *` -- is refused with `400` rather
    than applied unconditionally, since guessing which one the caller meant
    is the lost update the feature exists to prevent.
  - `Store.Revision()` reports the highest revision this replica has applied.
  - Revisions travel in snapshots under two reserved names beside the
    collections, spelled like every other internal name in the package so
    that no collection can collide with them. A replica restored from one
    refuses and accepts exactly what the replica that wrote it would. A
    snapshot written before this change has neither name and restores as a
    store whose keys have never been written, so an upgrade needs no
    migration.

- easyraft can reach the batteries v2 added to the engine. Every one of them
  was unreachable through the high-level layer, which is the layer most
  services use.

  - **Witnesses.** `WithWitness` builds one, `WithWitnessPeers` tells the
    full replicas which member it is, and `Store.AddWitness` brings one into
    a running cluster. A witness holds no data, so reads against it return
    the new `ErrWitness` rather than the empty answer its collections would
    otherwise give.
  - **Quorum and placement.** `WithCommitQuorum`, `WithZones`,
    `WithPreferredLeader`, and `Store.CommitQuorum` /
    `Store.SetCommitQuorum` for the policy a running group has agreed.
  - **`WithMaxClientTableSize`**, with `Store.MaxClientTableSize` and
    `Store.SetMaxClientTableSize` for the bound the group has agreed.
  - **`WithLeaseSafetyMargin`**, `WithProposalQueue` and `WithOnRemoved`.
  - **`WithTLSFiles(cert, key, ca)`** builds the Raft transport's mutual
    TLS from PEM files, which is the configuration easiest to get wrong: the
    authority has to be trusted in both directions, since every node is both
    a client and a server, and client certificates have to be required *and
    verified*, since anything weaker leaves the Raft port open to any client
    that can reach it. The files are read at construction, so a wrong path
    fails there rather than at the first connection between two nodes.
  - **`Store.Shutdown(ctx)` and `Manager.Shutdown(ctx)`**, which stop like
    `Stop` but give up waiting when the context is done. `Stop` finishes
    what it started -- accepted storage writes, and a departure under
    `WithLeaveOnStop` -- and a hung disk or an unreachable leader holds it
    there, which a process coming down on a deadline cannot afford.
  - **`WithSharedWAL`** puts every group on a `Manager` onto one
    write-ahead log rather than one per group, so a burst of appends across
    groups costs one `fsync` instead of one each.
    `Manager.SharedWAL()` exposes the log itself, whose `Groups` is how a
    host finds its groups again after a restart.
  - **`Store.Events`**, the stream of what the node does: leadership
    changes, peers arriving and leaving, snapshots, a client dropped from
    the exactly-once table, a durable write that failed.

### Changed

- `NewStore` now builds its `Store` through `newStoreShell`, which it already
  documented itself as doing while keeping a second copy of the same literal.
  The two had not drifted, but the previous time two construction paths
  assembled the same value separately one of them quietly fell behind by
  every field added to the other.

### Fixed

- `Store.Stop` left the Raft listener open. `Node.Stop` unregisters the
  node's handler but does not close the transport -- correctly, since a
  transport can be shared by several groups -- and the store only closed one
  it had never started. A service that restarts its store in process could
  not rebind its Raft address, and one that creates and destroys stores
  leaked a listener and its goroutines every time. The store now closes the
  transport it opened, whether or not the node ran, and a `Manager`'s groups
  still leave the shared one to the `Manager`.

- `grpctransport.Close` could return while the address was still taken.
  `GracefulStop` closes the listeners the server is serving on, but `Listen`
  hands the listener to `Serve` in a goroutine; a `Close` that arrived before
  that goroutine ran found a server with no listener, and the port was
  released some tens of milliseconds later when `Serve` discovered the server
  was already stopped. `Close` now closes the listener itself, so the address
  is free by the time it returns.

- A single-voter leader answered a linearizable read before committing
  anything in its own term, which after a restart meant answering from a
  commit index of zero. The commit index is not persisted -- a restarting
  node learns it again from its leader, and a single-node cluster's leader
  is itself -- so it comes back at zero with a log full of entries that
  certainly did commit, and `ReadIndex` returned that zero. A client that
  wrote, restarted the node and read back saw an empty state machine. The
  single-voter fast path now waits for the leader's no-op like every other
  leader, and resolves without a confirmation round only afterwards, since
  a node that is the whole cluster has nobody to confirm with. Found
  through easyraft, where several groups sharing one write-ahead log made
  the window wide enough to hit.

- Every setting easyraft added to a `Store`'s Raft configuration was missing
  from the `Manager`'s, which assembled its own copy. The two now go through
  one function, so a knob cannot exist for one kind of deployment and
  silently do nothing for the other.

- `easyraft.WithTLS` authenticated the connection and then trusted whatever
  the peer claimed to be. Every Raft RPC names the node it comes from, and
  nothing was binding that name to the certificate that carried it, so any
  holder of any certificate from the configured CA could claim to be the
  leader and make the cluster step down. A configuration whose `ClientAuth`
  is `tls.RequireAndVerifyClientCert` now also gets
  `grpctransport.MTLSPeerAuthorizer`. Weaker settings get none, because a
  check against a certificate that may not be there would refuse every RPC,
  and instead log a warning that the Raft port is open to any client that
  can reach it. The new `easyraft.WithPeerAuthorizer` replaces the default
  for certificates that carry node identity somewhere other than the Common
  Name or DNS names.

## v2.0.0

The import path is now `github.com/brunoga/raft/v2`, because this release
withdraws a promise `v1` made: a transport or an HTTP API that was open by
default now refuses to start unless the exposure is asked for by name. That
is a deliberate break and Go says a break means a new major version, so the
import path carries the `/v2` suffix. Everything else here is additive.

Upgrading is two steps: add `/v2` to the import paths, and give each listener
the security decision it now insists on -- `WithTLSConfig` or `WithInsecure`
for the Raft transport, an authorizer or the matching acknowledgement for the
HTTP endpoints. A deployment that was relying on the old defaults behaves
exactly as before once the acknowledgement options are passed.

Several changes below also alter what goes on the wire or into a snapshot, in
each case by adding something older nodes ignore. The individual entries say
what that means for a rolling upgrade; the short version is to upgrade every
node before using the feature that needs it.

### Added

- The proposal queue is configurable. `Config.ProposalQueueSize` sets how
  many proposals may wait for the event loop at once (the previous fixed
  1,024 is now the default), and `Config.ProposalOverflow` decides what
  `Propose` and `ProposeOnce` do when it is full: wait, as before, or return
  the new `ErrProposalQueueFull` at once so that a caller can shed load
  rather than hold a goroutine on it. `Node.ProposalQueueDepth` reports the
  occupancy, which is the number to alert on before either happens.

- `Config.OnRemoved`, a callback invoked once, from its own goroutine, when a
  committed configuration change removes this node from the cluster. A
  removed node is deliberately not stopped, so that whatever owns it decides
  what to do with it; this is where that decision goes, and it may call
  `Stop`.

- `Config.LeaseSafetyMargin` bounds what a lease read assumes about clocks.
  It is taken off the `ElectionTimeoutMin` lease, so a follower whose clock
  runs a little fast cannot hold an election inside a lease the leader still
  believes in, and it is the tolerance beyond which wall-clock time running
  ahead of monotonic time — which is what a paused or live-migrated VM looks
  like on resume — drops the lease rather than serving from it. `DefaultConfig`
  sets 15 ms; zero keeps the previous behaviour exactly.

- The exactly-once table's bound is replicated. `Config.MaxClientTableSize`
  had to be identical on every node of a group, and a node with a different
  value diverged silently: it evicted different clients, re-ran a retry its
  peers deduplicated, and nothing anywhere noticed. The bound is now written
  into the log by the leader ahead of the first `ProposeOnce` entry and
  carried in every snapshot, so every replica keeps the same table whatever
  its own `Config` says (a mismatch is logged as a warning and otherwise
  ignored). `Node.MaxClientTableSize` reports the value in effect;
  `Node.SetMaxClientTableSize` changes it for the whole group as a
  configuration change.

  On the wire this is a new config-entry opcode and a new snapshot framing
  version; both older framings are still read. Upgrade every node before a
  new-version leader is elected with `ProposeOnce` traffic, as a node on the
  previous version ignores the opcode and keeps its own bound.

- `storage/sharedwal`: one write-ahead log for every Raft group on a host.
  Every group appends to the same log and one goroutine writes everything
  waiting and syncs once per batch, so a burst of appends from many groups
  costs one `fsync` rather than one per group — the multiplier that made
  `filestore` the binding constraint at high group counts. Records carry
  their group and a checksum; a torn tail is cut off on open; snapshots live
  in one file per group; segments are reclaimed once nothing needs them,
  with the little that keeps an old one alive copied forward. `Storage(id)`
  returns a value implementing `raft.Storage`, `raft.BatchWriter` and
  `raft.CommitRecorder`; `Groups` lists the groups a log holds and `Remove`
  forgets one. The README's multi-Raft scale notes and the divergence doc's
  limitations are updated accordingly.

- Flexible quorums. `Node.SetCommitQuorum(ctx, q)` sets how many voters an
  entry must reach to commit, for the whole group; the election quorum
  becomes `voters − q + 1`, never below a majority, so the two always
  intersect and Leader Completeness holds unchanged (Howard, Malkhi and
  Spiegelman, *Flexible Paxos*). A commit quorum below a majority makes
  writes cheaper than elections; one above a majority, up to every replica,
  means no acknowledged write is ever on fewer than that many disks. The
  majority floor on elections is Raft's own requirement: two election
  quorums must intersect each other for a term to have one leader. The policy is
  group state, agreed through the log and carried in snapshots;
  `Config.CommitQuorum` is only what a group is created with, and no node
  ever counts by its own `Config`. A change is safe on a running group: a
  node holding the new policy in its log requires the stricter of old and
  new for every decision until the entry is applied. `Node.CommitQuorum`
  reports the value in effect.

  On the wire this is a new config-entry opcode and a trailing field in the
  snapshot membership section that older readers ignore. Upgrade every node
  before setting a policy; a node on the previous version counts by a
  majority regardless.

- Witnesses (dissertation §11.7.2). `Config.Witness` builds a node that
  keeps the index and term of every entry and never the entries themselves,
  needs no state machine, and applies nothing; `PeerConfig.Witness` marks
  it in the membership and `Node.AddWitness` adds one to a running group. It
  votes and counts towards every quorum, so two full replicas and a witness
  survive the loss of any one member. The leader sends it entries stripped
  to their shape and a snapshot of a few hundred bytes. A witness cannot
  lead, be transferred leadership, or be the source of a state transfer.

  The leader prefers full replicas: a witness's acknowledgement counts
  towards a commit quorum only while a full voter that lacks the entry has
  stopped answering, so in a healthy group every committed entry is on every
  full replica and the witness stands in for a replica that is down, not for
  one that is slow. `New` refuses a node whose recovered membership disagrees
  with `Config.Witness`; a full node that applies a membership entry calling
  it a witness stops with the new `ErrWitnessMismatch`. `GroupStatus` and
  `PeerProgress` report `Witness`, and the leader balancer never targets one.

  On the wire the witness role is a second bit in the peer role byte; a node
  on the previous version reads it as a non-voter, so upgrade every node
  before adding a witness.

### Changed

- **Breaking: nothing listens open by accident any more.** Every listener
  that was plaintext or unauthenticated by default now refuses to start
  unless that is asked for explicitly. A Raft peer is fully trusted, so an
  open transport is a cluster anyone who can reach the port can take over;
  the previous behaviour was a warning in the log, which is the wrong place
  for the only notice.

  - `grpctransport.Listen` returns the new `ErrNoTransportSecurity` unless
    given `WithTLSConfig`, the new `WithInsecure` (plaintext, on purpose,
    still warned about once), or the new `WithCustomCredentials` (the
    credentials come through `WithServerOptions` and `WithDialOptions`).
  - `Manager.Handler` answers every request with `403 Forbidden`, and logs an
    error once, unless given `WithRequestAuthorizer` or
    `WithInsecureHandlerAcknowledged`.
  - `easyraft.NewStore` and `easyraft.NewManager` return an error when the
    Raft transport has no `WithTLS` and no new
    `WithInsecureTransportAcknowledged`, and when an HTTP API would be served
    (`WithHTTPAddr` or `WithHTTPMux`) with no `WithHTTPAuth` or
    `WithBearerTokenAuth` and no `WithInsecureHTTPAcknowledged`.

  Migration: a deployment that was relying on plaintext or an open API adds
  the matching acknowledgement option and behaves as before. The examples
  have been updated the same way. This is the one change in the series that
  breaks a working configuration on purpose, which is why it is called out
  here rather than folded into an "Added" entry.

### Fixed

- A node removed by the leader was usually never told. The leader dropped
  its heartbeat pump and progress tracking the moment it appended the
  removal entry, so unless the entry happened to reach the node first, it
  never saw the change commit: it went on believing it was a voter, timed
  out, and campaigned against a cluster that ignored it, for ever. The leader
  now keeps replicating to a removed peer — without counting it towards
  anything — until it has acknowledged a commit index covering its removal,
  bounded by a few election timeouts so that a peer removed because it is
  dead does not keep a pump for ever.

- A proposal's outcome is now reported to `ProposalMetrics` before the call
  that made it returns, rather than just after. A caller that scraped its
  metrics the moment `Propose` returned could miss the very proposal it had
  just made.

## v1.1.0

### Added

- Eviction from the exactly-once client table is now reported. It is the moment
  the `ProposeOnce` guarantee stops holding for a client — from there its next
  retry runs a second time — and until now it happened in complete silence:
  the command applies cleanly, the log stays consistent, every replica agrees,
  and the only evidence is whatever the duplicate did.

  - `ClientTableMetrics`, a new optional interface a `Config.Metrics` may also
    satisfy, with `ClientForgotten(id, clientID NodeID)`.
  - `EventClientForgotten`, on the stream from `Node.Events`, naming the client
    in the new `Event.Client` field.
  - A warning on `Config.Logger` for deployments that wire neither. The first
    eviction is always logged; the rest are summarised once a minute, because a
    table one entry too small evicts on every proposal.

- `prommetrics` exports both as `raft_clients_forgotten_total` (a counter that
  stays at zero in a cluster meeting its promise, so the alert is on any
  increase at all) and `raft_client_table_size` (read at scrape time from a
  tracked node). The forgotten client is deliberately not a label: a table one
  entry too small evicts on every proposal, which would mint a series per
  eviction.

- `Node.ClientTableSize` reports the table's current occupancy. The eviction
  reports above all arrive after the guarantee has lapsed; this one arrives
  before. While it stays below `MaxClientTableSize` no client is ever
  forgotten, so it is the value worth alerting on.

### Fixed

- `watchtower` could not be stopped with ^C. It printed its shutdown line and
  stayed there; pressing ^C again did nothing, and the only way out was
  `kill -9`. Two deferred calls waited on each other — the observer goroutine
  returns when the event channel closes, and the only thing that closes it was
  deferred *before* the wait, so it ran after. A second fault the first one was
  hiding: an attached `/events` viewer held shutdown for the whole ten-second
  timeout, because `http.Server.Shutdown` waits for in-flight requests rather
  than cancelling them.

- `configsvc` and `ledger` installed no signal handler at all, so ^C terminated
  them through the default disposition and every deferred call was skipped —
  including the `store.Stop()` that stops the Raft node and closes the log.
  Their shutdown path had never once run. Both now shut down gracefully, and
  `main` is the usual two lines over a `run() error` so that nothing calls
  `os.Exit` past a defer.

  The other four services — `durablekv`, `idprovider`, `ratelimiter` and
  `shardkv` — were checked the same way and were already correct.

- `examples/internal/shutdowncheck` runs a service end to end and requires a
  prompt, clean exit on a signal, with the line its cleanup logs. None of these
  faults are reachable from a normal test: they live in `main`, and from
  outside, a process killed by the default handler and one that shut down
  cleanly both simply stop.

- `TestRecover_BringsBackACluster` in `examples/raftctl` raced its own ticks
  against an election and could fail with `node is not the leader`. Ticks come
  from the loop waiting on each proposal, so a slow snapshot could outlast a
  follower's timeout while that loop generated a full election window. It now
  re-finds the leader and retries, as a client would.

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
