package raft_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

// recordingSM records every command handed to Apply so that tests can assert
// exactly which entries reached the state machine.
type recordingSM struct {
	mu      sync.Mutex
	applied []string
}

func (s *recordingSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, string(e.Command))
	return nil, nil
}

func (s *recordingSM) Snapshot(_ context.Context, _ io.Writer) error { return nil }

func (s *recordingSM) Restore(_ context.Context, _ raft.SnapshotMeta, _ io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = nil
	return nil
}

func (s *recordingSM) snapshotApplied() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.applied...)
}

// newFollowerWithLog builds a single follower node whose storage is pre-seeded
// with entries and a hard state, without starting an election. The node has two
// peers so that it is never a single-voter cluster.
func newFollowerWithLog(t *testing.T, entries []raft.LogEntry, term raft.Term) (*raft.Node, *recordingSM) {
	t.Helper()

	ctx := context.Background()
	store := memstore.New()
	if err := store.AppendLogEntries(ctx, entries); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	if err := store.SaveHardState(ctx, raft.HardState{CurrentTerm: term}); err != nil {
		t.Fatalf("seed hard state: %v", err)
	}

	sm := &recordingSM{}
	cfg := raft.DefaultConfig()
	cfg.ID = "f1"
	cfg.Peers = []raft.PeerConfig{{ID: "l1", Voter: true}, {ID: "f2", Voter: true}}
	cfg.Storage = store
	cfg.StateMachine = sm
	cfg.Transport = memtransport.NewNetwork().NewTransport("f1")
	cfg.TickInterval = 0 // never ticks: this node must stay a follower

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	return node, sm
}

// waitApplied blocks until the state machine has seen want entries or the
// deadline expires, then returns what it actually saw.
func waitAppliedCount(sm *recordingSM, want int, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for {
		got := sm.snapshotApplied()
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// staleSuffixLog is the log of a follower that accepted entries 3 and 4 from a
// term-2 leader that was deposed before those entries committed. Entries 1 and
// 2 are committed; 3 and 4 must never be applied, because the term-3 leader
// that replaced the term-2 leader has different entries at those indices.
func staleSuffixLog() []raft.LogEntry {
	return []raft.LogEntry{
		{Index: 1, Term: 1, Command: []byte("committed-1")},
		{Index: 2, Term: 1, Command: []byte("committed-2")},
		{Index: 3, Term: 2, Command: []byte("stale-3")},
		{Index: 4, Term: 2, Command: []byte("stale-4")},
	}
}

// TestAppendEntries_CommitIndexBoundedByRequestExtent asserts that a follower
// advances commitIndex only as far as the entries the request actually covers
// (PrevLogIndex + len(Entries)), never as far as its own last log index.
//
// A new leader backs nextIndex off to a matching prefix and sends a heartbeat
// carrying its own high LeaderCommit. If the follower clamped LeaderCommit to
// its own last index instead, it would commit and apply the uncommitted suffix
// it still holds from the previous leader — entries the new leader is about to
// overwrite. That is a State Machine Safety violation: the entries applied at
// indices 3 and 4 would differ from the ones eventually committed there.
func TestAppendEntries_CommitIndexBoundedByRequestExtent(t *testing.T) {
	node, sm := newFollowerWithLog(t, staleSuffixLog(), 2)

	// The term-3 leader matches this follower only up to index 2, but its own
	// commitIndex is 4 (it committed its own entries 3 and 4).
	resp, err := node.Handler().HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
		Term:         3,
		LeaderID:     "l1",
		PrevLogIndex: 2,
		PrevLogTerm:  1,
		LeaderCommit: 4,
	})
	if err != nil {
		t.Fatalf("HandleAppendEntries: %v", err)
	}
	if !resp.Success {
		t.Fatalf("heartbeat rejected: %+v", resp)
	}

	applied := waitAppliedCount(sm, 3, 200*time.Millisecond)

	if got := node.CommitIndex(); got != 2 {
		t.Errorf("commitIndex = %d, want 2 (the request only covers up to index 2)", got)
	}
	for _, cmd := range applied {
		if cmd == "stale-3" || cmd == "stale-4" {
			t.Errorf("applied uncommitted entry %q from a deposed leader; applied=%v", cmd, applied)
		}
	}
}

// TestAppendEntries_CommitIndexAdvancesWithEntries is the companion to the test
// above: when the request does carry the entries, the follower must commit up
// to the last one the leader considers committed.
func TestAppendEntries_CommitIndexAdvancesWithEntries(t *testing.T) {
	node, sm := newFollowerWithLog(t, staleSuffixLog(), 2)

	// The term-3 leader overwrites indices 3 and 4 with its own entries and
	// reports them committed.
	resp, err := node.Handler().HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
		Term:         3,
		LeaderID:     "l1",
		PrevLogIndex: 2,
		PrevLogTerm:  1,
		Entries: []raft.LogEntry{
			{Index: 3, Term: 3, Command: []byte("real-3")},
			{Index: 4, Term: 3, Command: []byte("real-4")},
		},
		LeaderCommit: 4,
	})
	if err != nil {
		t.Fatalf("HandleAppendEntries: %v", err)
	}
	if !resp.Success {
		t.Fatalf("append rejected: %+v", resp)
	}

	applied := waitAppliedCount(sm, 4, 2*time.Second)

	if got := node.CommitIndex(); got != 4 {
		t.Errorf("commitIndex = %d, want 4", got)
	}
	want := []string{"committed-1", "committed-2", "real-3", "real-4"}
	if len(applied) != len(want) {
		t.Fatalf("applied %v, want %v", applied, want)
	}
	for i := range want {
		if applied[i] != want[i] {
			t.Fatalf("applied %v, want %v", applied, want)
		}
	}
}

// TestAppendEntries_CommitIndexNeverRegresses asserts that a late or reordered
// request carrying a lower LeaderCommit cannot move commitIndex backwards.
func TestAppendEntries_CommitIndexNeverRegresses(t *testing.T) {
	node, _ := newFollowerWithLog(t, staleSuffixLog(), 2)
	ctx := context.Background()

	if _, err := node.Handler().HandleAppendEntries(ctx, &raft.AppendEntriesRequest{
		Term:         3,
		LeaderID:     "l1",
		PrevLogIndex: 2,
		PrevLogTerm:  1,
		Entries:      []raft.LogEntry{{Index: 3, Term: 3, Command: []byte("real-3")}},
		LeaderCommit: 3,
	}); err != nil {
		t.Fatalf("HandleAppendEntries: %v", err)
	}
	if got := node.CommitIndex(); got != 3 {
		t.Fatalf("commitIndex = %d, want 3", got)
	}

	// A stale heartbeat from the same term reports an older commit index.
	if _, err := node.Handler().HandleAppendEntries(ctx, &raft.AppendEntriesRequest{
		Term:         3,
		LeaderID:     "l1",
		PrevLogIndex: 1,
		PrevLogTerm:  1,
		LeaderCommit: 1,
	}); err != nil {
		t.Fatalf("HandleAppendEntries: %v", err)
	}
	if got := node.CommitIndex(); got != 3 {
		t.Errorf("commitIndex regressed to %d, want it to stay at 3", got)
	}
}
