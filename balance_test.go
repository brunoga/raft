package raft_test

// Tests for §22 Leader Balancing.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/brunoga/raft"
)

// ---- Unit tests for LeastLeadersBalancer ------------------------------------

// TestLeastLeadersBalancer_AlreadyBalanced verifies that a perfectly balanced
// view (equal leader count on every physical node) produces no transfers.
func TestLeastLeadersBalancer_AlreadyBalanced(t *testing.T) {
	view := map[raft.NodeID][]raft.GroupStatus{
		"phys0": {
			{GroupID: 1, NodeID: "g1-p0", State: raft.Leader},
			{GroupID: 2, NodeID: "g2-p0", State: raft.Follower},
		},
		"phys1": {
			{GroupID: 1, NodeID: "g1-p1", State: raft.Follower},
			{GroupID: 2, NodeID: "g2-p1", State: raft.Leader},
		},
	}

	plan := raft.LeastLeadersBalancer{}.Plan(view)
	if len(plan) != 0 {
		t.Errorf("want no transfers for balanced view, got %v", plan)
	}
}

// TestLeastLeadersBalancer_Rebalance verifies that when one physical node holds
// all leaders the balancer plans enough transfers to reach ±1 balance.
func TestLeastLeadersBalancer_Rebalance(t *testing.T) {
	// 2 physical nodes, 3 groups, all leaders on phys0.
	// Expected: 1 transfer (phys0→2, phys1→1; difference is 1).
	view := map[raft.NodeID][]raft.GroupStatus{
		"phys0": {
			{GroupID: 1, NodeID: "g1-p0", State: raft.Leader},
			{GroupID: 2, NodeID: "g2-p0", State: raft.Leader},
			{GroupID: 3, NodeID: "g3-p0", State: raft.Leader},
		},
		"phys1": {
			{GroupID: 1, NodeID: "g1-p1", State: raft.Follower},
			{GroupID: 2, NodeID: "g2-p1", State: raft.Follower},
			{GroupID: 3, NodeID: "g3-p1", State: raft.Follower},
		},
	}

	plan := raft.LeastLeadersBalancer{}.Plan(view)

	// After 1 transfer: phys0=2, phys1=1 — balanced within ±1.
	if len(plan) != 1 {
		t.Fatalf("want 1 transfer, got %d: %v", len(plan), plan)
	}
	tr := plan[0]
	if tr.From == tr.To {
		t.Errorf("transfer From == To: %v", tr)
	}
	// The transfer must originate from a phys0 node and target a phys1 node.
	if tr.From != "g1-p0" && tr.From != "g2-p0" && tr.From != "g3-p0" {
		t.Errorf("transfer From not a phys0 leader: %v", tr)
	}
	if tr.To != "g1-p1" && tr.To != "g2-p1" && tr.To != "g3-p1" {
		t.Errorf("transfer To not a phys1 replica: %v", tr)
	}
}

// TestLeastLeadersBalancer_ThreeNodes verifies multi-node balancing: 9 groups
// all initially on phys0 should produce 3 transfers (3/3/3).
func TestLeastLeadersBalancer_ThreeNodes(t *testing.T) {
	const nGroups = 9
	view := map[raft.NodeID][]raft.GroupStatus{}
	for p := range 3 {
		physID := raft.NodeID(fmt.Sprintf("phys%d", p))
		statuses := make([]raft.GroupStatus, nGroups)
		for g := range nGroups {
			state := raft.Follower
			if p == 0 {
				state = raft.Leader
			}
			statuses[g] = raft.GroupStatus{
				GroupID: uint64(g + 1),
				NodeID:  raft.NodeID(fmt.Sprintf("g%d-p%d", g+1, p)),
				State:   state,
			}
		}
		view[physID] = statuses
	}

	plan := raft.LeastLeadersBalancer{}.Plan(view)

	// After 6 transfers: phys0=3, phys1=3, phys2=3.
	if len(plan) != 6 {
		t.Fatalf("want 6 transfers (9 groups → 3/3/3), got %d: %v", len(plan), plan)
	}

	// Simulate the transfers and verify final balance.
	count := map[raft.NodeID]int{"phys0": nGroups}
	for _, tr := range plan {
		// Determine source and target physical node from the node ID convention.
		for p := range 3 {
			if tr.From == raft.NodeID(fmt.Sprintf("g%d-p%d", tr.GroupID, p)) {
				count[raft.NodeID(fmt.Sprintf("phys%d", p))]--
			}
			if tr.To == raft.NodeID(fmt.Sprintf("g%d-p%d", tr.GroupID, p)) {
				count[raft.NodeID(fmt.Sprintf("phys%d", p))]++
			}
		}
	}
	for physID, n := range count {
		if n != 3 {
			t.Errorf("physical %s: %d leaders after rebalance, want 3", physID, n)
		}
	}
}

// TestLeastLeadersBalancer_SingleNode verifies that a single physical node
// (no peers) produces no transfers — nothing to balance across.
func TestLeastLeadersBalancer_SingleNode(t *testing.T) {
	view := map[raft.NodeID][]raft.GroupStatus{
		"phys0": {
			{GroupID: 1, NodeID: "g1-p0", State: raft.Leader},
			{GroupID: 2, NodeID: "g2-p0", State: raft.Leader},
		},
	}
	plan := raft.LeastLeadersBalancer{}.Plan(view)
	if len(plan) != 0 {
		t.Errorf("want no transfers for single node, got %v", plan)
	}
}

// ---- Integration test: BalanceController converges --------------------------

// TestBalanceController_ConvergesUniform starts with all group leaders on
// physical node 0, then runs the BalanceController and verifies that leaders
// are distributed ±1 across all physical nodes within the election timeout.
func TestBalanceController_ConvergesUniform(t *testing.T) {
	if testing.Short() {
		t.Skip("balance integration test skipped in short mode")
	}

	const (
		numGroups   = 6
		numPhysical = 3 // expected final balance: 2 leaders per node
	)

	c := newMRCluster(t, numGroups, numPhysical)
	c.WaitAllLeaders(electionTimeout)

	// Force all leaders onto physical node 0 by transferring any group whose
	// leader is currently on a different physical node.
	for g := range numGroups {
		target := c.ids[g][0]
		deadline := time.Now().Add(electionTimeout)
		for time.Now().Before(deadline) {
			p := c.leaderOf(g)
			if p == 0 {
				break
			}
			if p < 0 {
				c.Tick()
				time.Sleep(time.Millisecond)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			_ = c.groups[g][p].TransferLeadership(ctx, target)
			cancel()
			c.TickN(10)
		}
		if c.leaderOf(g) != 0 {
			t.Fatalf("group %d: could not transfer leader to physical 0 before test start", g+1)
		}
	}

	// Confirm all 6 leaders are on physical node 0.
	for g := range numGroups {
		if p := c.leaderOf(g); p != 0 {
			t.Fatalf("group %d: leader is on physical %d, want 0", g+1, p)
		}
	}

	// Build the providers map for the BalanceController.
	providers := make(map[raft.NodeID]raft.NodeProvider, numPhysical)
	for p := range numPhysical {
		providers[raft.NodeID(fmt.Sprintf("phys%d", p))] = c.mgrs[p]
	}

	ctrl := raft.NewBalanceController(providers, raft.LeastLeadersBalancer{}, 20*time.Millisecond)

	go ctrl.Run(t.Context())

	// Tick the cluster and wait for leaders to converge to ±1 per physical node.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)

		leadersByPhys := make([]int, numPhysical)
		allHaveLeader := true
		for g := range numGroups {
			p := c.leaderOf(g)
			if p < 0 {
				allHaveLeader = false
				break
			}
			leadersByPhys[p]++
		}
		if !allHaveLeader {
			continue
		}

		// Check ±1 balance: max - min ≤ 1.
		minC, maxC := leadersByPhys[0], leadersByPhys[0]
		for _, n := range leadersByPhys[1:] {
			if n < minC {
				minC = n
			}
			if n > maxC {
				maxC = n
			}
		}
		if maxC-minC <= 1 {
			return // balanced
		}
	}

	// Report final distribution.
	leadersByPhys := make([]int, numPhysical)
	for g := range numGroups {
		if p := c.leaderOf(g); p >= 0 {
			leadersByPhys[p]++
		}
	}
	t.Errorf("leaders not balanced within electionTimeout: %v (want each ~%d ±1)",
		leadersByPhys, numGroups/numPhysical)
}
