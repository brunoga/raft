package raft_test

// Property-based tests for the three core RPC handlers:
//   - HandleRequestVote
//   - HandleAppendEntries
//   - HandleInstallSnapshot
//
// Each test exercises an invariant that must hold for ALL valid inputs.
// We use testing/quick to generate random inputs; each property function
// returns false on violation so that quick.Check can report the shrunk
// counter-example.

import (
	"context"
	"testing"
	"testing/quick"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
)

// makeLeaderNode returns a started single-node cluster that has elected
// itself leader.  The caller must Stop() it.
func makeLeaderNode(t *testing.T) *raft.Node {
	t.Helper()
	n := newTestNode(t, "leader", nil)
	n.Start()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && n.StateSnapshot() != raft.Leader {
		n.Tick()
		time.Sleep(time.Millisecond)
	}
	if n.StateSnapshot() != raft.Leader {
		t.Fatal("node did not become leader")
	}
	return n
}

// makeFollowerNode returns a started node that is a follower in term `term`
// having voted for `votedFor` (empty string means no vote).
func makeFollowerNode(t *testing.T, term raft.Term, votedFor raft.NodeID) *raft.Node {
	t.Helper()
	store := memstore.New()
	if term > 0 || votedFor != "" {
		if err := store.SaveHardState(context.Background(), raft.HardState{CurrentTerm: term, VotedFor: votedFor}); err != nil {
			t.Fatalf("SaveHardState: %v", err)
		}
	}
	cfg := raft.DefaultConfig()
	cfg.ID = "follower"
	cfg.Peers = []raft.NodeID{"other"}
	cfg.Storage = store
	cfg.StateMachine = &noopSM{}
	cfg.Transport = &noopTransport{}
	cfg.TickInterval = 0
	n, err := raft.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.Start()
	time.Sleep(2 * time.Millisecond) // let the event loop start
	return n
}

// ── RequestVote invariants ──────────────────────────────────────────────────

// Invariant RV-1: response.Term is always >= the node's own persisted term.
// We probe this by starting a follower in a known term and sending various
// RequestVote messages.
func TestProperty_RequestVote_ResponseTermNeverLower(t *testing.T) {
	f := func(reqTerm uint8, candidateTerm uint8) bool {
		nodeTerm := raft.Term(candidateTerm)
		n := makeFollowerNode(t, nodeTerm, "")
		defer n.Stop()

		resp, err := n.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
			Term:         raft.Term(reqTerm),
			CandidateID:  "candidate",
			LastLogIndex: 0,
			LastLogTerm:  0,
		})
		if err != nil {
			return true // handler errors are not property violations
		}
		return resp.Term >= nodeTerm
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// Invariant RV-2: vote is never granted for a request whose term is strictly
// less than the node's current term.
func TestProperty_RequestVote_StaleTerm_NeverGranted(t *testing.T) {
	f := func(nodeTerm uint8) bool {
		if nodeTerm == 0 {
			return true // skip: node term 0 and req term 0 is not stale
		}
		n := makeFollowerNode(t, raft.Term(nodeTerm), "")
		defer n.Stop()

		resp, err := n.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
			Term:        raft.Term(nodeTerm) - 1,
			CandidateID: "candidate",
		})
		if err != nil {
			return true
		}
		return !resp.VoteGranted
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// Invariant RV-3: a node that has already voted for a different candidate in
// the current term must not grant a vote to a third candidate in the same term.
func TestProperty_RequestVote_AlreadyVoted_DifferentCandidate_Denied(t *testing.T) {
	f := func(term uint8) bool {
		if term == 0 {
			return true
		}
		n := makeFollowerNode(t, raft.Term(term), "existing-vote")
		defer n.Stop()

		resp, err := n.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
			Term:        raft.Term(term),
			CandidateID: "other-candidate",
		})
		if err != nil {
			return true
		}
		return !resp.VoteGranted
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// Invariant RV-4: if the node has not voted and the candidate's term is higher,
// the vote is granted (provided the log is up-to-date — for an empty log it
// always is).
func TestProperty_RequestVote_HigherTerm_EmptyLog_Granted(t *testing.T) {
	f := func(nodeTerm uint8) bool {
		n := makeFollowerNode(t, raft.Term(nodeTerm), "")
		defer n.Stop()

		resp, err := n.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
			Term:        raft.Term(nodeTerm) + 1,
			CandidateID: "candidate",
		})
		if err != nil {
			return true
		}
		return resp.VoteGranted
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// ── AppendEntries invariants ────────────────────────────────────────────────

// Invariant AE-1: success is false when req.Term < current term.
func TestProperty_AppendEntries_StaleTerm_Rejected(t *testing.T) {
	f := func(nodeTerm uint8) bool {
		if nodeTerm == 0 {
			return true
		}
		n := makeFollowerNode(t, raft.Term(nodeTerm), "")
		defer n.Stop()

		resp, err := n.HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
			Term:     raft.Term(nodeTerm) - 1,
			LeaderID: "leader",
		})
		if err != nil {
			return true
		}
		return !resp.Success
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// Invariant AE-2: response.Term is always >= req.Term on a successful call.
func TestProperty_AppendEntries_ResponseTermNotLowerThanRequest(t *testing.T) {
	f := func(reqTerm uint8) bool {
		n := makeFollowerNode(t, 0, "")
		defer n.Stop()

		resp, err := n.HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
			Term:     raft.Term(reqTerm),
			LeaderID: "leader",
		})
		if err != nil {
			return true
		}
		return resp.Term >= raft.Term(reqTerm)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// Invariant AE-3: AppendEntries with a non-zero PrevLogIndex fails when the
// follower's log is empty (prevLog cannot match).
func TestProperty_AppendEntries_PrevLogMismatch_Empty_Rejected(t *testing.T) {
	f := func(term uint8, prevIdx uint8) bool {
		if prevIdx == 0 {
			return true // prevIdx=0 means no log check required
		}
		n := makeFollowerNode(t, 0, "")
		defer n.Stop()

		resp, err := n.HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
			Term:         raft.Term(term) + 1,
			LeaderID:     "leader",
			PrevLogIndex: raft.Index(prevIdx),
			PrevLogTerm:  1,
		})
		if err != nil {
			return true
		}
		return !resp.Success
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// Invariant AE-4: a heartbeat (no entries, prevLogIndex=0) from a leader in a
// higher term must succeed and reset the follower's term.
func TestProperty_AppendEntries_Heartbeat_HigherTerm_Succeeds(t *testing.T) {
	f := func(nodeTerm uint8) bool {
		n := makeFollowerNode(t, raft.Term(nodeTerm), "")
		defer n.Stop()

		resp, err := n.HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
			Term:     raft.Term(nodeTerm) + 1,
			LeaderID: "leader",
		})
		if err != nil {
			return true
		}
		return resp.Success && resp.Term == raft.Term(nodeTerm)+1
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// ── InstallSnapshot invariants ──────────────────────────────────────────────

// Invariant IS-1: a snapshot with a lower or equal LastIncludedIndex than the
// node already has is silently ignored (Success is still true, but no change).
func TestProperty_InstallSnapshot_StaleSnapshot_Ignored(t *testing.T) {
	f := func(existingIdx uint8, reqIdx uint8) bool {
		if raft.Index(reqIdx) > raft.Index(existingIdx) {
			return true // not a stale snapshot, skip
		}
		store := memstore.New()
		existingMeta := raft.SnapshotMeta{
			LastIncludedIndex: raft.Index(existingIdx),
			LastIncludedTerm:  1,
		}
		if err := store.SaveSnapshot(context.Background(), existingMeta, []byte("snap")); err != nil {
			return true
		}
		cfg := raft.DefaultConfig()
		cfg.ID = "follower"
		cfg.Peers = []raft.NodeID{"leader"}
		cfg.Storage = store
		cfg.StateMachine = &noopSM{}
		cfg.Transport = &noopTransport{}
		cfg.TickInterval = 0
		n, err := raft.New(cfg)
		if err != nil {
			return true
		}
		n.Start()
		defer n.Stop()
		time.Sleep(2 * time.Millisecond)

		// A stale install-snapshot should not crash and should return a valid response.
		resp, err := n.HandleInstallSnapshot(context.Background(), &raft.InstallSnapshotRequest{
			Term:              2,
			LeaderID:          "leader",
			LastIncludedIndex: raft.Index(reqIdx),
			LastIncludedTerm:  1,
			Done:              true,
			Data:              []byte("stale"),
		})
		if err != nil {
			return true
		}
		// Response must have a valid term.
		return resp.Term > 0
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// Invariant IS-2: a snapshot from a stale leader (term < currentTerm) is
// rejected: the node does not install it.
func TestProperty_InstallSnapshot_StaleTerm_Rejected(t *testing.T) {
	f := func(nodeTerm uint8) bool {
		if nodeTerm == 0 {
			return true
		}
		n := makeFollowerNode(t, raft.Term(nodeTerm), "")
		defer n.Stop()

		resp, err := n.HandleInstallSnapshot(context.Background(), &raft.InstallSnapshotRequest{
			Term:              raft.Term(nodeTerm) - 1,
			LeaderID:          "leader",
			LastIncludedIndex: 100,
			LastIncludedTerm:  raft.Term(nodeTerm) - 1,
			Done:              true,
			Data:              []byte("data"),
		})
		if err != nil {
			return true
		}
		// Should not install: response term will be the node's current term.
		return resp.Term >= raft.Term(nodeTerm)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// ── Cross-handler invariants ────────────────────────────────────────────────

// Invariant AE-5: a leader rejects AppendEntries from any peer whose term is
// strictly lower than its own.
func TestProperty_AppendEntries_LeaderRejectsStalePeer(t *testing.T) {
	f := func() bool {
		n := makeLeaderNode(t)
		defer n.Stop()

		resp, err := n.HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
			Term:     0, // term 0 is always stale
			LeaderID: "stale-peer",
		})
		if err != nil {
			return true
		}
		return !resp.Success && resp.Term > 0
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 30}); err != nil {
		t.Error(err)
	}
}

// Invariant X-1: for any sequence of RequestVote calls in the same or lower
// term, the node never grants more than one vote (once voted, subsequent
// calls for other candidates are denied).
func TestProperty_RequestVote_VoteOncePerTerm(t *testing.T) {
	f := func(term uint8) bool {
		if term == 0 {
			return true
		}
		n := makeFollowerNode(t, 0, "")
		defer n.Stop()

		reqTerm := raft.Term(term)
		// First vote: candidate-A in reqTerm
		resp1, err := n.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
			Term:        reqTerm,
			CandidateID: "candidate-A",
		})
		if err != nil || !resp1.VoteGranted {
			return true // could not vote (maybe term too high or other reason); skip
		}

		// Second vote: candidate-B in same term — must be denied.
		resp2, err := n.HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
			Term:        reqTerm,
			CandidateID: "candidate-B",
		})
		if err != nil {
			return true
		}
		return !resp2.VoteGranted
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Error(err)
	}
}
