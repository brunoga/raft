package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/easyraft"
)

func BenchmarkRateLimiter(b *testing.B) {
	tmpDir, err := os.MkdirTemp("", "ratelimit-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Setup a 3-node cluster for realistic consensus overhead.
	ids := []raft.NodeID{"n1", "n2", "n3"}
	peers := map[raft.NodeID]string{
		"n1": "127.0.0.1:10001",
		"n2": "127.0.0.1:10002",
		"n3": "127.0.0.1:10003",
	}

	stores := make([]*easyraft.Store, 3)
	for i, id := range ids {
		s, err := easyraft.NewStore(
			easyraft.WithID(id),
			easyraft.WithRaftAddr(peers[id]),
			easyraft.WithDataDir(filepath.Join(tmpDir, string(id))),
			easyraft.WithPeers(peers),
			easyraft.WithSnapCount(10000), // Increase threshold for bench
		)
		if err != nil {
			b.Fatalf("failed to create store %s: %v", id, err)
		}
		stores[i] = s

		// Register the 'take' mutation on all nodes
		qColl := easyraft.AddCollection[Quota](s, "quotas")
		qColl.RegisterMutation("take", func(q *Quota, args []byte) (*Quota, []byte, error) {
			q.CurrentTokens--
			return q, nil, nil
		})

		s.Start()
	}

	defer func() {
		for _, s := range stores {
			s.Stop()
		}
	}()

	// Wait for a lead		// Wait for a leader to be elected and stable.
	var leaderStore *easyraft.Store
	ctx := context.Background()

	// Give it more time to stabilize
	start := time.Now()
	for time.Since(start) < 10*time.Second {
		// Let's just loop and try Create on all of them until one works.
		for _, s := range stores {
			coll := easyraft.AddCollection[Quota](s, "quotas")
			err := coll.Create(ctx, "bench", Quota{MaxTokens: 1000000000, CurrentTokens: 1000000000})
			if err == nil {
				leaderStore = s
				goto ready
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	b.Fatal("timeout waiting for leader and creating quota")

ready:
	qColl := easyraft.AddCollection[Quota](leaderStore, "quotas")

	b.Run("Mutation_Take", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// In a stable 3-node cluster on localhost, leader shouldn't change.
			_, err := qColl.Mutate(ctx, "bench", "take", nil)
			if err != nil {
				// If we lost leadership, try to find the new leader
				found := false
				for _, s := range stores {
					newColl := easyraft.AddCollection[Quota](s, "quotas")
					if _, err2 := newColl.Mutate(ctx, "bench", "take", nil); err2 == nil {
						qColl = newColl
						leaderStore = s
						found = true
						break
					}
				}
				if !found {
					b.Fatalf("mutate failed and no new leader found: %v", err)
				}
			}
		}
	})

	b.Run("Read_Linearizable", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := qColl.Read(ctx, "bench")
			if err != nil {
				// Retry on lease expiration during heavy bench load
				if strings.Contains(err.Error(), "lease has expired") {
					continue
				}
				b.Fatalf("read failed: %v", err)
			}
		}
	})

	b.Run("Read_Stale", func(b *testing.B) {
		// Stale reads can happen on any node! This shows the benefit of Raft.
		// We use n1 (first node) for stale reads to show local performance.
		staleColl := easyraft.AddCollection[Quota](stores[0], "quotas")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := staleColl.ReadStale("bench")
			if err != nil {
				b.Fatalf("read stale failed: %v", err)
			}
		}
	})
}
