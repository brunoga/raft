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
- **`Collection.OnChange`** — callback fired after each committed write, outside the Raft lock, on every replica.
- **Deterministic versioning** — `ConfigEntry.Version` is set by the HTTP handler (as `time.Now().UnixNano()`) before proposing, so all replicas apply the same value rather than reading the clock inside `Apply`.
- **`WithJoinAddr`** — nodes join one at a time without a full peer list.

---

## HTTP API

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/configs/{key}` | Set a value (create or update) |
| `GET` | `/configs/{key}` | Read — linearizable |
| `GET` | `/configs/{key}?consistency=stale` | Read — local, no round-trip |
| `DELETE` | `/configs/{key}` | Delete |
| `GET` | `/configs` | List all — linearizable |
| `GET` | `/configs?consistency=stale` | List all — local |
| `GET` | `/watch/{key}` | SSE stream for one key |
| `GET` | `/watch` | SSE stream for all keys |

### SSE event format

```
event: snapshot
data: {"key":"db.host","value":"localhost","version":1712345678901234567}

event: change
data: {"key":"db.host","value":"db2.example.com","version":1712345690000000000}

event: delete
data: {"key":"db.host","deleted":true}
```

On connect, the server sends one `snapshot` event per existing key in the collection so the client starts from a known state, then streams `change` and `delete` events as they are committed.

---

## Running a 3-node cluster

**Terminal 1 — bootstrap node**

```bash
./configsvc --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/cfg/n1
```

**Terminal 2 — join**

```bash
./configsvc --id n2 --raft-addr :7002 --http-addr :8002 --data-dir /tmp/cfg/n2 \
            --join localhost:8001
```

**Terminal 3 — join**

```bash
./configsvc --id n3 --raft-addr :7003 --http-addr :8003 --data-dir /tmp/cfg/n3 \
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
data: {"key":"db.host","value":"localhost","version":1712345678901234567}

event: change
data: {"key":"db.port","value":"5432","version":1712345679012345678}
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
data: {"key":"db.host","value":"db2.example.com","version":1712345690000000000}
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

Each node runs an independent copy of the config state machine. After every committed log entry is applied, EasyRaft's internal dispatcher calls `OnChange` outside the Raft lock. The `configsvc` handler receives the typed `*ConfigEntry` and calls `WatcherRegistry.notify`, which fans out to all locally connected SSE clients for that key.

```
Leader applies entry → all followers replicate and apply the same entry
         ↓ (on every node)
     OnChange fires
         ↓
  WatcherRegistry.notify
         ↓
  SSE clients on this node receive the event
```

Clients watching on different nodes may receive events at slightly different wall-clock times (bounded by log replication latency), but they all receive the same events in the same order.

### What happens on leader failover

The leader election process temporarily blocks new writes. In-flight SSE connections are unaffected — they remain open and will resume receiving events after the new leader is elected and commits entries. No reconnect is needed.

### Version field

`ConfigEntry.Version` is set to `time.Now().UnixNano()` by the HTTP handler **before** the mutation is proposed. The version is encoded in the request body and replicated as part of the log entry, so all replicas apply the exact same version value. This follows the same determinism pattern as the ratelimiter example — never read the clock inside `Apply`.
