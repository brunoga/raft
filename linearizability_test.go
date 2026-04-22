package raft_test

// Linearizability tests using the Porcupine checker.
//
// Strategy:
//  1. Run a cluster with real-time ticks.
//  2. Fire concurrent goroutines that propose key=value writes and record
//     the (call time, return time, input, output) of every operation.
//  3. Feed the history to Porcupine with a register model for each key.
//  4. Fail the test if the history is not linearizable.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/anishathalye/porcupine"

	"github.com/brunoga/raft"
)

// ---- Porcupine model for a single-key register -----------------------------

// registerOp encodes a write (set) or read (get).
type registerOp struct {
	write bool
	value int // value to write (write) or expected value (read)
}

// registerResult is the value observed by the operation.
type registerResult struct {
	value int
	ok    bool // false if the op returned an error (treated as unknown)
}

// registerModel is a Porcupine model for an integer register that supports
// write(v) → ok and read() → v.
var registerModel = porcupine.Model{
	// Initial state: register holds 0.
	Init: func() interface{} { return 0 },

	// Step: returns (ok, nextState).
	// A write always succeeds and updates the register.
	// A read succeeds iff the observed value equals the current state.
	Step: func(state, input, output interface{}) (bool, interface{}) {
		op := input.(registerOp)
		res := output.(registerResult)

		if !res.ok {
			// Error result: we don't know what happened; allow any transition.
			return true, state
		}

		if op.write {
			// Write succeeded; new state is the written value.
			return true, op.value
		}
		// Read: valid iff res.value == current state.
		return res.value == state.(int), state
	},

	DescribeOperation: func(input, output interface{}) string {
		op := input.(registerOp)
		res := output.(registerResult)
		if op.write {
			return fmt.Sprintf("write(%d) → ok=%v", op.value, res.ok)
		}
		return fmt.Sprintf("read() → %d ok=%v", res.value, res.ok)
	},
}

// ---- Linearizability test --------------------------------------------------

func TestLinearizability_ConcurrentWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("linearizability test skipped in short mode")
	}

	const (
		numClients   = 5
		opsPerClient = 8
	)

	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	// Tick in the background so the cluster stays live without concurrent Tick
	// calls interfering with proposal goroutines.
	stopTick := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.Tick()
			case <-stopTick:
				return
			}
		}
	}()
	defer close(stopTick)

	type event struct {
		clientID   int
		start, end time.Time
		op         registerOp
		result     registerResult
	}

	var mu sync.Mutex
	var events []event
	var wg sync.WaitGroup

	for cl := range numClients {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()
			// Each goroutine gets its own RNG to avoid data races.
			rng := rand.New(rand.NewPCG(uint64(clientID), 0))
			for op := range opsPerClient {
				val := clientID*1000 + op
				cmd := fmt.Appendf(nil, "x=%d", val)

				ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
				start := time.Now()
				_, err := leader.Propose(ctx, cmd)
				end := time.Now()
				cancel()
				result := registerResult{value: val, ok: err == nil}

				mu.Lock()
				events = append(events, event{
					clientID: clientID,
					start:    start,
					end:      end,
					op:       registerOp{write: true, value: val},
					result:   result,
				})
				mu.Unlock()

				time.Sleep(time.Duration(rng.IntN(3)) * time.Millisecond)
				_ = op
			}
		}(cl)
	}

	wg.Wait()

	// Build Porcupine history.
	history := make([]porcupine.Operation, len(events))
	for i, e := range events {
		history[i] = porcupine.Operation{
			ClientId: e.clientID,
			Input:    e.op,
			Call:     e.start.UnixNano(),
			Output:   e.result,
			Return:   e.end.UnixNano(),
		}
	}

	ok := porcupine.CheckOperations(registerModel, history)
	if !ok {
		t.Error("history is not linearizable")
	}
}

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
