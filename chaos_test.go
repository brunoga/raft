package raft_test

// Coarse-grained chaos tests: a cluster is repeatedly disrupted and must stay
// available and converge.
//
// These check liveness and eventual agreement on a value, which is a useful
// smoke test but is not a safety argument — a run that elected two leaders in
// one term and truncated a committed entry could still converge and pass. The
// safety properties are asserted continuously by TestSimChaos in
// simchaos_test.go, and end-to-end consistency by TestSimLinearizability in
// simlinearizability_test.go.

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/brunoga/raft"
)

// ---- Chaos / fault-injection tests -----------------------------------------

// TestChaos_RandomLeaderCrashes exercises the cluster under repeated leader
// crashes. After each crash a new leader must be elected and proposals must
// continue to succeed.
func TestChaos_RandomLeaderCrashes(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test skipped in short mode")
	}

	c := newCluster(t, 5)
	c.WaitLeader(electionTimeout)

	const rounds = 5
	for r := range rounds {
		// Propose some entries.
		for i := range 3 {
			cmd := fmt.Appendf(nil, "chaos=%d", r*10+i)
			if _, err := c.Propose(electionTimeout, cmd); err != nil {
				t.Fatalf("round %d propose %d: %v", r, i, err)
			}
		}

		// Crash (disconnect) the current leader.
		leaderIdx := c.WaitLeader(electionTimeout)
		c.Disconnect(leaderIdx)

		// A new leader must emerge from the remaining 4 nodes.
		deadline := time.Now().Add(electionTimeout)
		newLeader := -1
		for time.Now().Before(deadline) {
			c.Tick()
			time.Sleep(time.Millisecond)
			for i, n := range c.nodes {
				if i != leaderIdx && n.State() == raft.Leader {
					newLeader = i
					break
				}
			}
			if newLeader >= 0 {
				break
			}
		}
		if newLeader < 0 {
			t.Fatalf("round %d: no new leader after disconnecting old leader %d", r, leaderIdx)
		}

		// Heal the partition before the next round.
		c.Reconnect(leaderIdx)
		c.TickN(30) // let the old leader catch up
	}

	// Final sanity: cluster still accepts proposals.
	if _, err := c.Propose(electionTimeout, []byte("final=ok")); err != nil {
		t.Fatalf("final propose: %v", err)
	}
}

// TestChaos_NetworkPartitionAndHeal partitions the cluster into two groups for
// an extended period (so the minority group cannot elect a leader), then heals
// the partition and verifies convergence.
func TestChaos_NetworkPartitionAndHeal(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test skipped in short mode")
	}

	c := newCluster(t, 5)
	leaderIdx := c.WaitLeader(electionTimeout)

	// Propose some entries with the full cluster.
	for i := range 5 {
		cmd := fmt.Appendf(nil, "pre=%d", i)
		if _, err := c.Propose(electionTimeout, cmd); err != nil {
			t.Fatalf("pre-partition propose %d: %v", i, err)
		}
	}

	// Partition: minority = nodes 3 & 4 (indices 3,4 in a 5-node cluster).
	// The leader is in the majority group (indices 0,1,2).
	// Ensure the leader is in the majority.
	majorityGroup := []int{leaderIdx, (leaderIdx + 1) % 5, (leaderIdx + 2) % 5}
	minorityGroup := []int{(leaderIdx + 3) % 5, (leaderIdx + 4) % 5}

	for _, idx := range minorityGroup {
		c.Disconnect(idx)
	}

	// Majority can still make progress.
	for i := range 5 {
		cmd := fmt.Appendf(nil, "during=%d", i)
		if _, err := c.Propose(electionTimeout, cmd); err != nil {
			t.Fatalf("during-partition propose %d: %v", i, err)
		}
	}

	// Minority nodes must not be able to elect a leader (only 2/5 nodes).
	c.TickN(100)
	for _, idx := range minorityGroup {
		if s := c.nodes[idx].State(); s == raft.Leader {
			t.Errorf("minority node %d became Leader during partition", idx)
		}
	}
	_ = majorityGroup

	// Heal and wait for convergence.
	for _, idx := range minorityGroup {
		c.Reconnect(idx)
	}

	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		// Check that all nodes agree on "during=4".
		all := true
		for _, sm := range c.sms {
			if sm.Get("during") != "4" {
				all = false
				break
			}
		}
		if all {
			return
		}
	}

	for i, sm := range c.sms {
		t.Errorf("node %d: during=%q, want '4'", i+1, sm.Get("during"))
	}
}

// TestChaos_MessageDrops simulates a flaky network by repeatedly partitioning
// and healing individual nodes at random. The cluster must remain available
// (proposals to the majority succeed) and eventually converge.
func TestChaos_MessageDrops(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test skipped in short mode")
	}

	c := newCluster(t, 5)
	c.WaitLeader(electionTimeout)

	rng := rand.New(rand.NewPCG(7, 0))
	const totalTicks = 300

	var proposed []string
	lastPropose := 0

	for tick := range totalTicks {
		// Randomly disconnect/reconnect a non-leader node.
		if rng.IntN(10) == 0 {
			leaderIdx := c.LeaderIndex()
			candidates := make([]int, 0, 4)
			for i := range c.nodes {
				if i != leaderIdx {
					candidates = append(candidates, i)
				}
			}
			if len(candidates) > 0 {
				victim := candidates[rng.IntN(len(candidates))]
				c.Disconnect(victim)
				c.TickN(3)
				c.Reconnect(victim)
			}
		}

		c.Tick()
		time.Sleep(time.Millisecond)

		// Propose every 10 ticks if a leader exists.
		if tick-lastPropose >= 10 && c.LeaderIndex() >= 0 {
			cmd := fmt.Appendf(nil, "drop=%d", tick)
			if val, err := c.Propose(electionTimeout/4, cmd); err == nil {
				proposed = append(proposed, string(val))
				lastPropose = tick
			}
		}
	}

	if len(proposed) == 0 {
		t.Fatal("no proposals succeeded during chaos test")
	}

	// Tick until all state machines converge on the last written value.
	// Using a deadline loop (rather than a fixed TickN) is necessary because:
	//   1. The protocol must still make progress via ticks (leader heartbeats,
	//      AppendEntries to lagging followers).
	//   2. SM application is asynchronous — entries committed in the last tick
	//      may not yet be visible in the SM when TickN returns.
	last := proposed[len(proposed)-1]
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)

		allConverged := true
		for _, sm := range c.sms {
			if sm.Get("drop") != last {
				allConverged = false
				break
			}
		}
		if allConverged {
			break
		}
	}

	// All state machines must have the same last written value.
	for i, sm := range c.sms {
		if got := sm.Get("drop"); got != last {
			t.Errorf("node %d: drop=%q, want %q (last written)", i+1, got, last)
		}
	}
}
