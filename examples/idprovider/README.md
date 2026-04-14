# idprovider — distributed monotonic ID allocation service

`idprovider` is a production-ready distributed ID allocation service built on
the `raft` package. It supports **multiple independent domains**, each with its
own monotonically increasing counter. Allocations are unique, never reused, and
safe across leader failovers, network partitions, and process restarts.

## Allocation model

Each domain holds a single `uint64` counter. A committed allocation atomically
advances the counter by `count` and returns the exclusive range
`[start, start+count)`. Ranges within a domain are disjoint and ordered: if
allocation A commits before allocation B in the same domain, then
`A.start + A.count <= B.start`.

**Exactly-once semantics**: clients supply `X-Client-ID` and `X-Seq-Num`
headers on any mutating request (create domain, delete domain, allocate IDs).
If a request is committed but the response is lost in transit, retrying with the
same `(clientID, seqNum)` pair returns the original result without re-executing
the operation.

## Three-node quick-start

```bash
# Build
go build -o idprovider ./examples/idprovider

# Node 1
./idprovider --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/idp/n1 \
             --peer n2=localhost:7002 --peer n3=localhost:7003

# Node 2 (separate terminal)
./idprovider --id n2 --raft-addr :7002 --http-addr :8002 --data-dir /tmp/idp/n2 \
             --peer n1=localhost:7001 --peer n3=localhost:7003

# Node 3 (separate terminal)
./idprovider --id n3 --raft-addr :7003 --http-addr :8003 --data-dir /tmp/idp/n3 \
             --peer n1=localhost:7001 --peer n2=localhost:7002
```

After a leader is elected (~300 ms) any node's HTTP endpoint can be queried.

## HTTP API

### Domain management

#### `POST /domains/{name}`

Creates a domain. Idempotent — succeeds whether or not the domain already
exists.

```bash
curl -s -X POST http://localhost:8001/domains/orders
# 200 OK
```

Supports `X-Client-ID` / `X-Seq-Num` headers for exactly-once delivery.

#### `DELETE /domains/{name}`

Deletes a domain and its counter. Returns `404` if the domain does not exist.
Outstanding IDs already allocated from the domain remain valid.

```bash
curl -s -X DELETE http://localhost:8001/domains/orders
# 204 No Content
```

Supports `X-Client-ID` / `X-Seq-Num` headers for exactly-once delivery.

#### `GET /domains[?consistency=stale]`

Returns all domains and their current counter values.

- **Default** (no query param): linearizable — forwards the read barrier to the
  leader and waits for the local state machine to catch up before responding.
- **`?consistency=stale`**: serves directly from this node's local state machine
  with no leader round-trip. May lag by up to one replication interval, but
  never returns data from the future. The `X-Raft-Applied-Index` response header
  carries the applied index the read was taken at.

```bash
# Linearizable (default)
curl -s http://localhost:8001/domains | jq

# Stale — lower latency, no leader dependency
curl -s 'http://localhost:8001/domains?consistency=stale' | jq
# X-Raft-Applied-Index: 12
# {
#   "orders": 101,
#   "invoices": 42
# }
```

### ID allocation

#### `POST /domains/{name}/next[?count=N]`

Allocates `count` IDs (default `1`) from the named domain. Returns the reserved
range. Returns `404` if the domain does not exist.

```bash
# Allocate one ID (idempotent — same range on retry with same headers)
curl -s -X POST http://localhost:8001/domains/orders/next \
     -H 'X-Client-ID: checkout-svc' -H 'X-Seq-Num: 1'
# {"start":1,"count":1}

# Allocate 100 IDs at once
curl -s -X POST 'http://localhost:8001/domains/orders/next?count=100' \
     -H 'X-Client-ID: checkout-svc' -H 'X-Seq-Num: 2'
# {"start":2,"count":100}
```

**Response** `200 OK`:
```json
{"start": 1, "count": 1}
```
IDs `[start, start+count)` are exclusively yours.

**Headers for exactly-once delivery:**

| Header | Type | Description |
|--------|------|-------------|
| `X-Client-ID` | string | Stable client identifier (e.g. service name + instance) |
| `X-Seq-Num` | uint64 | Monotonically increasing per-client request number |

Both headers must be present to enable deduplication. Omitting them makes the
allocation at-most-once (safe for non-retrying callers).

#### `GET /domains/{name}/current[?consistency=stale]`

Returns the current high-water mark for a domain. All IDs ≤ `current` have been
allocated. Returns `404` if the domain does not exist.

Accepts the same `?consistency=stale` parameter as `GET /domains` above, with
identical semantics and the same `X-Raft-Applied-Index` response header.

```bash
# Linearizable (default)
curl -s http://localhost:8001/domains/orders/current
# {"domain":"orders","current":101}

# Stale
curl -s 'http://localhost:8001/domains/orders/current?consistency=stale'
# X-Raft-Applied-Index: 12
# {"domain":"orders","current":101}
```

## HTTP status codes

| Code | Meaning |
|------|---------|
| `200 OK` / `204 No Content` | Request succeeded |
| `400 Bad Request` | `?count` is zero or non-numeric; `X-Seq-Num` cannot be parsed |
| `404 Not Found` | Domain does not exist (allocation, current, or delete) |
| `503 Service Unavailable` | This node is not the leader; `X-Raft-Leader` header carries the current leader's node ID |
| `500 Internal Server Error` | Storage failure, context timeout, or other unexpected error |

Clients should retry `503` against the node named in `X-Raft-Leader`. `500` may
be transient (e.g. a brief leader election) and is safe to retry with the same
`(clientID, seqNum)` if idempotency headers were supplied. `400` and `404` are
permanent and should not be retried without fixing the request.

### `GET /status`

Node state, current leader ID, and last applied Raft index.

```bash
curl -s http://localhost:8001/status | jq
# {
#   "id": "n1",
#   "state": "Leader",
#   "leader": "n1",
#   "last_applied": 5
# }
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--id` | required | Unique node ID |
| `--raft-addr` | `:7001` | gRPC listen address for Raft peer RPCs |
| `--http-addr` | `:8001` | HTTP listen address for client requests |
| `--data-dir` | required | Directory for persistent log and snapshot |
| `--peer id=addr` | (repeatable) | Peer node: ID and its Raft gRPC address |
| `--tls-cert` | | PEM certificate file for mTLS (must be set with `--tls-key` and `--tls-ca`) |
| `--tls-key` | | PEM private key file for mTLS |
| `--tls-ca` | | PEM CA certificate file for mTLS peer verification |

All three TLS flags must be provided together or omitted entirely. When set,
all peer-to-peer Raft RPCs are encrypted and mutually authenticated.

## Operational notes

- **Leader routing**: non-leader nodes return `503` with `X-Raft-Leader`
  indicating the current leader's node ID. Map node IDs to HTTP addresses in
  your service discovery layer to implement automatic redirect.
- **Batch allocations**: prefer large `count` values over many single-ID
  requests to amortise the per-round-trip consensus cost.
- **Domain persistence**: the full domain map is included in every Raft snapshot
  and survives restarts. Counters only move forward — they are never reset.
- **Overflow**: each counter is `uint64`; at 10 million allocations per second
  it would take ~58 000 years to overflow.
- **Isolation**: domains are fully independent. A slow or high-volume domain
  does not affect allocation latency in other domains.
