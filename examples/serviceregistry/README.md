# serviceregistry — EasyRaft with key leases

A service registry. Instances register themselves under a lease and keep it
alive while they run. When one stops — cleanly, by crashing, or by being cut
off from the cluster — it stops renewing, the lease expires, and its entry is
deleted on every replica.

Nothing sweeps a table of heartbeats, and nothing has to notice that an
instance died. That is the reason a registry is the example for leases.

```bash
./cluster.sh
```

Three registry nodes and two `api` instances. Ask any node who is up:

```bash
curl -s http://localhost:8003/services/api
```

Kill an agent and ask again a few seconds later.

---

## Two programs

**`serviceregistry`** is the registry: an `easyraft.Store` with one collection
and three read endpoints. It has **no registration endpoint**, because it needs
none.

**`agent`** is what registers. It talks to the cluster from outside using
[`easyraft/client`](../../easyraft/client/), grants its own lease, writes its
own entry under it, and holds it with a keep-alive loop:

```go
lease, err := c.GrantLease(ctx, ttl)
if err != nil {
    return err
}
if err := instances.UpsertWithLease(ctx, key, me, lease); err != nil {
    return err
}
go func() { loopDone <- c.KeepAliveLoop(ctx, lease) }()
```

That is the whole registration protocol. A registry that wrote its own entries
would need one of its own, a way to authenticate it, and something to expire
what nobody renewed.

---

## What it demonstrates

| | |
|---|---|
| `Store.GrantLease` + `Collection.UpsertWithLease` | an entry that outlives nothing |
| `KeepAliveLoop` | one goroutine holding a registration for as long as its context lives |
| `RevokeLease` | giving a registration up on purpose, rather than waiting to be noticed |
| `Collection.ListPrefix` | every instance of one service, from keys shaped `<service>/<instance>` |
| `Collection.Scan` | the whole registry a page at a time, cursor carried by the caller |
| `Collection.OnChange` | arrivals and departures logged on every replica as they apply |
| `easyraft/client` | a process outside the cluster finding the leader and following it |

The keys are `<service>/<instance>`, which is what makes "who is running this
service" a single prefix scan rather than a filter over everything.

`OnChange` is worth watching in the node logs. A lease expiring deletes its
keys through the same path an explicit delete takes, so an instance leaving
looks exactly like an instance being removed — which is what lets a watcher
keep a local view without knowing anything about leases:

```
msg="instance registered" key=api/api-1 addr=10.0.0.1:9000
msg="instance left" key=api/api-1
```

---

## The two ways an instance leaves

They are written differently in `agent/main.go` on purpose.

**SIGKILL.** No code runs. The lease simply stops being renewed, expires
within the TTL, and every replica deletes the entry at the same point in the
log. This is the case the design is for: nothing had to be told.

**SIGTERM or ^C.** The agent revokes its lease on the way out, which removes
the entry *at once* rather than leaving it to be noticed. That is the right
behaviour for a planned stop — a process that exits without revoking leaves a
stale entry for the rest of its TTL, which is correct for a crash and
needlessly slow for a restart.

Try both. With `--ttl 60s` the difference is obvious: the revoked one is gone
immediately, the killed one a minute later.

---

## What a TTL promises

Less than it looks like, and the library says so rather than letting you find
out. Expiry is a **proposal by the leader**, not something each replica
decides: `Apply` may not read a clock, because it runs on every replica at
different times and again during log replay, so a state machine that deleted
keys when it noticed the time had passed would hold different state on every
node.

So a key may outlive its TTL by the sweep interval plus however far the
granting and sweeping clocks disagree, and may go early after a leader change
to a node whose clock runs ahead. Use seconds, not milliseconds, and treat it
as *gone reasonably soon after nobody renewed it*.

`--lease-sweep` sets how often the leader looks; `cluster.sh` uses 500ms so
the demo is quick. The default is one second, and nothing is proposed at all
while no lease is due.

---

## HTTP API

```
GET /services                     every instance, sorted
GET /services?limit=N&after=KEY   one page of them
GET /services/{name}              every instance of one service
GET /services/{name}/{instance}   one instance
```

`next` in a paged response is the cursor for the following request, and it is
empty exactly when the scan reached the end. A short page is **not** the end —
nothing stops a page from being short.

The registry also serves everything easyraft does, including `/__leases`:

```bash
curl -s http://localhost:8001/__leases
```

---

## Running it by hand

```bash
go build -o serviceregistry ./examples/serviceregistry
go build -o agent ./examples/serviceregistry/agent

./serviceregistry --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/reg/n1
./serviceregistry --id n2 --raft-addr :7002 --http-addr :8002 --data-dir /tmp/reg/n2 --join localhost:8001
./serviceregistry --id n3 --raft-addr :7003 --http-addr :8003 --data-dir /tmp/reg/n3 --join localhost:8001

./agent --service api --id api-1 --addr 10.0.0.1:9000 \
        --endpoints localhost:8001,localhost:8002,localhost:8003 --ttl 10s
```

The agent is given every node, not just one. The client finds whichever is
leader and follows it when it moves; a node that is down costs one connection
attempt.

> This example runs with a plaintext Raft transport and an open HTTP API, on
> purpose, because it is a local demo. Neither is acceptable on a network you
> do not control — see [Security](../../easyraft/README.md#security).
