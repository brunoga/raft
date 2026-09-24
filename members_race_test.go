package raft_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// TestMembers_ConcurrentWithConfigChange is a race-detector test for Node.Members.
//
// Members is documented as safe for concurrent use, and callers treat it that
// way: it is what Status reports through, what a rebalancer reads to find a
// group's replicas, and what operator tooling polls. It reads the peer list
// from an atomic mirror, but it read this node's own voting role straight out
// of the Config struct that the event loop rewrites on every configuration
// change. Two goroutines, one writing a bool and one reading it, with nothing
// between them: whatever a particular compiler and CPU happen to do with that
// today, it is not a value the caller can rely on, and the race detector fails
// any build that does it.
//
// Run with -race; without it this test passes whether or not the bug is there.
// Deliberately not run inside a synctest bubble. The reader below spins on
// Members with nothing to block on, which is the whole point -- it is what
// makes the race detector see the two goroutines meet. A bubble advances its
// clock only once every goroutine is durably blocked, so a spinning one stops
// it for good and this test would hang rather than run.
func TestMembers_ConcurrentWithConfigChange(t *testing.T) {
	ctx := context.Background()
	net := memtransport.NewNetwork()

	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = nullSM{}
	cfg.Transport = net.NewTransport("n1")
	cfg.TickInterval = 0
	tuneForManualTicks(&cfg)

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.Register("n1", node.Handler())
	node.Start()
	t.Cleanup(node.Stop)

	if !tickUntil(node, 3*time.Second, func() bool { return node.State() == raft.Leader }) {
		t.Fatal("node never became leader")
	}

	// A reader doing exactly what the documentation invites.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = node.Members()
			}
		}
	}()
	defer func() {
		close(stop)
		readers.Wait()
	}()

	// reconfigure applies a membership, retrying while the previous change is
	// still settling: only one may be outstanding at a time.
	reconfigure := func(members []raft.PeerConfig) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := node.ReconfigureCluster(ctx, members)
			if err == nil {
				return
			}
			if !errors.Is(err, raft.ErrConfigChangeInProgress) {
				t.Fatalf("ReconfigureCluster(%v): %v", members, err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("ReconfigureCluster(%v): still blocked by an earlier change after 5s", members)
			}
			node.Tick()
			time.Sleep(time.Millisecond)
		}
	}

	// Alternate between two memberships. Each change rewrites this node's own
	// role in Config from the event loop, whether or not the role differs.
	for i := range 10 {
		other := raft.NodeID("n2")
		if i%2 == 1 {
			other = "n3"
		}
		reconfigure([]raft.PeerConfig{
			{ID: "n1", Voter: true},
			{ID: other, Voter: false},
		})
		if !tickUntil(node, 5*time.Second, func() bool { return hasMember(node.Members(), other) }) {
			t.Fatalf("reconfiguration %d never took effect: %v", i, node.Members())
		}
	}
}
