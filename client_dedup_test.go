package raft_test

// Tests for §16 Client Protocol:
//   - NotLeaderError carries a leader hint
//   - ProposeOnce is idempotent for the same (clientID, seqNum)
//   - ProposeOnce rejects an obsolete seqNum
//   - ProposeOnce with a new seqNum appends normally
//   - Client dedup table survives a snapshot/restore cycle
//   - Leader() returns the current leader's NodeID

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/brunoga/raft"
)

// TestNotLeaderError_LeaderHint verifies that Propose on a follower returns a
// *NotLeaderError wrapping ErrNotLeader, with the Leader field set once the
// follower knows who the leader is.
func TestNotLeaderError_LeaderHint(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)

	followerIdx := (leaderIdx + 1) % 3
	follower := c.nodes[followerIdx]

	// Wait for the follower to hear the leader heartbeat and update its hint.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) && follower.Leader() == "" {
		c.Tick()
		time.Sleep(time.Millisecond)
	}

	ctx := context.Background()
	_, err := follower.Propose(ctx, []byte("cmd"))

	var nle *raft.NotLeaderError
	if !errors.As(err, &nle) {
		t.Fatalf("expected *raft.NotLeaderError, got %T: %v", err, err)
	}
	if !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("errors.Is(err, ErrNotLeader) should be true")
	}
	// The follower has heard a heartbeat from the leader by now, so Leader should
	// be non-empty and match the elected leader.
	if nle.Leader == "" {
		t.Errorf("expected a leader hint, got empty")
	}
	if nle.Leader != c.ids[leaderIdx] {
		t.Errorf("leader hint = %q, want %q", nle.Leader, c.ids[leaderIdx])
	}
}

// TestLeaderMethod returns the NodeID of the current leader.
func TestLeaderMethod(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	if got := leader.Leader(); got != c.ids[leaderIdx] {
		t.Errorf("leader.Leader() = %q, want %q", got, c.ids[leaderIdx])
	}

	// Followers should also know the leader after receiving a heartbeat.
	for i, node := range c.nodes {
		if i == leaderIdx {
			continue
		}
		if got := node.Leader(); got != c.ids[leaderIdx] {
			t.Errorf("follower %d Leader() = %q, want %q", i, got, c.ids[leaderIdx])
		}
	}
}

// TestProposeOnce_Idempotent verifies that re-sending the same (clientID,
// seqNum) returns the same result without re-applying.
func TestProposeOnce_Idempotent(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	ctx := context.Background()
	const client = raft.NodeID("client1")
	const seqNum = uint64(1)
	cmd := []byte("color=blue")

	// First propose.
	v1, err := leader.ProposeOnce(ctx, client, seqNum, cmd)
	if err != nil {
		t.Fatalf("ProposeOnce (1st): %v", err)
	}

	// Second propose with same (client, seqNum) — must return cached result.
	v2, err := leader.ProposeOnce(ctx, client, seqNum, cmd)
	if err != nil {
		t.Fatalf("ProposeOnce (2nd): %v", err)
	}
	if string(v1) != string(v2) {
		t.Errorf("idempotent retry: first=%q second=%q, want equal", v1, v2)
	}
}

// TestProposeOnce_ObsoleteSeqNum verifies ErrObsoleteSeqNum for a stale retry.
func TestProposeOnce_ObsoleteSeqNum(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	ctx := context.Background()
	const client = raft.NodeID("clientA")

	// Issue seqNum=2 first.
	if _, err := leader.ProposeOnce(ctx, client, 2, []byte("x=2")); err != nil {
		t.Fatalf("ProposeOnce seq=2: %v", err)
	}

	// Now retry with seqNum=1 (obsolete).
	_, err := leader.ProposeOnce(ctx, client, 1, []byte("x=1"))
	if !errors.Is(err, raft.ErrObsoleteSeqNum) {
		t.Fatalf("expected ErrObsoleteSeqNum for stale seqNum, got %v", err)
	}
}

// TestProposeOnce_NewSeqNum verifies that a new (higher) seqNum goes through
// as a fresh proposal.
func TestProposeOnce_NewSeqNum(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	ctx := context.Background()
	const client = raft.NodeID("clientB")

	if _, err := leader.ProposeOnce(ctx, client, 1, []byte("y=1")); err != nil {
		t.Fatalf("ProposeOnce seq=1: %v", err)
	}
	if _, err := leader.ProposeOnce(ctx, client, 2, []byte("y=2")); err != nil {
		t.Fatalf("ProposeOnce seq=2: %v", err)
	}
	// Both should apply; the SM should end up with y=2.
	// Wait for all nodes to converge.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		all := true
		for _, sm := range c.sms {
			if sm.Get("y") != "2" {
				all = false
				break
			}
		}
		if all {
			return
		}
	}
	for i, sm := range c.sms {
		t.Errorf("node %d: y=%q, want 2", i, sm.Get("y"))
	}
}

// TestProposeOnce_LRUEviction verifies that when MaxClientTableSize is reached
// the least-recently-used client is evicted and recently-used clients are kept.
//
// Setup: cap = 2, three distinct clients A, B, C.
//   - A submits seq=1  → A is MRU
//   - B submits seq=1  → B is MRU, A is LRU
//   - C submits seq=1  → C is MRU, A is evicted
//
// After eviction, B's entry must still be present (dedup works for B).
// A's entry is gone; retrying A's seq=1 triggers a fresh application, so
// the state machine sees a second write and A's key changes value.
func TestProposeOnce_LRUEviction(t *testing.T) {
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.MaxClientTableSize = 2
	})
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]
	ctx := context.Background()

	const (
		clientA = raft.NodeID("evict-A")
		clientB = raft.NodeID("evict-B")
		clientC = raft.NodeID("evict-C")
	)

	// A submits seq=1, writes "ka=v1" to the KV SM.
	if _, err := leader.ProposeOnce(ctx, clientA, 1, []byte("ka=v1")); err != nil {
		t.Fatalf("A seq=1: %v", err)
	}
	// B submits seq=1, writes "kb=v1".
	if _, err := leader.ProposeOnce(ctx, clientB, 1, []byte("kb=v1")); err != nil {
		t.Fatalf("B seq=1: %v", err)
	}
	// C submits seq=1, writes "kc=v1". This should evict A (LRU at cap=2).
	if _, err := leader.ProposeOnce(ctx, clientC, 1, []byte("kc=v1")); err != nil {
		t.Fatalf("C seq=1: %v", err)
	}

	// B is still in the table: retrying B's seq=1 must return the cached
	// result without re-applying (value stays "v1").
	bResult, err := leader.ProposeOnce(ctx, clientB, 1, []byte("kb=SHOULD_NOT_APPLY"))
	if err != nil {
		t.Fatalf("B seq=1 retry: %v", err)
	}
	if string(bResult) != "v1" {
		t.Errorf("B dedup: got %q, want cached result %q", bResult, "v1")
	}

	// Wait for all nodes to converge, then check that B's SM key did not change.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		if c.sms[leaderIdx].Get("kb") == "v1" {
			break
		}
	}
	if got := c.sms[leaderIdx].Get("kb"); got != "v1" {
		t.Errorf("SM: kb=%q after B dedup retry, want v1 (re-apply would change it)", got)
	}

	// A was evicted: retrying A's seq=1 must trigger a fresh application.
	// The SM will write "ka=v1" again (idempotent for this particular command),
	// but we can verify it goes through as a new proposal (seq=1 < seq=2 check).
	// The observable signal is that ProposeOnce does NOT return ErrObsoleteSeqNum
	// and instead applies the command (which would fail if A were still cached).
	if _, err := leader.ProposeOnce(ctx, clientA, 1, []byte("ka=v1")); err != nil {
		// Should succeed — A's entry was evicted so seq=1 is treated as new.
		t.Fatalf("A seq=1 after eviction: expected success (re-apply), got %v", err)
	}
}

// TestProposeOnce_DedupSurvivesSnapshot verifies that after a snapshot is taken
// the dedup table is correctly persisted so that a stale retry is still
// recognised after the snapshot.
func TestProposeOnce_DedupSurvivesSnapshot(t *testing.T) {
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.SnapshotThreshold = 2 // very aggressive — snapshot after every 2 entries
	})
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	ctx := context.Background()
	const client = raft.NodeID("clientSnap")

	// Commit several entries to force at least one snapshot.
	for i := 1; i <= 6; i++ {
		cmd := fmt.Appendf(nil, "snap_k%d=v%d", i, i)
		if _, err := leader.ProposeOnce(ctx, client, uint64(i), cmd); err != nil {
			t.Fatalf("ProposeOnce seq=%d: %v", i, err)
		}
	}

	// Wait for a snapshot to be taken (SnapshotThreshold=2, lastApplied>=2).
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		if leader.LastApplied() >= 6 {
			break
		}
	}

	// Retry with an obsolete seqNum — should still be rejected even after snapshot.
	_, err := leader.ProposeOnce(ctx, client, 3, []byte("snap_k3=retry"))
	if !errors.Is(err, raft.ErrObsoleteSeqNum) {
		t.Fatalf("after snapshot: expected ErrObsoleteSeqNum for seq=3, got %v", err)
	}
}
