# Example: Distributed Rate Limiter

This example demonstrates how to build a production-grade Distributed Rate Limiter using EasyRaft. It supports token-bucket logic with atomic decrements and automatic refills.

## Features
- **Atomic Quotas**: Use `RegisterMutation` to ensure token decrements are consistent across the cluster.
- **Auto-Refill**: Quotas refill based on time elapsed between requests. The timestamp is supplied by the caller in the mutation args so the state machine remains deterministic across all Raft nodes.
- **HTTP API**: Manage quotas and check limits via simple REST calls.
- **Leader Redirection**: Clients can hit any node; followers will redirect to the leader automatically.

## Running a 3-Node Cluster

The quickest way to start a local cluster is with the provided script:

```bash
./cluster.sh
```

It builds the binary, wipes any previous data directories, starts all three nodes (using `--join`), waits for a leader to be elected, and prints a member table confirming the cluster is healthy. Press Ctrl-C to stop all nodes and clean up.

Alternatively, start the nodes manually. There are two ways to form a cluster: **static peers** (all addresses known upfront) or **join** (nodes added one at a time to an existing cluster).

### Option A: Static peers (all addresses known upfront)

Open three terminals and run:

**Terminal 1:**
```bash
go run main.go -id n1 -raft :7001 -http :8001 -data data/n1 -peers n1=127.0.0.1:7001,n2=127.0.0.1:7002,n3=127.0.0.1:7003
```

**Terminal 2:**
```bash
go run main.go -id n2 -raft :7002 -http :8002 -data data/n2 -peers n1=127.0.0.1:7001,n2=127.0.0.1:7002,n3=127.0.0.1:7003
```

**Terminal 3:**
```bash
go run main.go -id n3 -raft :7003 -http :8003 -data data/n3 -peers n1=127.0.0.1:7001,n2=127.0.0.1:7002,n3=127.0.0.1:7003
```

### Option B: Join (add nodes one at a time)

Start the first node as a single-node cluster, then join subsequent nodes to it using the `-join` flag pointing at any existing node's HTTP address.

**Terminal 1** (bootstrap the cluster):
```bash
go run main.go -id n1 -raft :7001 -http :8001 -data data/n1 -peers n1=127.0.0.1:7001
```

**Terminal 2** (join the existing cluster):
```bash
go run main.go -id n2 -raft :7002 -http :8002 -data data/n2 -join 127.0.0.1:8001
```

**Terminal 3** (join again):
```bash
go run main.go -id n3 -raft :7003 -http :8003 -data data/n3 -join 127.0.0.1:8001
```

You can pass multiple comma-separated seed addresses to `-join` for redundancy (the node tries each in order until one accepts).

## Using the Rate Limiter

### 1. Create a Quota
Create a quota for API key `premium-user` with 100 max tokens and a refill rate of 1 token/sec.
```bash
curl -X POST http://localhost:8001/quotas/premium-user \
  -H 'Content-Type: application/json' \
  -d '{"max_tokens": 100, "current_tokens": 100, "refill_rate": 1}'
```

### 2. Request Tokens (Mutate)
Request 5 tokens. Supply the current Unix timestamp so the mutation is deterministic.
```bash
curl -X POST http://localhost:8001/quotas/premium-user/mutate \
  -H 'Content-Type: application/json' \
  -d "{\"name\": \"take\", \"args\": {\"requested\": 5, \"now\": $(date +%s)}}"
```
**Response:** `{"allowed":true,"remains":95}`

### 3. Check Quota Status
```bash
curl http://localhost:8001/quotas/premium-user
```

### 4. Observe Leader Redirection
If `n1` is not the leader, the curl command will receive a `307 Redirect`. Adding `-L` follows it automatically:
```bash
curl -L -X POST http://localhost:8002/quotas/premium-user/mutate \
  -H 'Content-Type: application/json' \
  -d "{\"name\": \"take\", \"args\": {\"requested\": 1, \"now\": $(date +%s)}}"
```

## Go Client Library

A high-level Go client is available in the [`client`](./client) package. It handles node discovery, leader redirection, and provides a type-safe API for managing quotas and requesting tokens.

```go
import "github.com/brunoga/raft/examples/ratelimiter/client"

c := client.New([]string{"http://localhost:8001", "http://localhost:8002"})

// Create a quota — returns ErrConflict if the key already exists.
err := c.CreateQuota(ctx, "premium-user", client.Quota{
    MaxTokens: 100, CurrentTokens: 100, RefillRate: 1,
})

// Create-or-update (tries POST; falls back to PUT on conflict).
err = c.SetQuota(ctx, "premium-user", client.Quota{
    MaxTokens: 200, CurrentTokens: 200, RefillRate: 2,
})

// Consume tokens.
resp, err := c.Take(ctx, "premium-user", 5)
if err == nil && resp.Allowed {
    fmt.Printf("Allowed! Tokens remaining: %d\n", resp.Remains)
}

// Read quota status (linearizable by default; pass true for a stale read).
quota, err := c.GetQuota(ctx, "premium-user", false)
if errors.Is(err, client.ErrNotFound) {
    log.Println("quota does not exist")
}

// Remove a quota.
err = c.DeleteQuota(ctx, "premium-user")
```

## HTTP status codes

| Code | Meaning |
|------|---------|
| `200 OK` | Read or mutation succeeded |
| `201 Created` | Quota created (`POST /quotas/{key}`) |
| `204 No Content` | Quota updated or deleted |
| `307 Temporary Redirect` | This node is not the leader; `Location` points at the leader |
| `400 Bad Request` | Malformed JSON or invalid mutation args |
| `404 Not Found` | Quota key does not exist |
| `409 Conflict` | Quota key already exists (`POST /quotas/{key}`) |
| `503 Service Unavailable` | Leader unknown (e.g. during an election) |
| `500 Internal Server Error` | Storage failure or mutation error |

## Production Notes

- **Cluster Synchronization**: The server uses `store.Ready(ctx)` at startup. This blocks until a leader is elected and the node is fully caught up, preventing "not leader" errors for initial requests.
- **Observability**: Prometheus metrics are available at `/metrics` (if configured), providing visibility into Raft internals and mutation performance.
- **Storage Isolation**: Each node's data is stored in its own directory (`data/n1`, etc.) to prevent file lock contention.
- **Determinism**: Never call `time.Now()` inside a mutation. Pass the timestamp in the mutation args — all nodes receive the same args and will compute the same result.
