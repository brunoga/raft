# `durablekv` — a state machine that keeps its own state on disk

Every other example here holds its state in memory and lets Raft rebuild it: on
restart the engine restores a snapshot and replays every entry after it. That is
the right default, and for a state machine that is itself a database it is work
already done, sitting on the disk.

This example is about saying so. Its state machine implements three optional
interfaces, each detected by a type assertion at construction, each independent:

| interface | what it changes |
|---|---|
| `raft.DurableStateMachine` | a restart replays nothing the store already applied |
| `raft.BatchApplier` | a run of entries costs one write and one `fsync`, not one each |
| `raft.SnapshotCapturer` | taking a snapshot does not pause the apply loop |

A state machine implementing none of them works exactly as before.

```bash
go build -o durablekv ./durablekv
./durablekv/cluster.sh          # 3 nodes on 127.0.0.1:7001-7003 / 8001-8003
```

---

## The applied index is a promise about durability

`DurableStateMachine` is one method:

```go
func (s *kvStore) AppliedIndex(ctx context.Context) (raft.Index, error)
```

The engine calls it once while the node is being built and **replays nothing at
or below what it returns**. That is the whole benefit, and the whole risk.

The promise is only keepable if the index is made durable *with* the data it
describes. Writing the data and then recording the index separately leaves a
window where a crash loses one and not the other, and the two failures are not
symmetrical:

- **Index behind data** — entries are applied twice on restart. Survivable only
  if every command is idempotent, which a library cannot assume.
- **Index ahead of data** — entries are never applied by anyone. This node is
  permanently different from every other replica, with nothing in the log to
  explain it: the engine was told the work was done.

So every record this store writes carries the index of the entry that produced
it, in the same write:

```
[4B len][4B crc32][8B applied index][1B op][4B key len][4B value len][key][value]
```

The durable applied index is whatever the last intact record says, which cannot
disagree with the data by construction. A record that does not parse, or whose
checksum fails, ends the replay and the file is truncated there — and that is
exactly the entry the engine replays, because the index stops at the last
record before it.

`TestAppliedIndex_NeverRunsAheadOfTheData` checks this against every truncation
point in a real file, and against a single-byte corruption at every offset.

## Restore is the other place the index matters

`Restore` replaces the state wholesale, so there is no per-entry record to carry
the index. It has to be written deliberately, and it has to be the snapshot's:

```go
s.data = data
s.applied = uint64(meta.LastIncludedIndex)
return s.rewriteLocked()
```

Reporting anything lower replays entries the snapshot already contains.
Reporting anything higher skips entries nobody applied.

## Batching pays for durability once

`ApplyBatch` gets everything committed since the apply loop last looked, which
under load is dozens of entries. They are encoded together, written together,
and **synced once**:

```
200 entries in 7 calls (28.6 per call)
```

That is 7 `fsync`s instead of 200. The cost of durability is per sync, not per
entry, and applying one at a time denies the store any way to say so.

A command that does not decode is reported as **that entry's** outcome rather
than failing the batch. One malformed payload is the proposer's problem;
failing the batch would make it everyone's.

## Capture keeps snapshots off the apply loop

`Snapshot` serialises the whole state, and it runs on the goroutine that applies
entries, because the two must not overlap. Everything committed during that
serialisation waits for it — a pause in apply, and so in the latency of every
proposal, once per `SnapshotThreshold` entries.

`Capture` is called on the apply goroutine and returns immediately with a
point-in-time handle; the serialisation happens on a background goroutine while
entries keep applying. Here the handle is a copy of the map, which is cheap
because the state is small. A store over a real engine would take that engine's
own snapshot instead — the shape of the interface is the same.

The captured state must not change afterwards, and
`TestCapture_IsNotAffectedByLaterWrites` checks it by writing to the store
between `Capture` and `Write`.

## The entry with no command

A newly elected leader appends an entry with an empty payload, to commit
anything left over from earlier terms. It reaches the state machine like any
other entry.

It is neither an error nor a write. A state machine that treats it as either
rejects something the engine produced for its own purposes, on every election —
which is what the first version of this example did, and what its restart test
caught.

---

## API

```bash
# Writes go through Raft; a follower redirects to the leader.
curl -L -X PUT http://localhost:8001/keys/greeting \
     -H 'Content-Type: application/json' -d '{"value":"hello"}'

# Linearizable by default: ReadIndex, then read the local store.
curl http://localhost:8001/keys/greeting

# Or skip the round trip and accept a possibly stale answer.
curl 'http://localhost:8002/keys/greeting?consistency=stale'

curl -L -X DELETE http://localhost:8001/keys/greeting
curl http://localhost:8001/keys

# sm_applied is what the state machine has durable; last_applied is what Raft
# has applied. They move together here, which is the point.
curl http://localhost:8001/status
```

## Seeing the restart cost nothing

Start the cluster, write a few thousand keys, stop a node and start it again.
Its log says what it found:

```
INFO durablekv: state machine opened applied_index=4096 keys=2000
```

and the engine replays nothing at or below that index.
`TestRestart_ReplaysNothingItHasAlreadyApplied` asserts exactly that, by
counting the entries the engine hands to the state machine after a restart:
it must be zero.

## What is deliberately simple

The store is an append-only file with an in-memory index, compacted when it
holds more than twice as many records as live keys. It is not a storage engine,
and it is not trying to be — it is the smallest thing that can honestly make the
durability promise the interfaces require. A real deployment would put a real
engine behind the same three methods.
