# `raftctl` — offline operator tool

Everything here runs against a **stopped** node's data directory. That is the
only time these operations make sense: a node that is up does not need
recovering, and `filestore` holds an exclusive lock on its directory, so
pointing this at a live node fails instead of corrupting it.

```bash
go build -o raftctl ./raftctl
```

## What it demonstrates

| | |
|---|---|
| `raft.InspectStorage` | read a stopped node's durable state without changing it |
| `RecoveryInfo.MoreRecentThan` | the §5.4.1 up-to-date rule, used to choose a survivor |
| `RecoveryInfo.UncommittedBand` | which entries a recovery has to guess about |
| `RecoveryInfo.MembersComplete` | whether the membership could be read from disk at all |
| `raft.RecoverCluster` | rewrite a stopped node so it can elect itself |
| `WithKnownCommitted`, `DiscardUncommitted` | narrow the guess, or take the other side of it |
| `raft.RecoveryReport` | exactly which entries were promoted or discarded |
| `filestore.ErrLocked` | "stop the node first", enforced rather than asked |

---

## The problem it solves

Raft keeps a cluster available while a minority is down and refuses to make
progress when a majority is. That refusal is the point — a cluster that
committed without a majority could lose the write — but it also means losing
two nodes of three, for good, leaves a cluster that cannot elect a leader,
cannot commit, and therefore cannot commit the configuration change that would
shrink it to a size the survivor is a majority of.

The data is intact and permanently unreachable. Nothing the running node offers
helps, because every operation needs a quorum.

## The procedure

### 1. Stop every surviving node

Not optional, and not merely advisory: the tool cannot open a directory a node
still holds.

```
$ raftctl inspect --data-dir /var/lib/app/raft
raftctl: /var/lib/app/raft is in use: stop the node before running this
```

### 2. Look at what each survivor holds

```
$ raftctl inspect --data-dir /var/lib/app/n1
/var/lib/app/n1
  term               7
  log                1..4812 (last term 7)
  snapshot           index 4096 term 6
  members            n1 (voter), n2 (voter), n3 (voter)
  committed through  4802
  in doubt           4803..4812 (10 entries)

  Entries 4803..4812 either committed on nodes that are gone or were still
  in flight. Recovering this node either makes them part of the cluster's
  history or discards them; nothing here can tell which they were.
```

**This is the last moment those entries can be told apart from the rest.** If
the commands in them matter — a payment, a provisioning step — read them now.

### 3. Choose the survivor whose history is kept

```
$ raftctl compare --data-dir /var/lib/app/n1 --data-dir /var/lib/app/n2
...
Recover /var/lib/app/n1.
```

The choice is the rule elections use: highest last term, then longest log. Any
other choice silently drops whatever the more recent node had.

### 4. Recover exactly one node

```
$ raftctl recover --data-dir /var/lib/app/n1 --id n1 \
                  --learner n2 --learner n3 --confirm
Recovered n1.
  membership was     n1 (voter), n2 (voter), n3 (voter)
  membership now     n1 (voter), n2 (learner), n3 (learner)
  entry written at   index 4813, term 8
  committed through  4802
  promoted           4803..4812

Entries 4803..4812 were not provably committed and are now part of this
cluster's history. A client told one of those writes had failed will find
that it succeeded. Keep this report.
```

`--id` must be the only voter: a recovered node that is not a majority by
itself cannot elect a leader either. Naming the others as `--learner` saves
adding them back by hand; they catch up from a snapshot.

**Keep the report.** A cluster that comes back after a recovery looks like any
other cluster, and months later it is the only way to explain a write that
vanished or one that reappeared.

### 5. Erase every other survivor

Their logs are divergent history now. A node that restarts holding entries the
recovered node does not have can disrupt the elections of a cluster it is no
longer part of — and recovering two nodes produces two clusters with the same
node IDs and different data.

### 6. Start the recovered node

It elects itself and serves. The learners rejoin and catch up.

---

## Choosing which way to be wrong

Above the last provably committed index, each entry either committed on the
majority that is gone or was still in flight, and nothing that survives can
tell which. There are only two things to do about it:

| | keeps | costs |
|---|---|---|
| default | the whole log | entries that were never committed become committed — a write reported as failed took effect |
| `--discard-uncommitted` | only the proven prefix | entries that *had* committed but were not yet applied are thrown away — a write reported as succeeded did not |

Neither is safe. Which is the lesser harm is a property of the application: a
system that compensates for failed writes is damaged by the first, one that
acknowledges durably is damaged by the second.

Make the band small before choosing. `--known-committed N` raises the proven
floor with evidence from outside storage — a `DurableStateMachine`'s
`AppliedIndex` is proof, since nothing is applied before it commits:

```bash
raftctl recover --data-dir /var/lib/app/n1 --id n1 \
                --known-committed 4811 --discard-uncommitted --confirm
```

It must be a number you can actually prove. Claiming more than was committed
makes `--discard-uncommitted` keep entries it should have discarded.

`--discard-uncommitted` is refused outright when nothing at all is proven — no
snapshot and no `--known-committed` — rather than throwing away a whole log on
the strength of a flag.

---

## What this cannot do

Entries the lost majority committed that never reached the survivor are **gone**.
Nothing that remains holds them. Raft's guarantee is that a committed entry is
on a majority, so losing a majority is losing the guarantee, and no tool run
afterwards recovers what no surviving disk has.

The answer to that is not a better recovery. It is more voters,
`Config.MinCommitZones` to spread them across failure domains, and backups
taken off the cluster.
