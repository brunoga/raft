package main

import (
	"context"
	"encoding/json"
	"flag"
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
			if len(parts) == 2 {
				peerMap[raft.NodeID(parts[0])] = parts[1]
			}
		}
	}

	// 1. Initialize EasyRaft Store.
	// In a real NoSQL DB, we would use easyraft.NewManager and multiple stores (shards).
	// For this example, we use a single store managing two collections.
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
	// This logic runs on every node during Apply, ensuring consistency.
	quotas.RegisterMutation("take", func(q *Quota, args []byte) (*Quota, []byte, error) {
		var requested int64 = 1
		if len(args) > 0 {
			_ = json.Unmarshal(args, &requested)
		}

		// Apply refill logic first
		now := time.Now().Unix()
		if q.LastRefillTime > 0 {
			elapsed := now - q.LastRefillTime
			refill := elapsed * q.RefillRate
			if refill > 0 {
				q.CurrentTokens += refill
				if q.CurrentTokens > q.MaxTokens {
					q.CurrentTokens = q.MaxTokens
				}
				q.LastRefillTime = now
			}
		} else {
			q.LastRefillTime = now
		}

		allowed := false
		if q.CurrentTokens >= requested {
			q.CurrentTokens -= requested
			allowed = true
		}

		resp, _ := json.Marshal(map[string]any{
			"allowed": allowed,
			"remains": q.CurrentTokens,
		})
		return q, resp, nil
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
