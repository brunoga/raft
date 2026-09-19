package raft

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// These tests are about the bookkeeping that makes an asynchronous log write
// safe, which is almost entirely one question: which indices does storage hold
// the log's current entry for?
//
// It is not the same as "which indices has a write completed for". A queued
// truncation takes effect in the log the moment it is queued and on disk only
// when the writer gets to it, so between those two moments storage holds
// entries this log has already replaced. Answering with the completed writes
// alone would let a follower acknowledge entries it no longer has, and let the
// apply loop read back a history the node had abandoned.

// pausableStore is an in-memory log store whose writes can be held open and
// then released one at a time, so a test can observe the log at each point
// between queueing a write and its landing.
type pausableStore struct {
	stubStorage

	mu      sync.Mutex
	entries []LogEntry
	permits chan struct{} // non-nil while writes are being held
}

// hold makes every subsequent write wait to be let through.
func (s *pausableStore) hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.permits = make(chan struct{})
}

// allow lets exactly n held writes through, blocking until each one has
// arrived. That is what makes these tests deterministic: the state being
// asserted on is the state after a known number of writes, not after however
// many happened to finish.
func (s *pausableStore) allow(t *testing.T, n int) {
	t.Helper()
	s.mu.Lock()
	permits := s.permits
	s.mu.Unlock()
	for range n {
		select {
		case permits <- struct{}{}:
		case <-time.After(10 * time.Second):
			t.Fatal("no storage write was waiting to be let through")
		}
	}
}

// release lets every held write through, and stops holding new ones.
func (s *pausableStore) release() {
	s.mu.Lock()
	permits := s.permits
	s.permits = nil
	s.mu.Unlock()
	if permits != nil {
		close(permits)
	}
}

func (s *pausableStore) wait() {
	s.mu.Lock()
	permits := s.permits
	s.mu.Unlock()
	if permits == nil {
		return
	}
	<-permits // a permit, or the channel closing
}

func (s *pausableStore) AppendLogEntries(_ context.Context, entries []LogEntry) error {
	s.wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entries...)
	return nil
}

func (s *pausableStore) TruncateSuffix(_ context.Context, from Index) error {
	s.wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = slices.DeleteFunc(s.entries, func(e LogEntry) bool { return e.Index >= from })
	return nil
}

func (s *pausableStore) TruncatePrefix(_ context.Context, to Index) error {
	s.wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = slices.DeleteFunc(s.entries, func(e LogEntry) bool { return e.Index < to })
	return nil
}

func (s *pausableStore) GetLogEntry(_ context.Context, index Index) (LogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.Index == index {
			return e, nil
		}
	}
	return LogEntry{}, ErrNotFound
}

func (s *pausableStore) GetLogEntries(_ context.Context, lo, hi Index) ([]LogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LogEntry
	for _, e := range s.entries {
		if e.Index >= lo && e.Index < hi {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *pausableStore) FirstIndex() (Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return 0, nil
	}
	return s.entries[0].Index, nil
}

func (s *pausableStore) LastIndex() (Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return 0, nil
	}
	return s.entries[len(s.entries)-1].Index, nil
}

// indices returns the log indices storage actually holds.
func (s *pausableStore) indices() []Index {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Index, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.Index)
	}
	return out
}

// newTestLog builds a raftLog over a pausable store.
func newTestLog(t *testing.T) (*raftLog, *pausableStore, *storageWriter) {
	t.Helper()
	store := &pausableStore{}
	w := newStorageWriter(store)
	t.Cleanup(w.close)
	rl, err := newRaftLog(store, w)
	if err != nil {
		t.Fatalf("newRaftLog: %v", err)
	}
	return rl, store, w
}

// settle waits for the writer to finish and applies every completion, standing
// in for the event loop.
func settle(t *testing.T, rl *raftLog, w *storageWriter) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, d := range w.takeDone() {
			if d.err != nil {
				t.Fatalf("storage write failed: %v", d.err)
			}
			rl.stabilize(d)
		}
		if !w.busy() && len(rl.segs) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("storage writes did not drain")
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// awaitWrite waits for exactly one completion and applies it.
func awaitWrite(t *testing.T, rl *raftLog, w *storageWriter) writeDone {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		done := w.takeDone()
		switch len(done) {
		case 0:
			if time.Now().After(deadline) {
				t.Fatal("no storage write completed")
			}
			time.Sleep(200 * time.Microsecond)
		case 1:
			if done[0].err != nil {
				t.Fatalf("storage write failed: %v", done[0].err)
			}
			rl.stabilize(done[0])
			return done[0]
		default:
			t.Fatalf("%d writes completed at once; the test needs them one at a time", len(done))
		}
	}
}

func entriesAt(term Term, from Index, count int) []LogEntry {
	out := make([]LogEntry, 0, count)
	for i := range count {
		out = append(out, LogEntry{Index: from + Index(i), Term: term, Command: []byte("cmd")})
	}
	return out
}

// TestRaftLog_AnAppendIsInTheLogBeforeItIsOnDisk pins the two halves of the
// contract at once: the entries count for everything the node decides now, and
// for nothing it promises about its own survival.
func TestRaftLog_AnAppendIsInTheLogBeforeItIsOnDisk(t *testing.T) {
	rl, store, w := newTestLog(t)

	store.hold()
	rl.append(entriesAt(1, 1, 3))

	if got := rl.lastLogIndex(); got != 3 {
		t.Errorf("lastLogIndex = %d, want 3: the entries are in the log immediately", got)
	}
	if got := rl.lastLogTerm(); got != 1 {
		t.Errorf("lastLogTerm = %d, want 1", got)
	}
	if rl.isUpToDate(2, 1) {
		t.Error("a candidate whose log ends at index 2 was judged up to date against a log ending at 3")
	}
	if got := rl.stableIndex(); got != 0 {
		t.Errorf("stableIndex = %d, want 0: nothing has been written yet", got)
	}

	store.release()
	settle(t, rl, w)

	if got := rl.stableIndex(); got != 3 {
		t.Errorf("stableIndex = %d after the write completed, want 3", got)
	}
}

// TestRaftLog_ReplacedEntriesAreNotStableBecauseTheOldOnesWere is the case the
// whole of stableIndex exists for, and the one a simpler implementation gets
// wrong.
//
// A follower takes entries from one leader, then takes conflicting entries
// from the next and queues a truncation followed by a replacement append. The
// first write completes: storage now holds the *old* entries at those indices,
// and reporting them as stable would have the follower acknowledge, and the
// apply loop read, entries belonging to a history it has already discarded.
func TestRaftLog_ReplacedEntriesAreNotStableBecauseTheOldOnesWere(t *testing.T) {
	rl, store, w := newTestLog(t)

	store.hold()
	rl.append(entriesAt(1, 1, 8)) // from the first leader

	// The second leader replaces everything from index 5.
	if err := rl.truncateSuffix(context.Background(), 5); err != nil {
		t.Fatalf("truncateSuffix: %v", err)
	}
	rl.append(entriesAt(2, 5, 2))

	if got := rl.lastLogIndex(); got != 6 {
		t.Fatalf("lastLogIndex = %d, want 6", got)
	}

	// Let only the first append through: storage now holds 1..8, where 5..8
	// are the discarded entries, and neither the truncation nor its
	// replacement has run.
	store.allow(t, 1)
	first := awaitWrite(t, rl, w)

	if got := store.indices(); len(got) != 8 {
		t.Fatalf("storage holds %v; the test needs only the first append to have run", got)
	}
	if first.durableAfter != 8 {
		t.Fatalf("the first write reported durableAfter = %d, want 8", first.durableAfter)
	}
	if got := rl.stableIndex(); got > 4 {
		t.Errorf("stableIndex = %d while indices 5 to 8 on disk still hold the discarded entries; want at most 4", got)
	}

	store.release()
	settle(t, rl, w)

	if got := rl.stableIndex(); got != 6 {
		t.Errorf("stableIndex = %d once every write landed, want 6", got)
	}
	if got := store.indices(); !slices.Equal(got, []Index{1, 2, 3, 4, 5, 6}) {
		t.Errorf("storage holds %v, want 1..6", got)
	}
	// And the entries at the replaced indices are the second leader's.
	term, err := rl.termAt(context.Background(), 5)
	if err != nil {
		t.Fatalf("termAt(5): %v", err)
	}
	if term != 2 {
		t.Errorf("term at index 5 = %d, want 2", term)
	}
}

// TestRaftLog_ReadsSeeEntriesThatAreOnlyInMemory pins that an entry is
// readable the moment it is appended. A leader replicates the entries it has
// just accepted, which are precisely the ones least likely to be on disk; a
// read path that went to storage would send nothing and the cluster would
// advance only as fast as the leader's disk.
func TestRaftLog_ReadsSeeEntriesThatAreOnlyInMemory(t *testing.T) {
	rl, store, w := newTestLog(t)

	store.hold()
	defer store.release()
	rl.append(entriesAt(4, 1, 5))

	got, err := rl.entries(context.Background(), 2, 5)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(got) != 3 || got[0].Index != 2 || got[2].Index != 4 {
		t.Fatalf("entries(2,5) returned %d entries starting at %v, want 2..4", len(got), got)
	}
	for _, e := range got {
		if e.Term != 4 {
			t.Errorf("entry %d has term %d, want 4", e.Index, e.Term)
		}
	}

	term, err := rl.termAt(context.Background(), 3)
	if err != nil {
		t.Fatalf("termAt(3): %v", err)
	}
	if term != 4 {
		t.Errorf("termAt(3) = %d, want 4", term)
	}

	store.release()
	settle(t, rl, w)
}

// TestRaftLog_ReadsSpanBothHalvesOfTheLog covers a range that starts on disk
// and ends in memory, which is the shape of every read a leader does while it
// is being written to.
func TestRaftLog_ReadsSpanBothHalvesOfTheLog(t *testing.T) {
	rl, store, w := newTestLog(t)

	rl.append(entriesAt(1, 1, 4))
	settle(t, rl, w)

	store.hold()
	defer store.release()
	rl.append(entriesAt(2, 5, 4))

	got, err := rl.entries(context.Background(), 3, 7)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	want := []Index{3, 4, 5, 6}
	if len(got) != len(want) {
		t.Fatalf("entries(3,7) returned %v, want indices %v", got, want)
	}
	for i := range want {
		if got[i].Index != want[i] {
			t.Fatalf("entries(3,7) returned index %d at position %d, want %d", got[i].Index, i, want[i])
		}
	}
	if got[1].Term != 1 || got[2].Term != 2 {
		t.Errorf("the boundary entries have terms %d and %d, want 1 and 2", got[1].Term, got[2].Term)
	}
}

// TestRaftLog_ReadsRefuseIndicesTheLogHasDiscarded pins the guard that makes
// the rest of it safe.
//
// After a truncation is queued, storage still holds the discarded entries
// until the writer gets to them. Every read is bounded by what the log says it
// has rather than by what storage will hand over, so those entries cannot come
// back through a read in the meantime.
func TestRaftLog_ReadsRefuseIndicesTheLogHasDiscarded(t *testing.T) {
	rl, store, w := newTestLog(t)

	rl.append(entriesAt(1, 1, 6))
	settle(t, rl, w)

	store.hold()
	defer store.release()

	if err := rl.truncateSuffix(context.Background(), 4); err != nil {
		t.Fatalf("truncateSuffix: %v", err)
	}

	// Storage has not been touched yet.
	if got := store.indices(); len(got) != 6 {
		t.Fatalf("storage holds %v; the test needs the truncation to still be pending", got)
	}

	if _, err := rl.termAt(context.Background(), 5); err == nil {
		t.Error("termAt returned a discarded entry that storage had not removed yet")
	}
	if got, err := rl.entries(context.Background(), 4, 7); err == nil && len(got) != 0 {
		t.Errorf("entries returned %d discarded entries that storage had not removed yet", len(got))
	}
	if rl.canDescribe(5) {
		t.Error("canDescribe said it could describe a discarded entry")
	}
	if got := rl.lastLogIndex(); got != 3 {
		t.Errorf("lastLogIndex = %d, want 3", got)
	}
}

// TestRaftLog_UnstableSizeTracksWhatIsHeldInMemory pins the number a leader
// throttles itself against. Overstating it would refuse proposals a healthy
// node could take; understating it would let the backlog grow without limit,
// which is the failure the limit exists to prevent.
func TestRaftLog_UnstableSizeTracksWhatIsHeldInMemory(t *testing.T) {
	rl, store, w := newTestLog(t)

	if got := rl.unstableSize(); got != 0 {
		t.Fatalf("unstableSize on a fresh log = %d, want 0", got)
	}

	store.hold()
	rl.append(entriesAt(1, 1, 4)) // 4 entries of 3 bytes each
	if got := rl.unstableSize(); got != 12 {
		t.Errorf("unstableSize = %d after appending 4 commands of 3 bytes, want 12", got)
	}

	if err := rl.truncateSuffix(context.Background(), 3); err != nil {
		t.Fatalf("truncateSuffix: %v", err)
	}
	if got := rl.unstableSize(); got != 6 {
		t.Errorf("unstableSize = %d after discarding 2 of the 4, want 6", got)
	}

	store.release()
	settle(t, rl, w)

	if got := rl.unstableSize(); got != 0 {
		t.Errorf("unstableSize = %d once every write landed, want 0", got)
	}
}

// TestRaftLog_CompactionReleasesEntriesFromBothHalves pins that reclaiming a
// prefix reclaims it in memory too, so a node that compacts while writes are
// outstanding does not keep the compacted entries alive.
func TestRaftLog_CompactionReleasesEntriesFromBothHalves(t *testing.T) {
	rl, store, w := newTestLog(t)

	store.hold()
	rl.append(entriesAt(1, 1, 6))

	rl.truncatePrefix(4)
	if got := rl.unstableSize(); got != 9 {
		t.Errorf("unstableSize = %d after compacting away 3 of 6 entries, want 9", got)
	}
	if rl.first != 4 {
		t.Errorf("first = %d after compaction, want 4", rl.first)
	}
	if rl.canDescribe(2) {
		t.Error("canDescribe said it could describe a compacted entry")
	}

	store.release()
	settle(t, rl, w)

	if got := store.indices(); !slices.Equal(got, []Index{4, 5, 6}) {
		t.Errorf("storage holds %v, want 4..6", got)
	}
	if got := rl.stableIndex(); got != 6 {
		t.Errorf("stableIndex = %d, want 6", got)
	}
}
