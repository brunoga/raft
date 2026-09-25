package raft

import "testing"

// staleLeader builds a leader at term 1 with entries 1..last, one voter peer
// "a", and the event loop never started, so the test owns all of its state.
func staleLeader(t *testing.T, last Index) *Node {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ID = "self"
	cfg.Peers = []PeerConfig{{ID: "a", Voter: true}, {ID: "b", Voter: true}}
	cfg.Storage = &memLogStorage{}
	cfg.StateMachine = &stubStateMachine{}
	cfg.Transport = &stubTransport{}
	cfg.TickInterval = 0
	node, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(node.Stop)
	entries := make([]LogEntry, 0, last)
	for i := Index(1); i <= last; i++ {
		entries = append(entries, LogEntry{Index: i, Term: 1, Command: []byte("x")})
	}
	node.log.append(entries)
	node.flushWrites(t)
	node.currentTerm = 1
	node.setState(Leader)
	node.termStartIndex = 1
	node.nextIndex = make(map[NodeID]Index)
	node.matchIndex = make(map[NodeID]Index)
	node.inflight = make(map[NodeID]int)
	node.snapshotInflight = make(map[NodeID]bool)
	return node
}

// TestAppendResult_StaleRejectionCannotRegressBelowMatch pins the invariant
// nextIndex > matchIndex. A follower that has acknowledged index 8 cannot
// lack index 7, so a rejection saying it does is one that was produced
// before that acknowledgement and delivered after. Honouring it drops
// nextIndex below matchIndex, and nothing then raises it again: a
// successful append whose last entry is not past matchIndex leaves
// nextIndex alone, and the pipeline re-sends the same entries with no
// timer between rounds, which is a leader spinning a core forever.
func TestAppendResult_StaleRejectionCannotRegressBelowMatch(t *testing.T) {
	node := staleLeader(t, 8)
	node.matchIndex["a"] = 8
	node.nextIndex["a"] = 9

	node.handleAppendResult(&appendResult{
		peer: "a", term: 1, success: false,
		req:           &AppendEntriesRequest{Term: 1, PrevLogIndex: 8, PrevLogTerm: 1},
		conflictIndex: 7,
	})
	if got, match := node.nextIndex["a"], node.matchIndex["a"]; got <= match {
		t.Fatalf("a stale rejection moved nextIndex to %d, at or below matchIndex %d", got, match)
	}
}

// TestAppendResult_SuccessAlwaysAdvancesNextIndex is the other half: even if
// nextIndex has somehow fallen behind, a success for entries up to i must
// leave nextIndex at least i+1, or the pipeline re-sends them forever.
func TestAppendResult_SuccessAlwaysAdvancesNextIndex(t *testing.T) {
	node := staleLeader(t, 8)
	node.matchIndex["a"] = 8
	node.nextIndex["a"] = 7 // already regressed

	node.handleAppendResult(&appendResult{
		peer: "a", term: 1, success: true,
		req: &AppendEntriesRequest{Term: 1, PrevLogIndex: 6, PrevLogTerm: 1,
			Entries: []LogEntry{{Index: 7, Term: 1}, {Index: 8, Term: 1}}},
	})
	if got := node.nextIndex["a"]; got < 9 {
		t.Fatalf("after a success through index 8, nextIndex is %d, want at least 9", got)
	}
}
