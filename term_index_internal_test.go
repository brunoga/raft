package raft

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
)

// These tests are about one number: how many times the event loop reads the
// log back from storage.
//
// It used to be a lot, and all of it on the loop. Every heartbeat looked up
// the term of the entry before the one it would send. Every inbound append
// checked the term at the index it started from. And when two logs disagreed,
// both sides walked backwards an index at a time, reading each entry, for as
// far as the disagreement went.
//
// That mattered more than it looks. Those reads take the same lock a
// file-backed store holds across its fsync, so a loop that no longer writes to
// the disk could still end up waiting for one -- which is the entire thing the
// asynchronous writer was built to avoid. Terms never decrease with index, so
// the whole log is a short sequence of runs, one per leadership epoch, and
// keeping those in memory answers every one of those questions for free.

// countingStore counts the reads made against it.
type countingStore struct {
	stubStorage

	mu      sync.Mutex
	entries []LogEntry
	reads   int
}

func (c *countingStore) AppendLogEntries(_ context.Context, entries []LogEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, entries...)
	return nil
}

func (c *countingStore) GetLogEntry(_ context.Context, index Index) (LogEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	for _, e := range c.entries {
		if e.Index == index {
			return e, nil
		}
	}
	return LogEntry{}, ErrNotFound
}

func (c *countingStore) GetLogEntries(_ context.Context, lo, hi Index) ([]LogEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	var out []LogEntry
	for _, e := range c.entries {
		if e.Index >= lo && e.Index < hi {
			out = append(out, e)
		}
	}
	return out, nil
}

func (c *countingStore) FirstIndex() (Index, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) == 0 {
		return 0, nil
	}
	return c.entries[0].Index, nil
}

func (c *countingStore) LastIndex() (Index, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) == 0 {
		return 0, nil
	}
	return c.entries[len(c.entries)-1].Index, nil
}

func (c *countingStore) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

func (c *countingStore) resetReads() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = 0
}

// termLog builds a log over a counting store, with runs of entries in the
// given terms: termLog(t, 3, 4, 3) gives three entries in term 1, four in
// term 2 and three in term 3.
func termLog(t *testing.T, lengths ...int) (*raftLog, *countingStore, *storageWriter) {
	t.Helper()

	store := &countingStore{}
	w := newStorageWriter(store)
	t.Cleanup(w.close)
	rl, err := newRaftLog(store, w)
	if err != nil {
		t.Fatalf("newRaftLog: %v", err)
	}

	idx := Index(1)
	for run, n := range lengths {
		entries := make([]LogEntry, 0, n)
		for range n {
			entries = append(entries, LogEntry{Index: idx, Term: Term(run + 1), Command: []byte("c")})
			idx++
		}
		rl.append(entries)
	}
	settle(t, rl, w)
	store.resetReads()
	return rl, store, w
}

// TestTermIndex_LookingUpATermReadsNothing is the property the whole structure
// exists for.
func TestTermIndex_LookingUpATermReadsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rl, store, _ := termLog(t, 200, 200, 200)

		for i := Index(1); i <= 600; i++ {
			got, err := rl.termAt(i)
			if err != nil {
				t.Fatalf("termAt(%d): %v", i, err)
			}
			want := Term((i-1)/200 + 1)
			if got != want {
				t.Fatalf("termAt(%d) = %d, want %d", i, got, want)
			}
		}

		if n := store.readCount(); n != 0 {
			t.Errorf("looking up 600 terms made %d storage reads, want 0", n)
		}
	})
}

// TestTermIndex_AConflictHintCostsNoReads covers the follower's side of a log
// disagreement.
//
// The hint the follower returns is where the term it disagreed in begins. It
// used to find that by walking back an index at a time, reading each entry, so
// the cost was the length of a term -- and the trigger is ordinary divergence
// after an election, which is when a cluster is already busy recovering.
func TestTermIndex_AConflictHintCostsNoReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rl, store, _ := termLog(t, 500, 500)

		// An index deep inside the second term. Its run begins at 501.
		if got := rl.termRunStart(900); got != 501 {
			t.Errorf("termRunStart(900) = %d, want 501", got)
		}
		if got := rl.termRunStart(300); got != 1 {
			t.Errorf("termRunStart(300) = %d, want 1", got)
		}
		if got := rl.termRunStart(1001); got != 0 {
			t.Errorf("termRunStart past the end of the log = %d, want 0", got)
		}
		if n := store.readCount(); n != 0 {
			t.Errorf("building conflict hints made %d storage reads, want 0", n)
		}
	})
}

// TestTermIndex_InterpretingAConflictHintCostsNoReads covers the leader's side.
//
// Given the term a follower disagreed in, the leader resumes just past its own
// last entry in that term. It used to find that by walking down from the end of
// its log reading each entry, so the cost was how far the follower had fallen
// behind.
func TestTermIndex_InterpretingAConflictHintCostsNoReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rl, store, _ := termLog(t, 100, 100, 100)

		cases := []struct {
			term  Term
			want  Index
			found bool
		}{
			{term: 1, want: 100, found: true},
			{term: 2, want: 200, found: true},
			{term: 3, want: 300, found: true},
			{term: 4, found: false},
		}
		for _, c := range cases {
			got, ok := rl.lastIndexOfTerm(c.term)
			if ok != c.found {
				t.Fatalf("lastIndexOfTerm(%d) found = %v, want %v", c.term, ok, c.found)
			}
			if ok && got != c.want {
				t.Errorf("lastIndexOfTerm(%d) = %d, want %d", c.term, got, c.want)
			}
		}
		if n := store.readCount(); n != 0 {
			t.Errorf("interpreting conflict hints made %d storage reads, want 0", n)
		}
	})
}

// TestTermIndex_FollowsTruncationAndCompaction pins that the index stays a
// faithful description of the log as the log changes underneath it. An index
// that drifted would be worse than no index at all: it answers the questions
// that decide where two logs diverge.
func TestTermIndex_FollowsTruncationAndCompaction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rl, _, w := termLog(t, 3, 3, 3) // 1..3 term 1, 4..6 term 2, 7..9 term 3

		// A new leader in term 5 overwrites from index 5.
		if err := rl.truncateSuffix(context.Background(), 5); err != nil {
			t.Fatalf("truncateSuffix: %v", err)
		}
		if got, err := rl.termAt(4); err != nil || got != 2 {
			t.Fatalf("termAt(4) = %d, %v; want 2, nil", got, err)
		}
		if _, err := rl.termAt(5); err == nil {
			t.Error("termAt returned a term for an index the log no longer holds")
		}
		if _, ok := rl.lastIndexOfTerm(3); ok {
			t.Error("the index still reports a term whose entries were all truncated")
		}

		rl.append([]LogEntry{
			{Index: 5, Term: 5, Command: []byte("c")},
			{Index: 6, Term: 5, Command: []byte("c")},
		})
		if got, ok := rl.lastIndexOfTerm(5); !ok || got != 6 {
			t.Errorf("lastIndexOfTerm(5) = %d, %v; want 6, true", got, ok)
		}
		if got := rl.termRunStart(6); got != 5 {
			t.Errorf("termRunStart(6) = %d, want 5", got)
		}

		// Compaction reclaims the first term and part of the second.
		rl.truncatePrefix(5)
		if _, err := rl.termAt(4); err == nil {
			t.Error("termAt returned a term for a compacted index")
		}
		if got, err := rl.termAt(5); err != nil || got != 5 {
			t.Fatalf("termAt(5) after compaction = %d, %v; want 5, nil", got, err)
		}
		if got, ok := rl.lastIndexOfTerm(1); ok {
			t.Errorf("lastIndexOfTerm(1) = %d after its entries were compacted away, want not found", got)
		}
		settle(t, rl, w)
	})
}

// TestTermIndex_SurvivesARestart pins that a node rebuilds the index from what
// is on disk. It is built once while the node is being constructed, which is
// what lets every later lookup be free.
func TestTermIndex_SurvivesARestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rl, store, w := termLog(t, 2, 3, 4)
		settle(t, rl, w)

		reopened, err := newRaftLog(store, newStorageWriter(store))
		if err != nil {
			t.Fatalf("newRaftLog on reopen: %v", err)
		}

		store.resetReads()
		for i := Index(1); i <= 9; i++ {
			want, _ := rl.termAt(i)
			got, err := reopened.termAt(i)
			if err != nil {
				t.Fatalf("termAt(%d) after reopen: %v", i, err)
			}
			if got != want {
				t.Errorf("termAt(%d) after reopen = %d, want %d", i, got, want)
			}
		}
		if got := reopened.lastLogTerm(); got != 3 {
			t.Errorf("lastLogTerm after reopen = %d, want 3", got)
		}
		if n := store.readCount(); n != 0 {
			t.Errorf("a reopened log made %d storage reads answering term lookups, want 0", n)
		}
	})
}

// TestTermIndex_StaysSmallHoweverLongTheLogGets pins the reason this is
// affordable. One run per leadership epoch, not one per entry: a log that
// never changes leader costs the same as a log with one entry.
func TestTermIndex_StaysSmallHoweverLongTheLogGets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rl, _, _ := termLog(t, 10_000)

		if got := len(rl.runs); got != 1 {
			t.Errorf("a log of 10,000 entries in one term needs %d runs, want 1", got)
		}
		if got, err := rl.termAt(9_999); err != nil || got != 1 {
			t.Fatalf("termAt(9999) = %d, %v; want 1, nil", got, err)
		}
	})
}

// TestTermIndex_AgreesWithTheEntriesThemselves is the safety net: the index is
// a claim about what the log holds, and the log itself is the authority.
func TestTermIndex_AgreesWithTheEntriesThemselves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rl, _, w := termLog(t, 7, 1, 4, 1, 9)
		settle(t, rl, w)

		got, err := rl.entries(context.Background(), rl.first, rl.last+1)
		if err != nil {
			t.Fatalf("entries: %v", err)
		}
		if len(got) != 22 {
			t.Fatalf("read %d entries, want 22", len(got))
		}
		for _, e := range got {
			indexed, err := rl.termAt(e.Index)
			if err != nil {
				t.Fatalf("termAt(%d): %v", e.Index, err)
			}
			if indexed != e.Term {
				t.Fatalf("the index says entry %d is in term %d; the entry says %d",
					e.Index, indexed, e.Term)
			}
		}
		if fmt.Sprint(len(rl.runs)) != "5" {
			t.Errorf("runs = %d, want 5", len(rl.runs))
		}
	})
}

// TestTermIndex_DescribesTheSnapshotBoundary covers the one index a log can
// still answer for when it holds no entry at all.
//
// A follower whose log has been compacted away entirely still knows the term
// at its snapshot boundary, and a leader disagreeing with it there needs a
// hint that points at that index. A hint of zero reaches the leader as "start
// again from the beginning", which makes it ship its whole state machine to a
// follower that needs almost none of it -- the outcome its own guard against
// hint-less rejections was written to avoid.
func TestTermIndex_DescribesTheSnapshotBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rl, _, _ := termLog(t, 2)
		rl.snapMeta = SnapshotMeta{LastIncludedIndex: 10, LastIncludedTerm: 3}

		// Compact everything away: the log now holds nothing but the boundary.
		rl.truncatePrefix(11)
		if rl.first != 0 {
			t.Fatalf("first = %d after compacting the whole log, want 0", rl.first)
		}

		if got, err := rl.termAt(10); err != nil || got != 3 {
			t.Fatalf("termAt(10) = %d, %v; want 3, nil", got, err)
		}
		if got, ok := rl.lastIndexOfTerm(3); !ok || got != 10 {
			t.Errorf("lastIndexOfTerm(3) = %d, %v; want 10, true: the boundary is in that term", got, ok)
		}
		if _, ok := rl.lastIndexOfTerm(9); ok {
			t.Error("lastIndexOfTerm reported a term the node has never held")
		}
	})
}
