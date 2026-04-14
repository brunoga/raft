package raft_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/brunoga/raft"
)

// BenchmarkPropose_SingleNode measures the throughput of a single-node cluster
// (no network, no follower round-trips). This gives the floor latency for the
// propose→commit→apply pipeline.
func BenchmarkPropose_SingleNode(b *testing.B) {
	n := newTestNode(b, "n1", nil)
	n.Start()
	defer n.Stop()

	// Tick until the single-node cluster elects itself.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n.Tick()
		time.Sleep(time.Millisecond)
		if n.StateSnapshot() == raft.Leader {
			break
		}
	}
	if n.StateSnapshot() != raft.Leader {
		b.Fatal("node did not become leader")
	}

	ctx := context.Background()
	b.ResetTimer()
	for i := range b.N {
		cmd := fmt.Appendf(nil, "k=%d", i)
		if _, err := n.Propose(ctx, cmd); err != nil {
			b.Fatalf("Propose: %v", err)
		}
	}
}

// BenchmarkPropose_ThreeNode measures propose throughput across a 3-node
// cluster in a tight loop, ticking between each proposal.
func BenchmarkPropose_ThreeNode(b *testing.B) {
	c := newCluster(b, 3)
	c.WaitLeader(5 * time.Second)

	b.ResetTimer()
	for i := range b.N {
		cmd := fmt.Appendf(nil, "k=%d", i)
		// Use c.Propose which ticks internally until the Future resolves.
		if _, err := c.Propose(5*time.Second, cmd); err != nil {
			b.Fatalf("Propose %d: %v", i, err)
		}
	}
}

// BenchmarkPropose_Pipelined submits a batch of proposals in parallel and
// waits for all of them, measuring throughput when the pipeline is saturated.
func BenchmarkPropose_Pipelined(b *testing.B) {
	const batchSize = 16

	c := newCluster(b, 3)
	leaderIdx := c.WaitLeader(5 * time.Second)
	leader := c.nodes[leaderIdx]

	// Background ticker.
	stopTick := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				c.Tick()
			case <-stopTick:
				return
			}
		}
	}()
	defer close(stopTick)

	ctx := context.Background()
	b.ResetTimer()

	// Each b.N iteration submits batchSize proposals in parallel and waits for
	// all of them. ns/op represents the time to commit one batch.
	errCh := make(chan error, batchSize)
	for i := range b.N {
		for j := range batchSize {
			cmd := fmt.Appendf(nil, "k=%d", i*batchSize+j)
			go func(c []byte) {
				_, err := leader.Propose(ctx, c)
				errCh <- err
			}(cmd)
		}
		for range batchSize {
			if err := <-errCh; err != nil {
				b.Fatalf("Propose: %v", err)
			}
		}
	}
}

// BenchmarkTick measures the overhead of a single tick through the event loop.
func BenchmarkTick(b *testing.B) {
	c := newCluster(b, 3)
	c.WaitLeader(5 * time.Second)

	b.ResetTimer()
	for range b.N {
		c.Tick()
	}
}
