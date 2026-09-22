# tenants — EasyRaft with a Raft group per tenant

A multi-tenant store where **every tenant is its own Raft group**. One node
runs many groups through an `easyraft.Manager`.

```bash
./cluster.sh
```

Three nodes, nine groups, leader balancing on.

---

## Why a group per tenant

Isolation you get by construction rather than by being careful. One tenant's
writes queue behind that tenant's log, not behind everybody's, and a tenant
whose state is being snapshotted does not stall the rest. A single group
serialises every write in the deployment through one log; that is the right
answer until it is not, and this is what the other answer looks like.

It is the only example built on the `Manager`, and it exists for the three
things that only make sense with one.

### `WithLeaderBalancing`

Groups elect leaders independently and nothing coordinates them. Left alone, a
host that stayed up while the others restarted ends up leading most of them —
and the leader does the replication, serves the linearizable reads and takes
every write, so one host doing all of it is both the bottleneck and the
failure that hurts most.

```go
easyraft.WithLeaderBalancing(map[raft.HostID]string{
    "n1": "127.0.0.1:8001",
    "n2": "127.0.0.1:8002",
    "n3": "127.0.0.1:8003",
}, 15*time.Second)
```

A host's ID is the node ID its `Manager` was built with, since one `Manager`
is one physical node. Every host may run the controller; the cooldown is what
stops two of them handing a group back and forth.

A transfer is an election, so the aim is a balanced cluster rather than a
perfectly balanced one. `cluster.sh` prints the leader count per host — kill a
node, restart it, and watch the counts even out again.

### `WithSharedWAL`

Every group on a host appends to one write-ahead log, so a burst of writes
across tenants costs one `fsync` rather than one per tenant. With a group per
tenant that is the difference between a disk that keeps up and one that does
not.

### The data path is already there

There are **no CRUD routes in this example**. The `Manager` serves them at
`/groups/{groupID}/{collection}/{key}`, and `WithHTTPMux` puts this program's
one route on the same port. What the example adds is the part the library
cannot decide for you: which group a tenant belongs to.

---

## Group IDs start at 1

```go
return 1 + h.Sum64()%uint64(groups), nil
```

Not `h.Sum64()%uint64(groups)`. A `Manager` routes every inbound RPC by the
group ID it carries, and zero is what a *single-group* node's RPCs carry — so
a group numbered zero would never be reachable. It would get no votes, no
appends and no election, and sit in `PreCandidate` for ever with nothing in
its own log to say why. `AddStore(0)` is refused for the same reason.

`TestGroupFor_NeverReturnsZero` pins it across every group count from 1 to 16.

---

## Addressing a tenant

The mapping is a pure function of the name, so the cluster and its clients
cannot drift apart without disagreeing about `--groups`:

```go
group, err := tenants.GroupFor("acme", groups)

c, err := client.New(
    client.WithEndpoints("127.0.0.1:8001", "127.0.0.1:8002", "127.0.0.1:8003"),
    client.WithGroup(group),
)
items := client.Collection[tenants.Item](c, tenants.CollectionName)
```

`WithGroup` is the only difference from a single-group client. After it, every
call is what it would be against a plain `Store`, and the client still finds
the leader **of that group** and follows it when balancing moves it.

`tenantctl` is that in a command:

```bash
tenantctl --tenant acme put greeting hello
tenantctl --tenant acme get greeting
tenantctl --tenant acme where     # which group, without touching the cluster
```

**Changing `--groups` moves tenants.** A hash keeps the mapping from drifting,
at the price of making the group count part of the data layout rather than a
setting. Growing a real deployment means adding groups and migrating the
tenants that move, or using a table instead of a hash and accepting that the
table has to be replicated. This example uses the hash because it is the
version that fits on a page; the trade is the interesting part.

---

## HTTP API

This program adds one route. Everything else is easyraft's.

| Method | Path | |
|--------|------|---|
| `GET` | `/tenants` | where each tenant lives and who leads it |
| `GET` | `/__balance/status` | every group on this node *(easyraft)* |
| `PATCH` | `/groups/{group}/items/{key}` | write an item *(easyraft)* |
| `GET` | `/groups/{group}/items/{key}` | read one *(easyraft)* |
| `GET` | `/groups/{group}/items` | list a tenant's items *(easyraft)* |

`GET /tenants` lists only the names passed with `--tenants`. The mapping is a
function, so nothing depends on that list being complete or agreed — it is
there so the endpoint has something to show.

---

## Running it by hand

```bash
go build -o tenants ./examples/tenants
go build -o tenantctl ./examples/tenants/tenantctl

./tenants --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/t/n1 \
          --groups 9 \
          --peers n1=localhost:7001,n2=localhost:7002,n3=localhost:7003 \
          --balance-peers n1=localhost:8001,n2=localhost:8002,n3=localhost:8003
```

Repeat for `n2` and `n3` with their own ports and data directories. Every node
declares the same peers and the same group count.

> This example runs with a plaintext Raft transport and an open HTTP API, on
> purpose, because it is a local demo. Neither is acceptable on a network you
> do not control — see [Security](../../easyraft/README.md#security).
