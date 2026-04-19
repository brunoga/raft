package raft_test

import (
	"context"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

func TestWitness_QuorumExclusion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	net := memtransport.NewNetwork()

	// 2 voters (n1, n2), 1 witness (n3).
	// Total voters = 2. Quorum = 2/2 + 1 = 2.
	ids := []raft.NodeID{"n1", "n2", "n3"}
	nodes := make([]*raft.Node, 3)
	for i, id := range ids {
		voter := true
		if id == "n3" {
			voter = false
		}
		peers := make([]raft.PeerConfig, 0, 2)
		for _, other := range ids {
			if other == id {
				continue
			}
			isOtherVoter := true
			if other == "n3" {
				isOtherVoter = false
			}
			peers = append(peers, raft.PeerConfig{ID: other, Voter: isOtherVoter})
		}

		cfg := raft.DefaultConfig()
		cfg.ID = id
		cfg.Voter = voter
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = &kvSM{data: make(map[string]string)}
		cfg.Transport = net.NewTransport(id)
		cfg.TickInterval = 0

		n, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(%s): %v", id, err)
		}
		nodes[i] = n
		n.Start()
		t.Cleanup(n.Stop)
	}

	// Helper to drive ticks.
	tick := func() {
		for _, n := range nodes {
			n.Tick()
		}
		time.Sleep(2 * time.Millisecond)
	}

	// 1. Elect a leader. Quorum is 2, so n1+n2 can elect.
	for range 100 {
		tick()
	}

	leaderIdx := -1
	for i, n := range nodes {
		if n.Status().State == raft.Leader {
			leaderIdx = i
			break
		}
	}
	if leaderIdx == -1 {
		t.Fatal("no leader elected with 2/2 voters")
	}
	if ids[leaderIdx] == "n3" {
		t.Fatal("witness elected as leader")
	}

	// 2. Partition one voter (n2).
	net.Partition("n2")

	// 3. Propose something. Should NOT commit because only n1 (voter) and n3 (witness) are up.
	// Voter count = 1/2. Not a majority.
	propCtx, propCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer propCancel()
	_, err := nodes[leaderIdx].Propose(propCtx, []byte("should-fail"))
	if err == nil {
		t.Error("proposal committed without voter quorum (witness must not count)")
	}

	// 4. Heal partition. Should now commit.
	net.Heal("n2")
	for range 50 {
		tick()
	}
	if _, err := nodes[leaderIdx].Propose(ctx, []byte("should-succeed")); err != nil {
		t.Errorf("proposal failed after healing partition: %v", err)
	}
}

func TestWitness_NoElection(t *testing.T) {
	net := memtransport.NewNetwork()

	// Single witness node.
	cfg := raft.DefaultConfig()
	cfg.ID = "witness"
	cfg.Voter = false
	cfg.Peers = nil
	cfg.Storage = memstore.New()
	cfg.StateMachine = &kvSM{data: make(map[string]string)}
	cfg.Transport = net.NewTransport("witness")
	cfg.TickInterval = 0

	n, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	n.Start()
	defer n.Stop()

	// Drive ticks past election timeout.
	for range 100 {
		n.Tick()
	}

	if n.Status().State != raft.Follower {
		t.Errorf("witness node changed state to %v, want Follower", n.Status().State)
	}
	if n.Status().Term != 0 {
		t.Errorf("witness node incremented term to %d, want 0", n.Status().Term)
	}
}

func TestWitness_Promotion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	net := memtransport.NewNetwork()

	// Start with 1 voter (n1).
	cfg1 := raft.DefaultConfig()
	cfg1.ID = "n1"
	cfg1.Voter = true
	cfg1.Storage = memstore.New()
	cfg1.StateMachine = &kvSM{data: make(map[string]string)}
	cfg1.Transport = net.NewTransport("n1")
	cfg1.TickInterval = 0
	n1, _ := raft.New(&cfg1)
	n1.Start()
	defer n1.Stop()

	// Start n2 as witness.
	cfg2 := raft.DefaultConfig()
	cfg2.ID = "n2"
	cfg2.Voter = false
	cfg2.Peers = []raft.PeerConfig{{ID: "n1", Voter: true}}
	cfg2.Storage = memstore.New()
	cfg2.StateMachine = &kvSM{data: make(map[string]string)}
	cfg2.Transport = net.NewTransport("n2")
	cfg2.TickInterval = 0
	n2, _ := raft.New(&cfg2)
	n2.Start()
	defer n2.Stop()

	tick := func() {
		n1.Tick()
		n2.Tick()
		time.Sleep(2 * time.Millisecond)
	}

	// Elect n1.
	for range 100 {
		tick()
	}

	// Add n2 as witness via ReconfigureCluster.
	err := n1.ReconfigureCluster(ctx, []raft.PeerConfig{
		{ID: "n1", Voter: true},
		{ID: "n2", Voter: false},
	})
	if err != nil {
		t.Fatalf("ReconfigureCluster(witness): %v", err)
	}

	for range 50 {
		tick()
	}

	// Verify n2 is replicating.
	_, err = n1.Propose(ctx, []byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		tick()
	}
	if n2.Status().LastApplied != n1.Status().LastApplied {
		t.Fatalf("witness did not replicate: n1=%d, n2=%d", n1.Status().LastApplied, n2.Status().LastApplied)
	}

	// Promote n2 to voter.
	err = n1.ReconfigureCluster(ctx, []raft.PeerConfig{
		{ID: "n1", Voter: true},
		{ID: "n2", Voter: true},
	})
	if err != nil {
		t.Fatalf("ReconfigureCluster(promote): %v", err)
	}

	for range 50 {
		tick()
	}

	if n2.Status().State != raft.Follower && n2.Status().State != raft.Leader {
		t.Fatalf("n2 should be Follower or Leader, got %v", n2.Status().State)
	}
}
