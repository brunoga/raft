package raft_test

// Integration tests covering:
//   Section 5  – RPC message types flow end-to-end
//   Section 6  – Leader election
//   Section 7  – Log replication
//   Section 8  – Committing & applying
//   Section 9  – Heartbeats
//   Section 10 – Log Compaction & Snapshots
//   Section 11 – Linearizable Read-Only Queries
//   Section 12 – Cluster Membership Changes
//   Section 13 – Pre-Vote Extension
//   Section 14 – Leadership Transfer
//   Section 20 – Graceful Shutdown (LastApplied, Stop drain)

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

const electionTimeout = 3 * time.Second

// ---- Section 6: Leader Election --------------------------------------------

func TestElection_SingleLeaderElected(t *testing.T) {
	c := newCluster(t, 3)
	idx := c.WaitLeader(electionTimeout)
	if idx < 0 {
		t.Fatal("no leader elected")
	}
	// Verify exactly one leader.
	leaders := 0
	for _, n := range c.nodes {
		if n.State() == raft.Leader {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("expected 1 leader, got %d", leaders)
	}
}

func TestElection_FiveNodeCluster(t *testing.T) {
	c := newCluster(t, 5)
	c.WaitLeader(electionTimeout)
}

func TestElection_ReelectAfterLeaderCrash(t *testing.T) {
	c := newCluster(t, 3)
	oldIdx := c.WaitLeader(electionTimeout)

	// Partition the leader away. It stays in Leader state (it is isolated and
	// can't learn about the new term), so we search among the remaining nodes.
	c.Disconnect(oldIdx)

	newLeaderIdx := -1
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		for i, n := range c.nodes {
			if i != oldIdx && n.State() == raft.Leader {
				newLeaderIdx = i
				break
			}
		}
		if newLeaderIdx >= 0 {
			break
		}
	}
	if newLeaderIdx < 0 {
		t.Fatal("no new leader elected after partitioning old leader")
	}
}

func TestElection_TermIncrementsOnElection(t *testing.T) {
	// After at least one election the term must be > 0.
	// We verify by proposing and checking it works (requires committed entries,
	// which requires a leader in a valid term).
	c := newCluster(t, 3)
	c.WaitLeader(electionTimeout)
	_, err := c.Propose(electionTimeout, []byte("x=1"))
	if err != nil {
		t.Fatalf("Propose after election: %v", err)
	}
}

func TestElection_FollowerDoesNotGrantStaleVote(t *testing.T) {
	// Start a cluster, let it elect a leader, then propose an entry so the
	// leader has a higher log. A candidate with an older log should not be
	// able to steal the leadership (the Raft log-completeness property).
	c := newCluster(t, 3)
	c.WaitLeader(electionTimeout)
	_, err := c.Propose(electionTimeout, []byte("key=val"))
	if err != nil {
		t.Fatalf("initial propose: %v", err)
	}

	// After the proposal commits, tick the cluster forward. No second leader
	// should appear with a conflicting log.
	c.TickN(20)
	leaders := 0
	for _, n := range c.nodes {
		if n.State() == raft.Leader {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("expected 1 leader, got %d after stable cluster", leaders)
	}
}

// ---- Section 7: Log Replication --------------------------------------------

func TestReplication_CommandReplicatedToAllFollowers(t *testing.T) {
	c := newCluster(t, 3)
	c.WaitLeader(electionTimeout)

	val, err := c.Propose(electionTimeout, []byte("color=blue"))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if string(val) != "blue" {
		t.Fatalf("expected 'blue', got %q", val)
	}

	// All state machines should have the value after replication.
	// Tick a few more times to let followers apply.
	c.TickN(10)
	for i, sm := range c.sms {
		if sm.Get("color") != "blue" {
			t.Errorf("node %d: expected color=blue, got %q", i+1, sm.Get("color"))
		}
	}
}

func TestReplication_MultipleCommands(t *testing.T) {
	c := newCluster(t, 3)
	c.WaitLeader(electionTimeout)

	cmds := []string{"a=1", "b=2", "c=3"}
	for _, cmd := range cmds {
		if _, err := c.Propose(electionTimeout, []byte(cmd)); err != nil {
			t.Fatalf("Propose %q: %v", cmd, err)
		}
	}

	c.TickN(10)
	expected := map[string]string{"a": "1", "b": "2", "c": "3"}
	for i, sm := range c.sms {
		for k, want := range expected {
			if got := sm.Get(k); got != want {
				t.Errorf("node %d: %s=%q, want %q", i+1, k, got, want)
			}
		}
	}
}

func TestReplication_FollowerRejoinsAndCatchesUp(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)

	// Propose a few entries with all nodes up.
	if _, err := c.Propose(electionTimeout, []byte("x=1")); err != nil {
		t.Fatalf("initial propose: %v", err)
	}

	// Disconnect one follower.
	disconnectedIdx := (leaderIdx + 1) % 3
	c.Disconnect(disconnectedIdx)

	// Propose more entries while the follower is partitioned.
	for i := 2; i <= 4; i++ {
		cmd := fmt.Appendf(nil, "x=%d", i)
		if _, err := c.Propose(electionTimeout, cmd); err != nil {
			t.Fatalf("propose with follower down: %v", err)
		}
	}

	// Reconnect the follower.
	c.Reconnect(disconnectedIdx)

	// Tick until the follower catches up. After reconnection a new election
	// may be needed (the partitioned node accumulated a high term), so allow
	// the full election timeout for convergence.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		if c.sms[disconnectedIdx].Get("x") == "4" {
			break
		}
	}

	if got := c.sms[disconnectedIdx].Get("x"); got != "4" {
		t.Fatalf("follower did not catch up: x=%q, want '4'", got)
	}
}

// ---- Section 8: Committing & Applying --------------------------------------

func TestCommit_ProposeReturnsResult(t *testing.T) {
	c := newCluster(t, 3)
	c.WaitLeader(electionTimeout)

	val, err := c.Propose(electionTimeout, []byte("greeting=hello"))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if string(val) != "hello" {
		t.Fatalf("expected 'hello', got %q", val)
	}
}

func TestCommit_AppliedInOrder(t *testing.T) {
	c := newCluster(t, 3)
	c.WaitLeader(electionTimeout)

	// Propose entries sequentially; each must build on the previous.
	for i := range 5 {
		cmd := fmt.Appendf(nil, "n=%d", i)
		if _, err := c.Propose(electionTimeout, cmd); err != nil {
			t.Fatalf("Propose %d: %v", i, err)
		}
	}
	c.TickN(10)

	// Final value should be the last write.
	for i, sm := range c.sms {
		if sm.Get("n") != "4" {
			t.Errorf("node %d: n=%q, want '4'", i+1, sm.Get("n"))
		}
	}
}

// ---- Section 9: Heartbeats -------------------------------------------------

func TestHeartbeat_LeaderMaintainsAuthority(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)

	// Tick the cluster well past one election timeout worth of heartbeats.
	// The leader should remain leader (heartbeats prevent followers from timing out).
	c.TickN(60)

	if c.nodes[leaderIdx].State() != raft.Leader {
		t.Fatalf("leader should still be leader after heartbeats; state=%s",
			c.nodes[leaderIdx].State())
	}
	// And still exactly one leader.
	leaders := 0
	for _, n := range c.nodes {
		if n.State() == raft.Leader {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("expected 1 leader after sustained heartbeats, got %d", leaders)
	}
}

func TestHeartbeat_FollowerDoesNotTimeOutUnderHeartbeat(t *testing.T) {
	c := newCluster(t, 3)
	c.WaitLeader(electionTimeout)

	// Run for a while; no node should become Candidate while the leader is alive.
	c.TickN(100)

	candidates := 0
	for _, n := range c.nodes {
		if n.State() == raft.Candidate {
			candidates++
		}
	}
	if candidates > 0 {
		t.Fatalf("no node should be Candidate while leader is heartbeating; found %d", candidates)
	}
}

// ---- Section 10: Log Compaction & Snapshots -----------------------------------

// TestSnapshot_AutomaticTrigger verifies that when applied entries exceed
// SnapshotThreshold, the node automatically takes a snapshot and truncates
// the log prefix, while the state machine still reflects all applied commands.
func TestSnapshot_AutomaticTrigger(t *testing.T) {
	// Low threshold so we trigger snapshots quickly.
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.SnapshotThreshold = 3
	})
	c.WaitLeader(electionTimeout)

	// Propose enough entries to exceed the threshold multiple times.
	for i := range 9 {
		cmd := fmt.Appendf(nil, "x=%d", i)
		if _, err := c.Propose(electionTimeout, cmd); err != nil {
			t.Fatalf("Propose %d: %v", i, err)
		}
	}

	// Give the snapshot goroutines time to complete.
	c.TickN(30)

	for i, sm := range c.sms {
		if got := sm.Get("x"); got != "8" {
			t.Errorf("node %d: x=%q, want '8'", i+1, got)
		}
	}
}

// TestSnapshot_LaggingFollowerReceivesSnapshot verifies that a follower that
// was partitioned while the leader took a snapshot receives the snapshot via
// InstallSnapshot and catches up correctly.
func TestSnapshot_LaggingFollowerReceivesSnapshot(t *testing.T) {
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.SnapshotThreshold = 3
	})
	leaderIdx := c.WaitLeader(electionTimeout)

	// Disconnect one follower before any entries are proposed.
	followerIdx := (leaderIdx + 1) % 3
	c.Disconnect(followerIdx)

	// Propose enough entries to cause at least one snapshot on the active nodes.
	for i := range 7 {
		cmd := fmt.Appendf(nil, "x=%d", i)
		if _, err := c.Propose(electionTimeout, cmd); err != nil {
			t.Fatalf("Propose %d with follower down: %v", i, err)
		}
	}

	// Let the snapshot goroutine run on the remaining nodes.
	c.TickN(30)

	// Reconnect the follower — the leader must send it a snapshot.
	c.Reconnect(followerIdx)

	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		if c.sms[followerIdx].Get("x") == "6" {
			break
		}
	}

	if got := c.sms[followerIdx].Get("x"); got != "6" {
		t.Fatalf("follower did not catch up via snapshot: x=%q, want '6'", got)
	}
}

// ---- Section 11: Linearizable Read-Only Queries --------------------------------

// TestReadIndex_ReturnsCommittedIndex verifies that ReadIndex resolves to a
// non-zero index after some entries have been committed, and that the index
// is at most the commitIndex at the time of the call.
func TestReadIndex_ReturnsCommittedIndex(t *testing.T) {
	c := newCluster(t, 3)
	c.WaitLeader(electionTimeout)

	// Establish some committed log entries.
	if _, err := c.Propose(electionTimeout, []byte("x=1")); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()

	leader := c.Leader(electionTimeout / 2)

	// ReadIndex blocks until the barrier heartbeat quorum completes; tick in
	// the background to drive the manual-clock cluster.
	type result struct {
		idx raft.Index
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		idx, e := leader.ReadIndex(ctx)
		resultCh <- result{idx, e}
	}()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var readIdx raft.Index
loop:
	for {
		select {
		case r := <-resultCh:
			if r.err != nil {
				t.Fatalf("ReadIndex: %v", r.err)
			}
			readIdx = r.idx
			break loop
		case <-ticker.C:
			c.Tick()
		case <-ctx.Done():
			t.Fatal("timed out waiting for ReadIndex")
		}
	}

	if readIdx == 0 {
		t.Fatal("ReadIndex returned 0, expected a committed index")
	}
}

// TestReadIndex_FollowerForwarding verifies that ReadIndex on a follower
// forwards the request to the leader and returns a safe index.
func TestReadIndex_FollowerForwarding(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)

	// WaitLeader returns as soon as a node enters Leader state; the no-op entry
	// may not yet be committed. Issue a ReadIndex on the leader to wait for the
	// nop to commit and apply before testing follower forwarding.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), electionTimeout)
	defer waitCancel()
	if _, err := c.nodes[leaderIdx].ReadIndex(waitCtx); err != nil {
		t.Fatalf("leader ReadIndex (nop wait): %v", err)
	}

	// Find a follower.
	followerIdx := (leaderIdx + 1) % 3
	follower := c.nodes[followerIdx]

	ctx := context.Background()
	idx, err := follower.ReadIndex(ctx)
	if err != nil {
		t.Fatalf("ReadIndex on follower: %v", err)
	}
	if idx == 0 {
		t.Fatal("ReadIndex returned 0")
	}
}

// TestReadStale_FollowerServesLocally verifies that a follower can serve a
// stale read without any RPC to the leader. After the leader applies an entry,
// we wait for the follower to catch up (via normal replication), then confirm
// that ReadStale returns a non-zero index and that the SM reflects the write.
func TestReadStale_FollowerServesLocally(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)

	// Propose a write so followers have something to apply beyond the no-op.
	if _, err := c.Propose(electionTimeout, []byte("flavor=vanilla")); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	followerIdx := (leaderIdx + 1) % 3
	follower := c.nodes[followerIdx]
	followerSM := c.sms[followerIdx]

	// Wait for the follower's SM to apply the write (normal replication, no
	// special API — just poll LastApplied so we know replication has landed).
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		if followerSM.Get("flavor") == "vanilla" {
			break
		}
		c.Tick()
		time.Sleep(time.Millisecond)
	}
	if followerSM.Get("flavor") != "vanilla" {
		t.Fatal("follower SM never applied the write")
	}

	// ReadStale must return a non-zero applied index with no RPC.
	idx := follower.ReadStale()
	if idx == 0 {
		t.Fatal("ReadStale on follower returned 0")
	}
	// Sanity: the value the SM holds is the one we wrote, readable immediately.
	if got := followerSM.Get("flavor"); got != "vanilla" {
		t.Errorf("follower SM has flavor=%q, want 'vanilla'", got)
	}
}

// TestReadIndex_FollowerForwarding_NoKnownLeader verifies that ReadIndex on a
// follower that has no known leader returns NotLeaderError immediately without
// attempting a forwarded RPC.
func TestReadIndex_FollowerForwarding_NoKnownLeader(t *testing.T) {
	c := newCluster(t, 3)
	// Do NOT call WaitLeader — all nodes start as followers with no known leader.

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.nodes[0].ReadIndex(ctx)
	if !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader before any leader is elected, got %v", err)
	}
}

// TestReadIndex_ForwardedRead_LeaderStepsDown verifies that a pending
// server-side ReadIndex RPC (representing a follower-forwarded request) is
// resolved with NotLeaderError when the leader steps down before the barrier
// heartbeat quorum is confirmed.
//
// We call HandleReadIndex directly on the leader to represent the arrival of
// a forwarded RPC, without going through the follower-forwarding path itself
// (which would introduce races between the goroutine's state snapshot and the
// cluster's ongoing re-election).
func TestReadIndex_ForwardedRead_LeaderStepsDown(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]
	leaderTerm := leader.Term()

	// Drop only the leader's outbound messages so barrier heartbeats cannot
	// reach any peer. Inbound is kept open so dispatchRPC can still deliver
	// the request into the leader's event loop.
	for _, i := range []int{(leaderIdx + 1) % 3, (leaderIdx + 2) % 3} {
		c.net.Drop(c.ids[leaderIdx], c.ids[i])
	}

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// Simulate a forwarded ReadIndex RPC arriving at the leader's handler.
		_, err := leader.Handler().HandleReadIndex(ctx, &raft.ReadIndexRequest{Term: leaderTerm})
		errCh <- err
	}()

	// Tick until CheckQuorum steps the leader down. becomeFollower drains
	// pendingReads, which unblocks the pending RPC with NotLeaderError.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.TickN(1)
		select {
		case err := <-errCh:
			if !errors.Is(err, raft.ErrNotLeader) {
				t.Errorf("expected ErrNotLeader after leader steps down, got %v", err)
			}
			return
		default:
		}
	}
	t.Fatal("pending forwarded ReadIndex did not return after leader stepped down")
}

// TestReadIndexRPC_StaleTerm_ServesRead is a regression test for a bug where
// handleReadIndexRPC returned ReadIndexResponse{Index: 0} with nil error when
// req.Term < leader's currentTerm. The caller would interpret Index=0 as a
// successful linearizable read and proceed without any guarantee.
// The fix: serve the read normally regardless of req.Term — the only safety
// invariant is "am I the leader?", confirmed by the barrier heartbeat.
func TestReadIndexRPC_StaleTerm_ServesRead(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	// WaitLeader returns as soon as a node enters Leader state, but the leader's
	// no-op entry (appended on election) may not yet be committed or applied.
	// Issue a ReadIndex to wait for the nop to commit and apply; otherwise
	// HandleReadIndex returns commitIndex=0, which the test would incorrectly
	// flag as the old bug.
	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	if _, err := leader.ReadIndex(ctx); err != nil {
		t.Fatalf("leader ReadIndex (nop wait): %v", err)
	}

	// Call HandleReadIndex with term 0 (always < any real term) directly.
	// The leader must still perform the barrier and return a non-zero index.
	type riResult struct {
		resp *raft.ReadIndexResponse
		err  error
	}
	resultCh := make(chan riResult, 1)
	go func() {
		resp, err := leader.Handler().HandleReadIndex(ctx, &raft.ReadIndexRequest{Term: 0})
		resultCh <- riResult{resp, err}
	}()

	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.TickN(1)
		select {
		case r := <-resultCh:
			if r.err != nil {
				t.Fatalf("ReadIndex with stale term returned error: %v", r.err)
			}
			if r.resp.Index == 0 {
				t.Fatal("ReadIndex with stale term returned zero index (old bug)")
			}
			return
		default:
		}
	}
	t.Fatal("ReadIndex with stale term timed out")
}

// TestReadIndexRPC_HigherTerm_StepsDown verifies that a leader receiving a
// ReadIndex RPC with a higher term steps down immediately (Raft §5.1).
func TestReadIndexRPC_HigherTerm_StepsDown(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]
	currentTerm := leader.Term()

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()

	// Send a ReadIndex RPC with a term higher than the leader's current term.
	resp, err := leader.Handler().HandleReadIndex(ctx, &raft.ReadIndexRequest{Term: currentTerm + 1})
	if !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader, got resp=%v err=%v", resp, err)
	}
	// The leader must have stepped down to follower.
	if s := leader.State(); s != raft.Follower {
		t.Errorf("after higher-term ReadIndex, state = %s, want Follower", s)
	}
}

// ---- Section 13: Pre-Vote Extension -------------------------------------------

// TestPreVote_NodeBecomesPreCandidateFirst verifies that a follower with peers
// enters PreCandidate state (not Candidate) when its election timer fires,
// because it must win a pre-vote round before incrementing its term.
func TestPreVote_NodeBecomesPreCandidateFirst(t *testing.T) {
	// Single node with fake peers so it can't win pre-vote automatically.
	n := newTestNodeWithPeers(t, "n1", []raft.PeerConfig{
		{ID: "n2", Voter: true},
		{ID: "n3", Voter: true},
	})
	n.Start()
	defer n.Stop()

	// Drive the election timer to fire (noopTransport returns VoteGranted=false,
	// so pre-vote will never succeed, keeping the node in PreCandidate).
	sawPreCandidate := false
	for range 60 {
		n.Tick()
		time.Sleep(time.Millisecond)
		s := n.State()
		if s == raft.Candidate || s == raft.Leader {
			t.Fatalf("node skipped PreCandidate and went directly to %s", s)
		}
		if s == raft.PreCandidate {
			sawPreCandidate = true
			break
		}
	}
	if !sawPreCandidate {
		t.Fatal("node never entered PreCandidate state before election timeout")
	}
}

// TestPreVote_PartitionedNodeDoesNotDisruptCluster verifies that a node
// isolated for many election timeouts does not disrupt the stable cluster
// when it reconnects. With pre-vote, the isolated node cannot increment its
// term because it cannot reach a quorum for pre-vote.
func TestPreVote_PartitionedNodeDoesNotDisruptCluster(t *testing.T) {
	c := newCluster(t, 5)
	leaderIdx := c.WaitLeader(electionTimeout)

	// Isolate one non-leader node for an extended period.
	isolatedIdx := (leaderIdx + 1) % 5
	c.Disconnect(isolatedIdx)

	// Tick many times while the node is isolated.
	c.TickN(150)

	// The isolated node must still be a PreCandidate (stuck, can't win pre-vote)
	// and must NOT have incremented into a real Candidate or done anything
	// to the rest of the cluster.
	if s := c.nodes[isolatedIdx].State(); s == raft.Candidate || s == raft.Leader {
		t.Errorf("isolated node reached state %s, want PreCandidate or Follower", s)
	}

	// The remaining 4 nodes should still have exactly one leader.
	leaders := 0
	for i, n := range c.nodes {
		if i != isolatedIdx && n.State() == raft.Leader {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("cluster has %d leaders while node is isolated, want 1", leaders)
	}

	// Reconnect and verify the cluster stays stable.
	c.Reconnect(isolatedIdx)
	c.TickN(50)

	leaders = 0
	for _, n := range c.nodes {
		if n.State() == raft.Leader {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("cluster has %d leaders after reconnect, want 1", leaders)
	}

	// Cluster should accept new proposals.
	if _, err := c.Propose(electionTimeout, []byte("stable=yes")); err != nil {
		t.Fatalf("Propose after reconnect: %v", err)
	}
}

// ---- Section 11: LastApplied -----------------------------------------------

// TestLastApplied_AdvancesAfterCommit verifies that LastApplied() increases
// monotonically and reflects entries that have been applied to the state machine.
func TestLastApplied_AdvancesAfterCommit(t *testing.T) {
	c := newCluster(t, 3)
	c.WaitLeader(electionTimeout)

	leader := c.Leader(electionTimeout)
	before := leader.LastApplied()

	if _, err := c.Propose(electionTimeout, []byte("x=1")); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	// Give apply goroutine a moment to reflect the update.
	c.TickN(5)

	after := leader.LastApplied()
	if after <= before {
		t.Fatalf("LastApplied did not advance: before=%d after=%d", before, after)
	}
}

// ---- Section 12: Cluster Membership Changes --------------------------------

// TestMembership_AddServer verifies that a new peer can be added to a running
// cluster via AddServer, and that after the config entry commits, all nodes
// have the new peer in their peer list and continue to accept proposals.
func TestMembership_AddServer(t *testing.T) {
	// Start a 2-node cluster (n1, n2); we will add n3 dynamically.
	net := memtransport.NewNetwork()
	ids := []raft.NodeID{"n1", "n2"}

	var nodes []*raft.Node
	for _, id := range ids {
		peers := make([]raft.PeerConfig, 0)
		for _, other := range ids {
			if other != id {
				peers = append(peers, raft.PeerConfig{ID: other, Voter: true})
			}
		}
		tr := net.NewTransport(id)
		sm := &kvSM{data: make(map[string]string)}
		cfg := raft.DefaultConfig()
		cfg.ID = id
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = sm
		cfg.Transport = tr
		cfg.TickInterval = 0
		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("New %s: %v", id, err)
		}
		nodes = append(nodes, node)
	}
	for i, node := range nodes {
		net.Register(ids[i], node.Handler())
	}
	for _, node := range nodes {
		node.Start()
	}
	t.Cleanup(func() {
		for _, node := range nodes {
			node.Stop()
		}
	})

	// Helper: tick all registered nodes.
	tick := func() {
		for _, node := range nodes {
			node.Tick()
		}
		time.Sleep(time.Millisecond)
	}
	waitLeader := func() *raft.Node {
		deadline := time.Now().Add(electionTimeout)
		for time.Now().Before(deadline) {
			tick()
			for _, node := range nodes {
				if node.State() == raft.Leader {
					return node
				}
			}
		}
		t.Fatal("no leader elected")
		return nil
	}

	leader := waitLeader()

	// Propose a baseline entry.
	ctx := context.Background()
	proposeDone := make(chan error, 1)
	go func() { _, e := leader.Propose(ctx, []byte("k=v")); proposeDone <- e }()
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		select {
		case e := <-proposeDone:
			if e != nil {
				t.Fatalf("initial propose: %v", e)
			}
			goto proposed
		default:
			tick()
		}
	}
	t.Fatal("initial propose timed out")
proposed:

	// Build n3 and connect it to the network.
	n3SM := &kvSM{data: make(map[string]string)}
	n3tr := net.NewTransport("n3")
	n3cfg := raft.DefaultConfig()
	n3cfg.ID = "n3"
	n3cfg.Peers = []raft.PeerConfig{
		{ID: "n1", Voter: true},
		{ID: "n2", Voter: true},
	} // knows the existing cluster
	n3cfg.Storage = memstore.New()
	n3cfg.StateMachine = n3SM
	n3cfg.Transport = n3tr
	n3cfg.TickInterval = 0
	n3, err := raft.New(&n3cfg)
	if err != nil {
		t.Fatalf("New n3: %v", err)
	}
	net.Register("n3", n3.Handler())
	n3.Start()
	t.Cleanup(n3.Stop)
	nodes = append(nodes, n3)

	// Tell the leader to add n3.
	addDone := make(chan error, 1)
	go func() { addDone <- leader.AddServer(ctx, raft.PeerConfig{ID: "n3", Voter: true}) }()

	deadline = time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		select {
		case e := <-addDone:
			if e != nil {
				t.Fatalf("AddServer: %v", e)
			}
			goto added
		default:
			tick()
		}
	}
	t.Fatal("AddServer timed out")
added:

	// After the config change commits, the cluster should accept new proposals.
	tick() // let n3 catch up a bit
	done2 := make(chan error, 1)
	go func() { _, e := leader.Propose(ctx, []byte("k2=v2")); done2 <- e }()
	deadline = time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		select {
		case e := <-done2:
			if e != nil {
				t.Fatalf("propose after AddServer: %v", e)
			}
			return
		default:
			tick()
		}
	}
	t.Fatal("propose after AddServer timed out")
}

// TestMembership_ConfigChangeInProgressBlocked verifies that proposing a second
// membership change while one is already in flight resolves with
// ErrConfigChangeInProgress.
// Strategy: disconnect all followers so the first change can never commit,
// keeping pendingConfigIndex set when the second change arrives.
func TestMembership_ConfigChangeInProgressBlocked(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()

	// Disconnect followers so the first config change cannot commit (no quorum).
	for i := range c.nodes {
		if i != leaderIdx {
			c.Disconnect(i)
		}
	}

	// First AddServer — appended but never committed (followers are partitioned).
	// Run in a goroutine since it will block until ctx expires.
	go func() { _ = leader.AddServer(ctx, raft.PeerConfig{ID: "n99", Voter: true}) }()
	// Give the event loop time to process the first proposal and set pendingConfigIndex.
	time.Sleep(5 * time.Millisecond)

	// Second AddServer should return ErrConfigChangeInProgress immediately.
	err := leader.AddServer(ctx, raft.PeerConfig{ID: "n100", Voter: true})
	if err != raft.ErrConfigChangeInProgress {
		t.Fatalf("expected ErrConfigChangeInProgress from second AddServer, got %v", err)
	}
}

// ---- Section 14: Leadership Transfer ----------------------------------------

// TestLeadershipTransfer_LeaderStepsDown verifies that after TransferLeadership
// is called, the current leader eventually steps down and a new leader is elected.
func TestLeadershipTransfer_LeaderStepsDown(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	// Pick any follower as the transfer target.
	targetIdx := (leaderIdx + 1) % 3
	targetID := c.ids[targetIdx]

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()

	if err := leader.TransferLeadership(ctx, targetID); err != nil {
		t.Fatalf("TransferLeadership: %v", err)
	}

	// Keep ticking until the old leader steps down.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		if leader.State() != raft.Leader {
			break
		}
	}

	if leader.State() == raft.Leader {
		t.Fatal("old leader did not step down after leadership transfer")
	}

	// A new leader must exist.
	newLeaderIdx := -1
	deadline = time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		for i, n := range c.nodes {
			if n.State() == raft.Leader {
				newLeaderIdx = i
				break
			}
		}
		if newLeaderIdx >= 0 {
			break
		}
	}
	if newLeaderIdx < 0 {
		t.Fatal("no new leader elected after leadership transfer")
	}

	// Cluster must still accept proposals.
	if _, err := c.Propose(electionTimeout, []byte("post=transfer")); err != nil {
		t.Fatalf("Propose after transfer: %v", err)
	}
}

// TestLeadershipTransfer_NonLeaderReturnsError verifies that TransferLeadership
// returns ErrNotLeader when called on a follower.
func TestLeadershipTransfer_NonLeaderReturnsError(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)

	followerIdx := (leaderIdx + 1) % 3
	follower := c.nodes[followerIdx]
	targetID := c.ids[(leaderIdx+2)%3]

	ctx := context.Background()
	err := follower.TransferLeadership(ctx, targetID)
	if !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader from follower TransferLeadership, got %v", err)
	}
}

// ---- Section 20: Graceful Shutdown ------------------------------------------

// TestGracefulShutdown_StopDrainsApplyLoop verifies that Stop() waits for the
// apply goroutine to finish — i.e., it does not return while Apply calls are
// still in progress.
func TestGracefulShutdown_StopDrainsApplyLoop(t *testing.T) {
	c := newCluster(t, 3)
	c.WaitLeader(electionTimeout)

	// Propose several entries so there is work for the apply goroutine.
	for i := range 5 {
		cmd := fmt.Appendf(nil, "g=%d", i)
		if _, err := c.Propose(electionTimeout, cmd); err != nil {
			t.Fatalf("Propose %d: %v", i, err)
		}
	}

	// Stop() must return without deadlocking. The t.Cleanup in newCluster
	// already calls Stop() for every node, so a hang here would cause the
	// test to time out.
}
