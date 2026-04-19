# EasyRaft — High-Level Consensus for Go

EasyRaft is a production-ready, domain-first abstraction layer built on top of the `brunoga/raft` package. It allows you to build strongly-consistent, replicated services (from simple ID providers to sharded NoSQL databases) without managing the complexities of the Raft protocol.

## Why EasyRaft?

Standard Raft implementations require you to manually handle log entry encoding, state machine transitions, snapshotting, and leader proxying. EasyRaft automates all of this, providing:

- **Type-Safe Collections**: Manage your domain types (`Collection[T]`) directly.
- **Multi-Raft by Default**: Easily shard your data across multiple independent Raft groups using a single `Manager`.
- **Atomic Transactions**: Commit updates across multiple collections atomically.
- **Deterministic Mutations**: Define complex logic (like "increment if less than X") that executes identically on all nodes.
- **Batteries-Included HTTP**: Auto-generated REST API with leader redirection, Prometheus metrics, and health checks.
- **Linearizability**: Exactly-once semantics and linearizable reads out of the box.

---

## Core Concepts

### 1. The Manager
The `Manager` is the top-level entity that owns shared resources like the gRPC transport and the HTTP server. It allows you to run multiple independent Raft groups (shards) in a single process.

### 2. The Store
A `Store` represents a single Raft group. It has its own replicated log, its own leader, and manages one or more collections.

### 3. Collection[T]
A `Collection` is a typed namespace within a Store. It provides a CRUD API for your domain objects.

---

## Quick Start: Single-Collection Service

```go
type Config struct {
    Theme string `json:"theme"`
}

// 1. Initialize EasyRaft
er, _ := easyraft.New[Config](
    easyraft.WithID("node-1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithDataDir("/data/node-1"),
)

// 2. Register a custom mutation (Optional)
er.RegisterMutation("toggle-theme", func(c *Config, args []byte) (*Config, []byte, error) {
    if c.Theme == "dark" { c.Theme = "light" } else { c.Theme = "dark" }
    return c, nil, nil
})

// 3. Start the node
er.Start()

// 4. Use the API
ctx := context.Background()
err := er.Create(ctx, "user-1", Config{Theme: "dark"})
```

---

## Advanced: Sharding with Multi-Raft

```go
mgr, _ := easyraft.NewManager(
    easyraft.WithID("node-1"),
    easyraft.WithRaftAddr(":7001"),
    easyraft.WithHTTPAddr(":8001"),
)

// Shard 1 (Group 1)
s1, _ := mgr.AddStore(1, easyraft.WithDataDir("/data/s1"))
users := easyraft.AddCollection[User](s1, "users")

// Shard 2 (Group 2)
s2, _ := mgr.AddStore(2, easyraft.WithDataDir("/data/s2"))
billing := easyraft.AddCollection[Invoice](s2, "billing")

mgr.Start()
```

---

## Production Readiness Features

### 📡 Automatic Redirection
Follower nodes automatically respond with `307 Temporary Redirect` to the current leader. They coordinate their HTTP addresses via an internal metadata collection, so clients only need one entry point.

### 📈 Observability
- `GET /metrics`: Standard Prometheus metrics for Raft state, commit lag, and snapshot performance.
- `GET /status`: JSON snapshot of all Raft groups, terms, and applied indices.
- `GET /health`: Liveness probe for load balancers.

### 💾 Non-Blocking Snapshots
EasyRaft uses a structural copy-on-write strategy for snapshots. This means encoding and disk I/O happen asynchronously, preventing latency spikes during large state persistence.

### 🔒 Security
Integrated TLS support for all gRPC communication via `easyraft.WithTLS(config)`.

### 🛡️ Linearizability & Exactly-Once
Use `CreateOnce`, `UpdateOnce`, or `MutateOnce` to ensure that retried requests (due to network timeouts) are handled idempotently.

---

## HTTP API Reference

If `WithHTTPAddr` is provided, EasyRaft exposes:

- `POST   /groups/{gid}/{collection}/{key}` - Create item
- `GET    /groups/{gid}/{collection}/{key}` - Read (Linearizable)
- `GET    /groups/{gid}/{collection}/{key}?consistency=stale` - Read (Fast/Local)
- `PUT    /groups/{gid}/{collection}/{key}` - Update item
- `DELETE /groups/{gid}/{collection}/{key}` - Delete item
- `GET    /groups/{gid}/{collection}`      - List all
- `POST   /groups/{gid}/{collection}/{key}/mutate` - Invoke named mutation
- `GET    /status` - Cluster status
- `GET    /metrics` - Prometheus metrics
