# Where this implementation diverges from the Raft paper

Raft is specified twice: in the 2014 USENIX paper *In Search of an
Understandable Consensus Algorithm* (Ongaro and Ousterhout), and at greater
length in Ongaro's 2014 dissertation *Consensus: Bridging Theory and Practice*.
Neither is a specification an implementation can follow literally and still be
correct in production, and every serious implementation departs from them
somewhere.

This document lists every place this library does. It exists because the
alternative is that you find out by reading the source, and because a
divergence nobody wrote down is indistinguishable from a bug.

Section numbers cite the dissertation unless stated otherwise.

---

## Additions the paper describes but does not require

These are optional extensions from the dissertation rather than departures, but
they change behaviour enough to be worth stating.

### Pre-vote (§9.6)

A node that has been partitioned and rejoins would otherwise raise its term,
force the leader to step down, and lose the election it just caused, costing
the cluster a term for nothing. A pre-candidate first asks whether it *would*
win without raising its term, and only stands if a majority says yes.

A pre-vote is denied by any node that has heard from a live leader recently,
and by the leader itself. It is granted or denied entirely from memory: it
records nothing, so it is the one message this implementation sends without
first waiting for a disk write. See "Asynchronous persistence" below.

### Check-quorum (§6.2)

A leader that cannot reach a majority steps down of its own accord after one
election timeout, rather than continuing to accept proposals that can never
commit. Without it, a partitioned leader keeps answering clients for as long as
the partition lasts.

### Leadership transfer (§3.10)

Exposed as `Node.TransferLeadership`. The leader catches the target up, stops
accepting proposals, and sends `TimeoutNow`, which makes the target campaign
immediately rather than waiting out an election timeout.

### ReadIndex and lease reads (§6.4)

Three read paths, named at the call site rather than selected by a config flag:

| Method | Cost | Guarantee |
|---|---|---|
| `ReadIndex` | one round of heartbeats | linearizable |
| `ReadIndexLease` | none, while the lease holds | linearizable if clocks behave |
| `ReadStale` | none | whatever this replica has applied |

`ReadIndexLease` is the one that trades a safety assumption for latency. It
relies on bounded clock drift between leader and followers. `ReadIndex` does
not, and is the default for anything that matters.

### Exactly-once client semantics (§6.3)

`ProposeOnce` carries a client identifier and a sequence number, and the result
of each client's most recent command is kept in a table that is included in
every snapshot. A retry after a leader change returns the original result
rather than applying the command twice.

The table is bounded by `MaxClientTableSize` and evicts least-recently-used
entries. **A client that retries after its entry has been evicted has its
command executed a second time.** This is a real limit, not a theoretical one:
size the table to outlive the retry window of your slowest client.

The bound cannot be removed. A client picks its own identifier, so a client the
node has forgotten and a client it has never seen are the same observation, and
telling them apart would mean remembering every identifier ever used — which is
not a bound at all, just a slower failure. What the bound costs is therefore
fixed; what is avoidable is finding out about it from the duplicate. Eviction
is reported three ways, at the moment it happens:

- `ClientTableMetrics.ClientForgotten`, for a `Config.Metrics` that implements it.
- `EventClientForgotten`, on the stream from `Node.Events`.
- A warning on `Config.Logger`, for deployments that wire neither. The first is
  always logged; the rest are summarised once a minute, because a table one
  entry too small evicts on every proposal.

Those all arrive after the guarantee has lapsed for that client.
`Node.ClientTableSize` arrives before: it is the table's occupancy, and while it
stays below `MaxClientTableSize` no client is ever forgotten and exactly-once
holds absolutely. Alert on it approaching the bound; treat a non-zero eviction
count as the deadline already missed.

---

## Divergences

### Asynchronous persistence

**The paper implies every durable write happens before the node acts on it.
Here the write is queued and the node carries on.**

Storage writes are carried out by a separate goroutine. The event loop never
waits for a disk, because a node that blocks in `fsync` stops counting election
ticks and stops answering its peers, which is indistinguishable from being
down: its followers time out and call an election against a leader that is
alive and healthy apart from one pending write.

What preserves the safety argument is that *every statement whose meaning is
"this is on disk" waits for the disk*, while everything else proceeds. There
are five such statements, and they are the whole of the contract:

1. A follower's acknowledgement of appended entries.
2. A leader counting its own log towards a commit quorum.
3. The commit index handed to the apply loop, which reads entries back from
   storage.
4. Any message carrying this node's term, because a node that forgets a term it
   acted on can decide that term a second time. The pre-vote is the single
   exception, and is safe because it promises nothing.
5. A follower's acknowledgement of a completed snapshot install.

A node that crashes before a write lands comes back without the entries and
without having acknowledged them, which is the case Raft already handles by
retrying.

Because appends no longer block, waiting for the disk is no longer the
backpressure either. `MaxUnstableLogBytes` replaces it: past that much
unwritten log, a leader refuses proposals and a follower refuses entries, both
with `ErrWriteBacklogFull`.

### A commit index can outrun the disk

Following from the above: a follower can have a commit index higher than
anything its disk holds, because the entry is in the log the moment it arrives
while the write is still queued. Nothing acts on that. Applying reads from
storage and stops where storage is known to hold the log's current entries, and
a commit index is not persisted, so a crash in that window brings the node back
with no commit index at all.

If you inspect a node's durable log directly, this is the discrepancy you will
see, and it is expected.

### Commit index is not persisted, but it is recorded

The paper treats `commitIndex` as volatile and this implementation agrees for
every purpose the protocol has: a restarted node learns its commit index from
its leader, and nothing in normal operation reads what was written down.

A `Storage` that implements `CommitRecorder` is nevertheless told, roughly
every 256 commits, how far the log had committed *and* reached this node's
disk. That record exists for the node with no leader left to learn from — the
survivor of a permanent quorum loss, whose log splits into a provably committed
prefix and a band that is a coin toss. Without it the only proof on disk is the
snapshot boundary, which can be thousands of entries back.

It is a lower bound, never an estimate. It is written without an fsync, since
losing the newest value only widens the band, and checksummed, since a torn
record could otherwise read back larger than anything that committed. A
recorded index never moves backwards, and is clamped to the log on the way out.

### Terms are indexed in memory, not read from the log

Terms never decrease with index, so the log is a short sequence of runs, one
per leadership epoch. Those runs are held in memory and answer every term
lookup without touching storage.

This matters because the naive implementation reads the log on every heartbeat,
on every inbound append, and once per index while working out where two logs
diverge. Those reads sit on the event loop and, in a file-backed store, contend
with the lock the writer holds across its `fsync`, so a loop that no longer
writes to the disk could still end up waiting for one.

The index is built once while the node is being constructed, by a scan of the
log that startup already performs to recover the cluster membership.

### The commit index never advances past what a request vouches for

§3.5 says a follower sets `commitIndex = min(leaderCommit, index of last new
entry)`. An implementation that clamps to its own last index instead will
commit whatever uncommitted suffix it still carries from a previous leader.
This implementation clamps to `prevLogIndex + len(entries)`, which is exactly
the range the request establishes as matching.

### A leader commits only entries from its own term

§3.6.2. An entry from an earlier term is never committed by counting replicas,
even when it is present on a majority; it commits only as a side effect of an
entry from the current term committing. The leader appends a no-op on election
so this happens promptly.

The commit scan is bounded below by the index of that no-op rather than walking
down to the commit index, since nothing below it can be committed by replica
count anyway.

### Membership changes use joint consensus, adopted at apply time

§4.3 for the mechanism. Two details are worth stating:

- **Adoption happens when the entry is applied, not when it is appended.** The
  dissertation describes adopting a configuration as soon as it is appended.
  Doing that here broke cluster growth: a single-node leader adding a voter
  immediately lost its own quorum and stepped down before the change could
  commit. The latest configuration in the log is still what is used when
  recovering a node from disk, where no such race exists.
- **One change at a time.** A second configuration change is refused while one
  is outstanding, with `ErrConfigChangeInProgress`.

### Flexible quorums

**The paper commits and elects on majorities. Here the two sizes are a group
setting, constrained only to intersect.**

Howard, Malkhi and Spiegelman ("Flexible Paxos") observed that the Leader
Completeness argument uses nothing about a majority except that a commit
quorum and an election quorum intersect, so a commit quorum of `Q` and an
election quorum of `N - Q + 1` serve it as well as two majorities.
`Node.SetCommitQuorum` sets `Q` for the group through a config entry; every
quorum decision -- commit, election, check-quorum and read confirmation --
goes through one pair of functions in `quorum.go`.

Raft needs one thing more than Paxos does: two election quorums must
intersect *each other*, so that a term has at most one leader. Paxos gives
each proposer its own ballot numbers; Raft shares terms, and its Log Matching
property -- an index and a term name one entry -- rests on one leader per
term. A simulated cluster with an election quorum of two out of five produced
two leaders in one term within seconds. So the election quorum is never below
a majority, and the one trade this cannot make is cheaper elections: a group
that writes to every replica still elects on a majority, and what it buys is
that no acknowledged write is on fewer than every disk.

The subtlety is the change itself. Membership here is adopted at apply time,
and that is safe for membership because a single-server change keeps every
pair of majorities overlapping. A quorum policy has no such property: a node
holding a relaxed commit quorum in its log but still counting elections by the
old, smaller election quorum could elect itself without an entry the new
commit quorum had already committed. So a policy is in force from the moment
it is appended, as the *stricter* of old and new for every decision, and only
relaxes when the entry is applied. The entry therefore commits under the larger
of the two commit quorums, which is on every node the new election quorum can
be drawn from.

### Witnesses stand in for a replica that is down

§11.7.2 describes a *witness*: a voter that stores the log's metadata and none
of its entries. `PeerConfig.Witness` and `Config.Witness` are that. A learner
(`Voter: false`) is its opposite -- the full log and no vote -- and the two
are distinct roles.

The divergence is in how a witness's acknowledgement is counted. The
dissertation counts it like any voter's. Here a leader counts it towards a
commit quorum only while some full voter that lacks the entry has stopped
answering. Counting it always would let a fast witness and a slow full
replica commit entries that live on one full disk; if that disk then fails,
no survivor holds the entries and no full replica behind them can be elected,
so the group is stuck until the failed replica returns. Preferring full
replicas keeps every committed entry on every full replica in a healthy
group, at the cost of waiting for a slow replica instead of the witness. The
witness earns its keep when a replica is actually down, which is what it is
for.

### A failed durable write stops the node

Raft assumes storage does not fail. When it does, this implementation stops the
node rather than continuing, because a node that cannot persist its term, its
vote or its log can no longer honour the assumptions the safety argument rests
on. `Node.FatalError` reports why, and `Config.OnFatal` is called once.

### Snapshot installs are acknowledged only once durable

The final chunk of a snapshot is not acknowledged until the snapshot is on
disk, because the leader records that acknowledgement as the receiver's match
index. A chunk this node discards is answered with an error rather than a bare
response, so the leader retries rather than believing the snapshot arrived.

### Proposals are refused rather than accepted and lost

A command too large to replicate is refused with `ErrProposalTooLarge` rather
than appended to a log it can never leave. Without this, the entry is retried
for ever, every later proposal queues behind it, and the group stops making
progress with nothing having reported an error.

### Quorum loss has an escape hatch outside consensus

The paper has no answer for a cluster that loses a majority of its voters
permanently. There is none within consensus: every operation that could shrink
the cluster back to a size the survivors are a majority of needs a majority to
commit it.

`RecoverCluster` rewrites a stopped node's durable state from outside the
protocol, appending a configuration entry in a term above any the node has seen
so that the restarted node adopts a membership it can form a quorum in. It is
deliberately outside the safety argument, and it is worth being precise about
which parts of that are unavoidable.

Losing entries the dead majority committed and this node never received is
unavoidable by anything: a committed entry is guaranteed to be on a majority,
so losing a majority can lose it outright, and no algorithm recovers what no
surviving disk holds.

Everything above the highest index the node can prove was committed is a
second, bounded problem. Each such entry either committed on the majority that
died or was in flight when it did, and nothing that survives distinguishes
them. Recovery either keeps that band, promoting entries that may never have
committed, or discards it, throwing away entries that may have. Both are
wrong in a different direction, so the choice is the operator's:
`DiscardUncommitted` selects the second.

What the implementation does about it is bound the band and report it.
`RecoveryInfo.KnownCommittedIndex` is the snapshot's last included index, since
a snapshot is taken at an applied index and applying follows committing;
`WithKnownCommitted` raises that floor with a durable state machine's applied
index, which is usually within a few entries of the true commit point;
`UncommittedBand` names the range before anything is written, and
`RecoveryReport` records which indices were promoted or discarded afterwards.

The other safeguards are that it refuses a membership the recovered node could
not elect itself in, that it refuses to discard a log nothing is proven about,
that the term it writes in is above the log's own last term as well as the hard
state's -- so it cannot create the one thing a crashed write would, an entry
from a term the node does not believe it reached -- and that `filestore` holds
an exclusive lock on its directory, so recovery run against a node that is
still up fails at the open rather than racing its writes.

---

## Known limitations

Stated here rather than discovered later.

- **No batched write across groups.** Each group writes and syncs its own log.
  At high group counts this is the binding constraint; see the scale notes in
  the README. Sharing one write-ahead log across groups needs a storage
  abstraction that spans them rather than one per node, which is an
  architectural change rather than a missing option.
- **No weighted quorums, deliberately.** A quorum is a count of voters, not a
  sum of weights: a voter cannot count for more than one. Weights would be
  safe -- a weighted majority intersects another weighted majority exactly as
  a plain one does -- so this is a choice rather than a limit of the model.

  It is a choice because the things people reach for weights to express are
  already here, and expressed better. A write that must survive losing a
  whole failure domain is `Config.MinCommitZones`, which weights cannot say
  at all: a sum says nothing about *where* the replicas that contributed to
  it are. A member that should not vote is a learner, which is weight zero.
  Cheaper writes, or writes that are on every disk before they are
  acknowledged, is `SetCommitQuorum`. A cheap tie-breaker in a third site is
  a witness, which votes in full and stores nothing. Leadership on the largest machine is
  `Config.PreferredLeader`; quorum size is about how many failures a group
  survives, not how fast its members are, and weighting a node up makes the
  group *depend* on it rather than benefit from it.

  What weights would add is a misconfiguration with no good error. Any node
  whose weight exceeds half the total becomes mandatory -- no quorum can be
  formed without it -- so the group quietly stops tolerating its loss, and
  nothing about the configuration looks wrong until that node is the one
  that fails. The bound would also have to be replicated like the commit
  quorum, and would collide with it: `SetCommitQuorum(3)` would mean three
  voters or three weight units, and every decision site would have to pick.

  The seam is there if a deployment ever needs it. Every quorum decision goes
  through one pair of functions in `quorum.go` and the membership entry
  already carries per-peer role bits, so a weight is additive rather than a
  redesign. The case that would justify it is hierarchical quorums -- a
  majority of zones, each contributing a majority of its own members, which
  is Zookeeper's model and which weights only approximate. `MinCommitZones`
  is already half of that.
- **Lease reads assume bounded clock drift.** `ReadIndex` does not; prefer it
  unless you have measured your clocks.
- **Recovering from permanent quorum loss is not a safe operation, and cannot
  be made one.** Half of it is information-theoretic: what no surviving disk
  holds is gone. The other half is a choice between two wrong answers, bounded
  and reported but not eliminated. See the divergence above.
