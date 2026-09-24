# Changelog

Notable changes, newest first. This project follows
[semantic versioning](https://semver.org/); what a version number promises is
spelled out in [`docs/compatibility.md`](docs/compatibility.md).

## v2.1.2

Examples and tests only. No library code changed, so a `v2.1.1` deployment
upgrades by changing the version and nothing else.

The example cluster scripts had drifted into nine variations on one idea, and
one of them did not work at all. They now share a single library, and every
one of them can walk through what its example demonstrates rather than
printing commands nobody ran.

### Fixed

- **Two flaky lease-read tests.** `TestReadIndexLease_ExpiryFromSendTime`
  failed about five times in a hundred, and the nightly soak's own fix for it
  had not gone far enough. It simulates a round-trip by advancing a manual
  clock when a read barrier reaches a follower, and it took "the first barrier
  to arrive" to be the round it was timing -- but the round that waits for the
  no-op sends a request to every follower, and one of those can still be in
  flight when the timed round begins. The clock was then advanced by a
  straggler, before the timed barrier was sent rather than after, leaving the
  lease anchored 100ms later than the test believed and still valid when the
  assertion expected it expired. Barrier generations are monotonic and travel
  on every request, so the round is now identified by its generation. 150
  clean runs, from five failures in sixty.

  `TestReadIndexLease_FollowerForwarding` assumed leadership would not move
  between electing a leader and asking a follower to forward to it. On a busy
  machine it does, and the forward is answered `ErrNotLeader` by a node that
  has since stepped down -- which is not a failure of forwarding. It now
  re-reads the leader on each attempt. CI caught this one; four hundred local
  runs did not.

- **`examples/tenants/cluster.sh` exited instead of starting a cluster.** The
  readiness loop counted leaders with `grep -o ... | wc -l`, and grep exits 1
  when it matches nothing. Under `set -o pipefail` that becomes the status of
  the assignment, which `set -e` treats as fatal -- so the loop killed the
  script on its first pass, before a single dot, because at that point no
  group had elected anything yet. The same shape was latent in four other
  scripts, which survived only because the string they happened to grep for
  is always present.

### Changed

- **The example `cluster.sh` scripts all behave the same way now**, from one
  shared `examples/internal/clusterlib.sh` rather than nine copies that had
  drifted. Each waits for readiness, shows the cluster, prints how to
  exercise it, and stays up until Ctrl-C. A node that dies at startup is
  reported by name with the end of its log, rather than waiting out a
  timeout and printing a table of zeroes.

- **`./cluster.sh --demo`** runs what the example demonstrates instead of
  printing it: each step says what it is about to show and why that matters,
  then runs it and shows the output. It exits non-zero if any step fails, so
  it doubles as an end-to-end check.

  Both modes read from one list of steps. Printed commands and executed ones
  cannot drift apart, which is how a command in a README outlives the
  endpoint it calls.

## v2.1.1

Three fixes, no API change. Two came out of the nightly soak, which repeats
the suite to catch what fails one run in several; the third was reported by
someone running an example.

### Fixed

- **A lease read arriving before the leader's no-op committed was answered
  with a quorum round instead of being declined.** `ReadIndexLease` has a
  two-outcome contract -- answer from a valid lease, or report
  `ErrLeaseExpired` so the caller can fall back -- and `handleReadIndex`
  checked `leaderNopCommitted` before it looked at the lease, so a read in
  that window was queued as an ordinary one and answered later by the full
  barrier round the caller had opted out of. On a leader that has just lost
  its followers, "later" is until the context expires. It now reports
  `ErrLeaseExpired` there. Nothing is served that was not served before: the
  request is declined rather than answered, so Raft §8 is untouched and the
  fallback every caller already has does the rest.

- **The simulation's invariant checker asserted on a torn read.** The soak
  reported 41 Leader Completeness violations against one node, claiming it
  was missing every committed index it had ever held -- which is not what a
  safety violation looks like. `readLog` reads the durable log in three
  calls while the node keeps writing to it, and a truncate-and-re-append can
  shorten the range between the second call and the third. The error was
  swallowed and returned as nil, which is indistinguishable from a node
  holding nothing. The engine was never at fault.

- **`examples/tenants/cluster.sh` started 1000 Raft groups and then tried to
  execute a directory.** `GROUPS` is a bash special variable holding the
  user's group IDs, so `GROUPS=9` was silently ignored and `--groups
  "$GROUPS"` passed the user's primary group ID. Before that could even be
  reached, the binary was built to a path that is the example's own package
  directory -- `go build -o` into an existing directory writes the binary
  inside it and reports success -- so every node died at startup with an
  error that went only to its log file. The wait loop printed dots either
  way; it now checks the nodes are alive and prints the log of the first one
  that is not.

## v2.1.0

Everything here is additive. A `v2.0.0` deployment upgrades by changing the
version and nothing else: no import path change, no option that has to be
passed, no behaviour that was relied on and is now different.

The theme is the batteries `easyraft` was missing. A store could replicate a
value but not compare-and-swap it, not expire it, not page through it, not
back it up, and not be talked to from a process that was not part of the
cluster. It can now do all five. The engine underneath is unchanged except
for one new accessor.

Three of the fixes below were found by building those features rather than
by a bug report, which is the most useful thing to say about them: a leaked
Raft listener, a client that could not outlast a leader election, and a
group ID that produced a Raft group nothing could ever reach.


### Added

- **`examples/tenants`**, a multi-tenant store where every tenant is its own
  Raft group, hosted many-to-a-node by an `easyraft.Manager`. It is the only
  example built on the `Manager`, which until now was undemonstrated
  entirely, and it is the worked example for `WithLeaderBalancing`,
  `WithSharedWAL` and `client.WithGroup`.

  It has no CRUD routes of its own: the `Manager` already serves them, and
  what the example adds is the part the library cannot decide, which is which
  group a tenant belongs to. The mapping is `1 + hash%groups` rather than
  `hash%groups`, because a group numbered zero would never be reachable, and
  a test pins that across every group count from 1 to 16.

- **`examples/ledger` gained a guarded transaction.** Transfers are posted
  into an accounting period, and each transfer's batch carries
  `Txn.CheckRev` on that period's revision -- so one validated while the
  books were open cannot commit after they closed. A per-operation condition
  could not express it: the transaction does not write the period, it only
  depends on it, which is the gap `CheckRev` fills.

- **`examples/configsvc` gained compare-and-swap.** A `GET` returns the key's
  revision as an `ETag`, `If-Match` makes the next write conditional on it,
  `If-None-Match: *` is create-if-absent, and a failed condition answers
  `412`. The Go client gained `GetRev`, `SetIf` and `DeleteIf`.

  Which number the condition uses is the point the example now makes: the
  revision, not the `Version` field it already exposed. A timestamp is the
  wrong thing to compare against, because two writers in the same nanosecond
  get the same one and a clock that steps back produces one already used.

- **`examples/serviceregistry`**, a service registry where instances register
  themselves under a lease and disappear when they stop renewing it. It is the
  worked example for key leases, `KeepAliveLoop`, prefix scans and pagination,
  and the `easyraft/client` package -- the thing that registers is a separate
  program talking to the cluster from outside it, so the registry has no
  registration endpoint at all.

  The two ways an instance leaves are written differently on purpose: a
  SIGKILL runs no code and the entry expires, while a clean stop revokes the
  lease and the entry goes at once.

- **Leader balancing on a `Manager`.** `WithLeaderBalancing(hosts, interval)`
  keeps group leadership spread across the hosts given, moving it when it
  bunches up. Groups elect leaders independently and nothing coordinates
  them, so a host that stayed up while others restarted ends up leading most
  of them -- and the leader does the replication, serves the linearizable
  reads and takes every write.

  A host's ID is the node ID its `Manager` was built with, since one
  `Manager` is one physical node. This node is asked directly; the others
  over HTTP at `/__balance/status` and `/__balance/transfer`, behind the same
  authorization hook as every other route and carrying whatever credential
  `WithBearerTokenAuth` set. A configuration that would silently balance
  nothing -- this host missing from the list, fewer than two hosts, no HTTP
  listener, an address that is not one -- fails at `Start` with the reason.

  A transfer is an election, so the point is a balanced cluster rather than a
  perfectly balanced one: a group is left alone for a cooldown after it
  moves, a round where any host fails to report is skipped rather than
  planned from a partial view, and leadership only moves to a voter close
  enough to the leader to take over. Off unless asked for.

- **Backup and restore.** `Store.Backup` writes a cluster's state to an
  `io.Writer` and reports the revision it describes; `Store.Import` replaces a
  cluster's state with one. Over HTTP they are `GET /__backup` and
  `POST /__restore`, wrapped by `client.Backup` and `client.Restore`.
  `BackupStale` reads from a follower without disturbing the leader, and is
  still internally consistent because it is taken under one lock.

  The swap is a single log entry. A backup can be larger than any one entry
  may carry, so the bytes are sent in chunks that accumulate in the state
  machine and are decoded by the entry carrying the last of them. Every
  replica swaps at the same point in the log, none is ever half-imported, and
  a failure at any chunk leaves the old state exactly as it was.

  The staged bytes travel in snapshots. They have to: a replica that restored
  from one mid-import and came back with nothing staged would apply the final
  chunk differently from every other replica, which is divergence with no
  error and nothing in the log to explain it.

  The README says the two things an import does not do: it does not stop
  writes, and it is not free on memory -- while one is in flight the cluster
  holds the encoded backup on top of the state it is about to replace, on
  every replica.

- **`raft.Node.MaxProposalBytes`** reports the largest command `Propose` will
  accept, and **`easyraft.WithMaxProposalBytes`** sets it. A caller splitting
  a large piece of work across proposals needs the number rather than a guess
  at it: a guess that is too big fails at the worst moment, and one that is
  too small makes an operation take many times the entries it should.

- **The in-memory ceiling is now visible.** `Store.StateBytes()` and
  `Store.KeyCount()` report how much application state a replica is holding,
  exported as `easyraft_state_bytes` and `easyraft_state_keys` with the same
  `{group, node}` labels the engine's metrics carry. Both are published from
  the moment a node starts, since a gauge that appears only once something
  happens cannot be alerted on when nothing does.

  The size is a running total kept in step by each write rather than a walk
  of every collection, because a walk is exactly what a store big enough for
  the number to matter cannot afford per scrape. It counts each key plus its
  encoded value, and the README says plainly that this undercounts the
  process's real footprint -- Go map overhead sits on top -- while being
  exact about growth, which is the part worth alarming on. The README also
  says what to do when the state will not fit: a disk-backed
  `raft.StateMachine` under the engine, giving up what this package layers on
  top.

- **`easyraft/client`**, a Go client for talking to an easyraft cluster from
  a process that is not part of it. It finds the leader, follows it when it
  moves, retries what is safe to retry, and returns the same error values an
  in-process caller sees -- `ErrKeyNotFound`, `ErrRevisionMismatch`,
  `ErrLeaseNotFound` and the rest -- so a service that moves from embedding a
  node to talking to one changes where its handle comes from and nothing
  else. `Coll[T]` mirrors `Collection[T]` method for method; leases, batches
  and cluster information are on the `Client`.

  What it will not do is retry a write that could apply twice. A caller
  cannot tell a request that never arrived from a response that was lost, so
  reads are retried, writes carrying an exactly-once identity are retried
  because the cluster deduplicates them, and conditional writes are retried
  because a second attempt is refused by the revision the first one moved.
  An ordinary `Create` with no identity is attempted once. Following a
  redirect is not a retry and always happens.

- **Exactly-once writes over HTTP.** `X-Raft-Client-Id` and `X-Raft-Seq` on
  any write route make it deduplicated, which is what lets a retry after a
  lost response be safe rather than a second write. They cover every write
  including `/batch`, mutations and a lease grant -- a repeated grant used to
  leave a second lease nothing would ever renew. Both headers or neither: one
  of them is a client that meant to be deduplicated and is not, which is
  answered with `400` rather than applied as an ordinary write.

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

- A node now advertises its HTTP address with an exponential backoff starting
  at 50ms rather than retrying on a fixed 5-second timer. The first attempt
  almost always fails, because `Start` runs before any election has finished
  and there is nobody to propose to yet -- so every cluster spent five
  seconds after startup answering writes on its followers with a 503 naming a
  leader they could not redirect to. It is now a few hundred milliseconds.

- `NewStore` now builds its `Store` through `newStoreShell`, which it already
  documented itself as doing while keeping a second copy of the same literal.
  The two had not drifted, but the previous time two construction paths
  assembled the same value separately one of them quietly fell behind by
  every field added to the other.

### Fixed

- `Manager` ignored `WithHTTPMux`. A `Store` has always registered its routes
  on a mux the caller supplied, so an application can serve its own routes on
  the same port; a `Manager` always built one of its own, so the option
  compiled, read as set, and did nothing. An application that wanted one port
  had no way to say so and no way to find out it had failed to -- easyraft's
  routes simply were not there.

- `easyraft/client` gave up before a leader election finished. The default
  retry budget was four attempts doubling from 50ms -- about 350ms in total
  -- while an election takes an election timeout, one to two seconds with the
  default Raft timings. Since `503` with *no leader currently elected* is the
  commonest retryable answer, the client failed at exactly the moment it
  exists to paper over. It is now six attempts from 100ms, about two and a
  half seconds, exported as `DefaultRetryAttempts` and `DefaultRetryBackoff`.
  `WithRetry` still lowers it.

- `Manager.AddStore(0)` is now refused rather than accepted. A `Manager`
  routes every inbound RPC by the group ID it carries, and zero is what a
  single-group node's RPCs carry, so the transport refuses it: a group
  numbered zero got no votes, no appends and no election, and sat in
  `PreCandidate` for ever with nothing in its own log to say why. The error
  says so and says to number groups from one. Refusing a call that used to
  be accepted is not a breaking change here: no cluster can have been
  relying on a group that never took a write.

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
