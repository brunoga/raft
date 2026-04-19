# Example: Distributed Rate Limiter

This example demonstrates how to build a production-grade Distributed Rate Limiter using EasyRaft. It supports token-bucket logic with atomic decrements and automatic refills.

## Features
- **Atomic Quotas**: Use `RegisterMutation` to ensure token decrements are consistent across the cluster.
- **Auto-Refill**: Quotas refill based on time elapsed between requests.
- **HTTP API**: Manage quotas and check limits via simple REST calls.
- **Leader Redirection**: Clients can hit any node; followers will redirect to the leader automatically.

## Running a 3-Node Cluster

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

## Using the Rate Limiter

### 1. Create a Quota
Create a quota for API key `premium-user` with 100 max tokens and a refill rate of 1 token/sec.
```bash
curl -X POST http://localhost:8001/default/quotas/premium-user \
  -d '{"max_tokens": 100, "current_tokens": 100, "refill_rate": 1}'
```

### 2. Request Tokens (Mutate)
Request 5 tokens from the quota.
```bash
curl -X POST http://localhost:8001/default/quotas/premium-user/mutate \
  -d '{"name": "take", "args": 5}'
```
**Response:** `{"allowed":true,"remains":95}`

### 3. Check Quota Status
```bash
curl http://localhost:8001/default/quotas/premium-user
```

### 4. Observe Leader Redirection
If `n1` is not the leader, the curl command will receive a `307 Redirect`. Adding `-L` to curl will follow it automatically:
```bash
curl -L -X POST http://localhost:8002/default/quotas/premium-user/mutate \
  -d '{"name": "take", "args": 1}'
```
