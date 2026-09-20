# `watchtower` — what the library can tell you about itself

Two seams nothing else here uses:

| | |
|---|---|
| `raft.Node.Events` | what happened, to whom, and when — on a bounded, lossy channel |
| `raft.ApplyMetrics` | how much of its time the apply loop spends applying rather than waiting |

```bash
go build -o watchtower ./watchtower
./watchtower/cluster.sh                        # 3 nodes, fast state machine
./watchtower/cluster.sh --apply-delay 2ms      # same, with a slow one
```

---

## Metrics say how often; events say who

A counter of configuration changes does not name the peer that joined. A
replication-latency histogram does not say which follower stopped answering —
and that is the only fact that decides whether the next failure costs the
cluster its quorum.

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

`GET /events` streams them as server-sent events; `GET /observed` is the state
built from them — who is up, who leads, what happened recently.

```bash
curl -N http://localhost:8001/events
```

### The node reports transitions, not state

`EventPeerUnresponsive` fires when a peer stops answering, and nothing fires
while it stays down. An observer that does not keep the state cannot answer
"which peers are down right now", which is the question worth asking.

This one keeps it, along with how many times each peer has flapped — a peer
that has gone down and come back four times is not the same as one that has
been healthy all along, and only the count distinguishes them.

### Delivery is lossy, deliberately

The channel holds 64 events and **the node never blocks on it**. A consumer
that stops receiving loses events rather than stalling consensus for every
other client of that node.

Nothing is lost silently: the next event a lagging consumer receives carries
the number discarded before it, in `Event.Dropped`. An observer that ignores
that field presents a clean history it does not have, which is worse than
presenting none — so this one counts them and `/observed` reports the total.

The same rule applies one level down. The fan-out to SSE clients never blocks
either, or a browser tab left open on a laptop that went to sleep would stop
the event history for everyone else.

### The last event a node ever sends

`EventNodeFailed` means the node stopped because a durable write failed. It
will not report anything again, and it will not recover on its own. Surfacing
it is the difference between a node that is quiet and a node that is dead.

---

## Saturation says whether it is you or consensus

Proposal latency covers consensus **and** the state machine, so a rise in it
does not say which of the two to fix. Apply saturation does:

- **Near 1** — the apply loop never gets to wait. The state machine is the
  constraint; faster consensus buys nothing.
- **Low, with slow proposals** — the apply loop is idle waiting for entries to
  commit. The problem is upstream of the state machine: the network, the disk,
  or a quorum that is struggling.

The example's own test drives the same cluster with the same load driver, twice:

```
apply delay 0s:   230000 proposals in 2s, saturation 0.452
apply delay 2ms:     896 proposals in 2s, saturation 0.906
```

Two hundred and fifty times fewer proposals, and the number says why.

Try it:

```bash
./watchtower/cluster.sh --apply-delay 2ms
# in another terminal
for i in $(seq 1 500); do
  curl -s -L -X PUT http://localhost:8001/keys/k$i \
       -H 'Content-Type: application/json' -d '{"value":"v"}' >/dev/null
done
curl -s http://localhost:8001/metrics | grep raft_apply_saturation
```

### Reporting to more than one place

`Config.Metrics` takes one implementation, and the engine finds the optional
interfaces on it by type assertion. Reporting to Prometheus *and* keeping the
latest value where `/health` can read it is not a choice between the two —
embed the Prometheus implementation and override the one method:

```go
type metricsSink struct {
    *prommetrics.Metrics
    sm *kvSM
}

func (m metricsSink) ApplySaturation(id raft.NodeID, saturation float64) {
    m.Metrics.ApplySaturation(id, saturation)
    m.sm.setSaturation(saturation)
}
```

`prommetrics` implements every optional metrics interface the engine looks for,
`ApplyMetrics` included, and exports the ratio as `raft_apply_saturation`.

---

## Watching a peer go away

The events are most interesting when something breaks. With the cluster
running:

```bash
# Freeze a follower. Its peers notice within an election timeout.
pkill -STOP -f 'watchtower --id n3'
curl -s http://localhost:8001/observed | jq '.peers'

# And let it back in.
pkill -CONT -f 'watchtower --id n3'
```

`/health` reports `degraded_but_serving` while a peer is unreachable and still
returns 200, because a missing follower is not an outage while a quorum
remains. Returning 503 there would pull a healthy node out of rotation for a
problem on a different machine.

## API

```bash
curl -L -X PUT http://localhost:8001/keys/greeting \
     -H 'Content-Type: application/json' -d '{"value":"hello"}'
curl http://localhost:8001/keys/greeting     # local read, no ReadIndex
curl http://localhost:8001/observed | jq     # state built from events
curl http://localhost:8001/health | jq       # the operator's one-line answer
curl -N http://localhost:8001/events         # live stream
curl http://localhost:8001/metrics           # Prometheus
```

Reads here are local and may be stale: this example is about observing the
node, and [`durablekv`](../durablekv/) covers linearizable reads.
