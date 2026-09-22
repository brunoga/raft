# configsvc — distributed configuration service

A strongly-consistent, replicated configuration service built on EasyRaft. Clients can read and write string key-value pairs, and subscribe to changes on any key (or all keys) over Server-Sent Events (SSE).

```
go build -o configsvc ./examples/configsvc
```

---

## What this example demonstrates

This example focuses on **watch/subscribe** — a pattern none of the other examples cover. It shows the key architectural insight:

> Replicated state (the configs) lives inside Raft. Subscriber state (who is watching what) is purely local to each node. The `OnChange` hook fires after every committed write on every replica, so each node independently notifies its own connected clients.

Additional patterns:

- **`Collection.Upsert`** — atomic create-or-update with no race window between checking existence and writing.
- **`Collection.ReadRev` / `UpsertIf` / `DeleteIf`** — compare-and-swap, for the writes that are last-writer-wins only by accident. See below.
- **`Collection.OnChange`** — callback fired after each committed write, outside the Raft lock, on every replica.
- **`easyraft.Watcher[T]`** — generic pub/sub fan-out that converts `OnChange` events into buffered Go channels; wired with `configs.OnChange(watcher.Notify)`.
- **`Watcher[T].ServeSSE`** — streams `ChangeEvent[T]` values to HTTP clients as Server-Sent Events; handles headers, snapshot-on-connect, and live events.
- **`WithHTTPMux`** — registers EasyRaft management routes (`/join`, `/members`, etc.) on the application's own mux so a single HTTP server handles everything.
- **Deterministic versioning** — `ConfigEntry.Version` is set by the HTTP handler (as `time.Now().UnixNano()`) before proposing, so all replicas apply the same value rather than reading the clock inside `Apply`.
- **`WithJoinAddr`** — nodes join one at a time without a full peer list.

---

## HTTP API

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/configs/{key}` | Set a value (create or update); honours `If-Match` and `If-None-Match` |
| `GET` | `/configs/{key}` | Read — linearizable; the key's revision comes back as `ETag` |
| `GET` | `/configs/{key}?consistency=stale` | Read — local, no round-trip |
| `DELETE` | `/configs/{key}` | Delete; honours `If-Match` |
| `GET` | `/configs` | List all — linearizable |
| `GET` | `/configs?consistency=stale` | List all — local |
| `GET` | `/watch/{key}` | SSE stream for one key |
| `GET` | `/watch` | SSE stream for all keys |

---

## Compare-and-swap

Setting a value somebody typed is last-writer-wins, and that is fine. Setting one *computed from the value already there* is not: a read and a write are two log entries, and a write that lands between them is silently overwritten.

A `GET` returns the key's revision as an `ETag`. Hand it back as `If-Match` and the write applies only while the key is still at it:

```bash
etag=$(curl -sS -D- -o/dev/null http://localhost:8002/configs/db.host \
       | awk '/[Ee][Tt]ag:/ {print $2}' | tr -d '\r')

curl -L -X PUT -H "If-Match: $etag" \
     -H 'Content-Type: application/json' -d '{"value":"db.internal"}' \
     http://localhost:8001/configs/db.host
```

A second write with the same ETag answers `412 Precondition Failed`, and nothing is written. Re-read, redo the work, retry — that loop is a read-modify-write that loses nothing without holding a lock.

`If-None-Match: *` requires the key to be absent, which is how a caller claims one without overwriting whoever got there first. Anything else — a weak validator, a list of ETags, both headers at once — is answered with `400` rather than applied unconditionally, because a guess about which one the caller meant is the lost update the condition was asked for to prevent.

### Which number to compare against

The `ETag` carries the **revision**, not the `Version` field in the body. That distinction is the point.

`Version` is this service's own data: a timestamp it chose to expose. A timestamp is the wrong thing to compare against — two writers in the same nanosecond get the same one, and a clock that steps back produces one that has already been used. The revision is the index of the Raft entry that wrote the key: unique across the cluster, agreed by everyone, and never backwards.

### When to reach for a mutation instead

If the new value can be computed *inside* the state machine, register a mutation instead — it is one atomic entry and needs no condition or retry, as the [`ratelimiter`](../ratelimiter/) example shows. Compare-and-swap is for the case a mutation cannot cover: a value decided in a browser, in another service, or by a person.

---

## Go Client Library

A high-level Go client is available in the [`client`](./client) package. It handles node discovery (retrying other nodes if one is down), automatic redirects to the leader, and provides a type-safe API for CRUD operations and SSE watches.

```go
import "github.com/brunoga/raft/v2/examples/configsvc/client"

c := client.New([]string{"http://localhost:8001", "http://localhost:8002"})

// Set (upsert) a value.
err := c.Set(ctx, "db.host", "localhost")

// Get a value — linearizable by default; pass true for a local stale read.
entry, err := c.Get(ctx, "db.host", false)
if errors.Is(err, client.ErrNotFound) {
    log.Println("key does not exist")
}

// Compare-and-swap: read with the revision, write it back only if nothing
// moved in between.
entry, rev, err := c.GetRev(ctx, "db.host", false)
err = c.SetIf(ctx, "db.host", entry.Value+".internal", rev)
if errors.Is(err, client.ErrRevisionMismatch) {
    // Somebody wrote the key first. Read it again and redo the work.
}

// Delete a key.
err = c.Delete(ctx, "db.host")

// List all keys — linearizable by default; pass true for a stale read.
all, err := c.List(ctx, false)
for k, v := range all {
    fmt.Printf("%s = %s\n", k, v.Value)
}

// Watch for changes on a single key. Pass "" to watch all keys.
ch, err := c.Watch(ctx, "db.host")
for ev := range ch {
    switch ev.Type {
    case "snapshot", "change":
        fmt.Printf("%s: %s = %s\n", ev.Type, ev.Key, ev.Value.Value)
    case "delete":
        fmt.Printf("deleted: %s\n", ev.Key)
    }
}
```

## HTTP status codes

| Code | Meaning |
|------|---------|
| `200 OK` | Read succeeded |
| `204 No Content` | Write or delete succeeded |
| `307 Temporary Redirect` | This node is not the leader; `Location` points at the leader |
| `400 Bad Request` | Malformed JSON or missing key |
| `404 Not Found` | Config key does not exist (GET or DELETE) |
| `503 Service Unavailable` | Leader unknown (e.g. during an election) |
| `500 Internal Server Error` | Storage failure or unexpected error |

---

### SSE event format

Events are JSON-encoded [`easyraft.ChangeEvent[ConfigEntry]`](../../easyraft/watcher.go). The `value` field carries the full `ConfigEntry` object:

```
event: snapshot
data: {"key":"db.host","value":{"value":"localhost","version":1712345678901234567}}

event: change
data: {"key":"db.host","value":{"value":"db2.example.com","version":1712345690000000000}}

event: delete
data: {"key":"db.host","deleted":true}
```

On connect, the server sends one `snapshot` event per existing key in the collection so the client starts from a known state, then streams `change` and `delete` events as they are committed.

---

## Running a 3-node cluster

The quickest way to start a local cluster is with the provided script:

```bash
./cluster.sh
```

It builds the binary, wipes any previous data directories, starts all three nodes, waits for a leader to be elected, and prints a member table confirming the cluster is healthy. Press Ctrl-C to stop all nodes and clean up.

### Interactive REPL client

Once the cluster is running, use the REPL for interactive exploration:

```bash
go run ./examples/configsvc/repl
```

```
configsvc> set db.host localhost
ok
configsvc> get db.host
db.host = "localhost"  (version 1734567890123456789)
configsvc> list
  db.host                         "localhost"
configsvc> watch db.host
watching — press Enter to stop
  snapshot  db.host = "localhost"
  change    db.host = "prod-db-01"

configsvc> stats
=== Stats: http://localhost:8001 ===

Cluster Members:
  n1  leader   :7001
  n2  follower  :7002
  n3  follower  :7003

Node Status:
  id=n1           state=Leader      term=1     last_applied=3
...
configsvc> help
```

Use `--nodes` to point at a non-default cluster:

```bash
go run ./examples/configsvc/repl --nodes http://host1:8001,http://host2:8001,http://host3:8001
```

Alternatively, start the nodes manually in separate terminals:

**Terminal 1 — bootstrap node**

```bash
./configsvc --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 --data-dir /tmp/cfg/n1
```

**Terminal 2 — join**

```bash
./configsvc --id n2 --raft-addr 127.0.0.1:7002 --http-addr 127.0.0.1:8002 --data-dir /tmp/cfg/n2 \
            --join localhost:8001
```

**Terminal 3 — join**

```bash
./configsvc --id n3 --raft-addr 127.0.0.1:7003 --http-addr 127.0.0.1:8003 --data-dir /tmp/cfg/n3 \
            --join localhost:8001
```

---

## Example session

**Watch all keys on node 2 (any node works — watches are local)**

```bash
curl -N http://localhost:8002/watch
```

**Set values (follows 307 redirect to leader automatically)**

```bash
curl -L -X PUT http://localhost:8001/configs/db.host \
     -H 'Content-Type: application/json' -d '{"value":"localhost"}'

curl -L -X PUT http://localhost:8001/configs/db.port \
     -H 'Content-Type: application/json' -d '{"value":"5432"}'
```

The watcher on node 2 will print:

```
event: change
data: {"key":"db.host","value":{"value":"localhost","version":1712345678901234567}}

event: change
data: {"key":"db.port","value":{"value":"5432","version":1712345679012345678}}
```

**Watch a single key**

```bash
curl -N http://localhost:8003/watch/db.host
```

**Update and observe**

```bash
curl -L -X PUT http://localhost:8001/configs/db.host \
     -H 'Content-Type: application/json' -d '{"value":"db2.example.com"}'
```

The single-key watcher receives:

```
event: change
data: {"key":"db.host","value":{"value":"db2.example.com","version":1712345690000000000}}
```

**Delete**

```bash
curl -L -X DELETE http://localhost:8001/configs/db.host
```

```
event: delete
data: {"key":"db.host","deleted":true}
```

**Stale read (served locally, no leader round-trip)**

```bash
curl 'http://localhost:8003/configs/db.port?consistency=stale'
# {"value":"5432","version":1712345679012345678}
```

---

## Architecture notes

### Why watches work on every node

Each node runs an independent copy of the config state machine. After every committed log entry is applied, EasyRaft's internal dispatcher calls `OnChange` outside the Raft lock. `configsvc` wires `configs.OnChange(watcher.Notify)`, so `Watcher[T].Notify` fans out `ChangeEvent[ConfigEntry]` values to every locally subscribed SSE connection.

```
Leader applies entry → all followers replicate and apply the same entry
         ↓ (on every node)
     OnChange fires → Watcher[T].Notify
         ↓
  ServeSSE streams ChangeEvent[T] to SSE clients on this node
```

Clients watching on different nodes may receive events at slightly different wall-clock times (bounded by log replication latency), but they all receive the same events in the same order.

### What happens on leader failover

The leader election process temporarily blocks new writes. In-flight SSE connections are unaffected — they remain open and will resume receiving events after the new leader is elected and commits entries. No reconnect is needed.

### Version field

`ConfigEntry.Version` is set to `time.Now().UnixNano()` by the HTTP handler **before** the mutation is proposed. The version is encoded in the request body and replicated as part of the log entry, so all replicas apply the exact same version value. This follows the same determinism pattern as the ratelimiter example — never read the clock inside `Apply`.
