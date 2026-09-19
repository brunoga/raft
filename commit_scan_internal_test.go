package raft

import (
	"context"
	"testing"
)

// countingLogStorage records how many times the commit path reads an entry
// back from storage.
type countingLogStorage struct {
	memLogStorage
	reads int
}

func (c *countingLogStorage) GetLogEntry(ctx context.Context, index Index) (LogEntry, error) {
	c.reads++
	return c.memLogStorage.GetLogEntry(ctx, index)
}

func (c *countingLogStorage) GetLogEntries(ctx context.Context, lo, hi Index) ([]LogEntry, error) {
	c.reads++
	return c.memLogStorage.GetLogEntries(ctx, lo, hi)
}

// TestMaybeAdvanceCommit_DoesNotReadTheLogFromStorage asserts that deciding
// whether the commit index can move costs no storage reads.
//
// The scan used to walk down from the last index asking storage for each
// entry's term, and when nothing was committable it walked all the way to the
// commit index — on every acknowledgement from every peer. On a leader with a
// backlog and a lagging follower that is a great many disk reads, all of them
// on the event loop, where they delay heartbeats and every inbound RPC.
//
// None of them are needed. A leader only ever appends its own entries, so
// everything at or above the first index of its term is in that term; anything
// below can never be committed by replica count, so there is nothing to test
// there.
func TestMaybeAdvanceCommit_DoesNotReadTheLogFromStorage(t *testing.T) {
	ctx := context.Background()
	store := &countingLogStorage{}

	cfg := DefaultConfig()
	cfg.ID = "self"
	// Four peers, so a quorum needs three of five and nothing this test does
	// can commit: the scan runs its full length every time.
	cfg.Peers = []PeerConfig{
		{ID: "a", Voter: true}, {ID: "b", Voter: true},
		{ID: "c", Voter: true}, {ID: "d", Voter: true},
	}
	cfg.Storage = store
	cfg.StateMachine = &stubStateMachine{}
	cfg.Transport = &stubTransport{}
	cfg.TickInterval = 0

	node, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Stand the node up as leader by hand: the event loop is never started, so
	// the test owns all of this state.
	const backlog = 500
	entries := make([]LogEntry, 0, backlog)
	for i := range backlog {
		entries = append(entries, LogEntry{Index: Index(i + 1), Term: 7, Command: []byte("x")})
	}
	if err := node.log.append(ctx, entries); err != nil {
		t.Fatalf("append: %v", err)
	}
	node.currentTerm = 7
	node.setState(Leader)
	node.termStartIndex = 1
	node.nextIndex = make(map[NodeID]Index)
	node.matchIndex = make(map[NodeID]Index)
	node.inflight = make(map[NodeID]int)
	node.snapshotInflight = make(map[NodeID]bool)

	store.reads = 0
	for range 20 {
		node.maybeAdvanceCommit()
	}

	if store.reads != 0 {
		t.Errorf("advancing the commit index read the log %d times over 20 attempts "+
			"across %d entries; it should read none", store.reads, backlog)
	}
	if node.commitIndex != 0 {
		t.Errorf("commitIndex = %d with only one of five members holding the entries, want 0",
			node.commitIndex)
	}
}

// TestMaybeAdvanceCommit_StillCommitsOnQuorum is the guard on the other side:
// bounding the scan must not stop it finding the highest committable index.
func TestMaybeAdvanceCommit_StillCommitsOnQuorum(t *testing.T) {
	ctx := context.Background()
	store := &countingLogStorage{}

	cfg := DefaultConfig()
	cfg.ID = "self"
	cfg.Peers = []PeerConfig{{ID: "a", Voter: true}, {ID: "b", Voter: true}}
	cfg.Storage = store
	cfg.StateMachine = &stubStateMachine{}
	cfg.Transport = &stubTransport{}
	cfg.TickInterval = 0

	node, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Entries 1 and 2 are from an earlier term; 3 to 6 are this leader's.
	entries := []LogEntry{
		{Index: 1, Term: 6, Command: []byte("old")},
		{Index: 2, Term: 6, Command: []byte("old")},
		{Index: 3, Term: 7, Command: nil},
		{Index: 4, Term: 7, Command: []byte("x")},
		{Index: 5, Term: 7, Command: []byte("x")},
		{Index: 6, Term: 7, Command: []byte("x")},
	}
	if err := node.log.append(ctx, entries); err != nil {
		t.Fatalf("append: %v", err)
	}
	node.currentTerm = 7
	node.setState(Leader)
	node.termStartIndex = 3
	node.nextIndex = map[NodeID]Index{"a": 7, "b": 7}
	node.matchIndex = map[NodeID]Index{"a": 5, "b": 2}
	node.inflight = make(map[NodeID]int)
	node.snapshotInflight = make(map[NodeID]bool)

	node.maybeAdvanceCommit()

	// Self and a hold index 5; b does not. Two of three is a majority.
	if node.commitIndex != 5 {
		t.Errorf("commitIndex = %d, want 5", node.commitIndex)
	}
}

// TestMaybeAdvanceCommit_WillNotCommitAnEarlierTermByCount pins the safety
// rule the bound relies on: an entry from a previous term is never committed
// on replica count alone, however widely it is replicated (Raft 5.4.2, the
// Figure 8 case).
func TestMaybeAdvanceCommit_WillNotCommitAnEarlierTermByCount(t *testing.T) {
	ctx := context.Background()
	store := &countingLogStorage{}

	cfg := DefaultConfig()
	cfg.ID = "self"
	cfg.Peers = []PeerConfig{{ID: "a", Voter: true}, {ID: "b", Voter: true}}
	cfg.Storage = store
	cfg.StateMachine = &stubStateMachine{}
	cfg.Transport = &stubTransport{}
	cfg.TickInterval = 0

	node, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	entries := []LogEntry{
		{Index: 1, Term: 6, Command: []byte("old")},
		{Index: 2, Term: 6, Command: []byte("old")},
		{Index: 3, Term: 7, Command: nil}, // this leader's no-op
	}
	if err := node.log.append(ctx, entries); err != nil {
		t.Fatalf("append: %v", err)
	}
	node.currentTerm = 7
	node.setState(Leader)
	node.termStartIndex = 3
	node.nextIndex = map[NodeID]Index{"a": 3, "b": 3}
	// Both peers hold the old entries; neither holds the no-op yet.
	node.matchIndex = map[NodeID]Index{"a": 2, "b": 2}
	node.inflight = make(map[NodeID]int)
	node.snapshotInflight = make(map[NodeID]bool)

	node.maybeAdvanceCommit()

	if node.commitIndex != 0 {
		t.Errorf("commitIndex = %d: an entry from an earlier term was committed on "+
			"replica count alone", node.commitIndex)
	}
}
