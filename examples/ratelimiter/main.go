package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/easyraft"
)

// Quota represents the rate limit configuration for an API key.
type Quota struct {
	MaxTokens      int64 `json:"max_tokens"`
	CurrentTokens  int64 `json:"current_tokens"`
	RefillRate     int64 `json:"refill_rate"` // tokens per second
	LastRefillTime int64 `json:"last_refill_time"`
}

// takeArgs is the JSON payload for the "take" mutation.
// Both fields are required:
//   - Requested: number of tokens to consume (must be ≥ 1).
//   - Now: the caller's current Unix timestamp. All nodes must apply the same
//     value, so the timestamp must be provided by the caller before proposing —
//     never computed inside the mutation function.
type takeArgs struct {
	Requested int64 `json:"requested"`
	Now       int64 `json:"now"`
}

// takeResponse is the JSON body returned by a successful "take" mutation.
type takeResponse struct {
	Allowed bool  `json:"allowed"`
	Remains int64 `json:"remains"`
}

func main() {
	id := flag.String("id", "", "Node ID")
	raftAddr := flag.String("raft", ":7001", "Raft listen address")
	httpAddr := flag.String("http", ":8001", "HTTP API listen address")
	dataDir := flag.String("data", "data/n1", "Data directory")
	peers := flag.String("peers", "", "Comma-separated list of id=addr")
	flag.Parse()

	if *id == "" {
		log.Fatal("-id is required")
	}

	peerMap := make(map[raft.NodeID]string)
	if *peers != "" {
		for _, p := range strings.Split(*peers, ",") {
			parts := strings.Split(p, "=")
			if len(parts) != 2 {
				// Warn and skip: the node can still start and reach the peers
				// it does know about. If those form a quorum the cluster
				// converges; dying here would only make things worse.
				log.Printf("WARNING: skipping malformed peer %q (expected id=host:port)", p)
				continue
			}
			peerMap[raft.NodeID(parts[0])] = parts[1]
		}
	}

	// 1. Initialize EasyRaft Store.
	// For this example, we use a single store with one collection ("quotas").
	// A production system with many tenants might shard via easyraft.NewManager.
	store, err := easyraft.NewStore(
		easyraft.WithID(raft.NodeID(*id)),
		easyraft.WithRaftAddr(*raftAddr),
		easyraft.WithHTTPAddr(*httpAddr),
		easyraft.WithDataDir(*dataDir),
		easyraft.WithPeers(peerMap),
		easyraft.WithLogger(slog.Default()),
	)
	if err != nil {
		log.Fatal(err)
	}

	// 2. Define the 'quotas' collection.
	quotas := easyraft.AddCollection[Quota](store, "quotas")

	// 3. Register a mutation for atomic token decrement (the "Take" operation).
	//
	// IMPORTANT: This function runs inside Apply on every node during log replay.
	// It must be purely deterministic — no calls to time.Now(), rand, or any
	// other non-deterministic source. The current timestamp is supplied by the
	// caller in the args so that all nodes apply the exact same value.
	quotas.RegisterMutation("take", func(q *Quota, args []byte) (*Quota, []byte, error) {
		var a takeArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, fmt.Errorf("take: invalid args: %w", err)
		}
		if a.Requested < 1 {
			return nil, nil, fmt.Errorf("take: requested must be >= 1, got %d", a.Requested)
		}
		if a.Now <= 0 {
			return nil, nil, fmt.Errorf("take: now must be a positive Unix timestamp, got %d", a.Now)
		}

		// Apply time-based refill using the caller-supplied timestamp.
		// Using a.Now (not time.Now) keeps the mutation deterministic across all nodes.
		if q.LastRefillTime == 0 {
			q.LastRefillTime = a.Now
		} else if a.Now > q.LastRefillTime {
			elapsed := a.Now - q.LastRefillTime
			if refill := elapsed * q.RefillRate; refill > 0 {
				q.CurrentTokens += refill
				if q.CurrentTokens > q.MaxTokens {
					q.CurrentTokens = q.MaxTokens
				}
				q.LastRefillTime = a.Now
			}
		}

		var resp takeResponse
		resp.Remains = q.CurrentTokens
		if q.CurrentTokens >= a.Requested {
			q.CurrentTokens -= a.Requested
			resp.Allowed = true
			resp.Remains = q.CurrentTokens
		}

		b, err := json.Marshal(resp)
		if err != nil {
			return nil, nil, fmt.Errorf("take: marshal response: %w", err)
		}
		return q, b, nil
	})

	// 4. Start the cluster.
	store.Start()
	defer store.Stop()

	log.Printf("Rate Limiter %s starting on %s (HTTP %s)", *id, *raftAddr, *httpAddr)

	// 5. Wait for cluster to be ready.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := store.Ready(ctx); err != nil {
		log.Printf("Warning: cluster not ready yet: %v", err)
	}
	cancel()

	// 6. Handle signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutting down...")
}
