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

### Commit index is not persisted

The paper treats `commitIndex` as volatile and this implementation agrees, but
it is worth stating because some implementations persist it to shorten restart.
Here a restarted node learns its commit index from the leader.

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

### Learners are not witnesses

A non-voting member here replicates the full log and does not vote. §11.7.2
describes a *witness*, which votes but does not store the full log. They are
opposites, and witnesses are not implemented.

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
deliberately outside the safety argument: it can promote entries that were
never committed, and it discards whatever the lost majority had that this node
does not. Both are inherent to recovering a cluster whose majority is gone, and
both are stated in the API documentation and in the README.

The safeguards are that it refuses a membership the recovered node could not
elect itself in, that it never rewrites or discards entries the node already
has, and that the term it writes in is above the log's own last term as well as
the hard state's, so it cannot create the one thing a crashed write would: an
entry from a term the node does not believe it reached.

---

## Known limitations

Stated here rather than discovered later.

- **No batched write across groups.** Each group writes and syncs its own log.
  At high group counts this is the binding constraint; see the scale notes in
  the README. Sharing one write-ahead log across groups needs a storage
  abstraction that spans them rather than one per node, which is an
  architectural change rather than a missing option.
- **No flexible or weighted quorums.** A quorum is a majority. Placement can be
  constrained -- `Config.MinCommitZones` requires a write to reach more than one
  failure domain before it commits -- but the count itself is not configurable,
  so there is no way to trade read quorum size against write quorum size.
- **No witnesses.** A member either replicates the log in full or does not vote;
  there is no member that votes without storing entries (dissertation §11.7.2).
- **Lease reads assume bounded clock drift.** `ReadIndex` does not; prefer it
  unless you have measured your clocks.
- **Recovering from permanent quorum loss is not a safe operation.** It cannot
  be; see the divergence above. It is an operator action with data-loss
  consequences, not something the cluster does for itself.
