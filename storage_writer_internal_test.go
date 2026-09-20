package raft

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// recordingBatchStore records the durable operations issued against it.
type recordingBatchStore struct {
	stubStorage

	mu  sync.Mutex
	ops []string
}

func (s *recordingBatchStore) record(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, op)
}

func (s *recordingBatchStore) operations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

func (s *recordingBatchStore) SaveHardState(_ context.Context, hs HardState) error {
	s.record(fmt.Sprintf("hardstate(term=%d)", hs.CurrentTerm))
	return nil
}

func (s *recordingBatchStore) AppendLogEntries(_ context.Context, entries []LogEntry) error {
	s.record(fmt.Sprintf("append(%d..%d)", entries[0].Index, entries[len(entries)-1].Index))
	return nil
}

// batchSeamStore is recordingBatchStore that also implements BatchWriter.
type batchSeamStore struct{ recordingBatchStore }

func (s *batchSeamStore) SaveState(_ context.Context, hs *HardState, entries []LogEntry) error {
	s.record(fmt.Sprintf("savestate(term=%d,%d..%d)",
		hs.CurrentTerm, entries[0].Index, entries[len(entries)-1].Index))
	return nil
}

// TestStorageWriter_BatchesATermWithTheEntriesThatFollowIt tests the coalescing
// directly, with the queue held still.
//
// The shape is the one a follower makes on every term change that carries
// entries: record the term, then append. A store that keeps both in one log
// can write them as one record and pay one fsync where it used to pay two, and
// this is where the engine decides to give it the chance.
func TestStorageWriter_BatchesATermWithTheEntriesThatFollowIt(t *testing.T) {
	store := &batchSeamStore{}
	w := newStorageWriter(store)

	// Fill the queue without letting the writer run, so what it sees is fixed.
	w.queue = []writeOp{
		{seq: 1, kind: writeHardState, hs: HardState{CurrentTerm: 4}},
		{seq: 2, kind: writeAppend, entries: entriesAt(4, 1, 2)},
		{seq: 3, kind: writeAppend, entries: entriesAt(4, 3, 1)},
	}

	batch, stop := w.take()
	if stop {
		t.Fatal("take reported the writer should stop")
	}
	if len(batch) != 3 {
		t.Fatalf("take returned %d operations, want all 3 together", len(batch))
	}
	if err := w.run(batch); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := store.operations()
	if len(got) != 1 || got[0] != "savestate(term=4,1..3)" {
		t.Errorf("storage saw %v, want one savestate carrying the term and entries 1..3", got)
	}
}

// TestStorageWriter_WithoutTheSeamTheTermGoesAlone pins that a store which does
// not implement BatchWriter is driven exactly as before: the term first, on its
// own, so a log recovered after a crash never holds entries from a term the
// node does not believe it reached.
func TestStorageWriter_WithoutTheSeamTheTermGoesAlone(t *testing.T) {
	store := &recordingBatchStore{}
	w := newStorageWriter(store)

	w.queue = []writeOp{
		{seq: 1, kind: writeHardState, hs: HardState{CurrentTerm: 4}},
		{seq: 2, kind: writeAppend, entries: entriesAt(4, 1, 2)},
	}

	first, _ := w.take()
	if len(first) != 1 || first[0].kind != writeHardState {
		t.Fatalf("take returned %d operations starting with kind %v, want the hard state alone",
			len(first), first[0].kind)
	}
	if err := w.run(first); err != nil {
		t.Fatalf("run: %v", err)
	}
	second, _ := w.take()
	if err := w.run(second); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := store.operations()
	if len(got) != 2 || got[0] != "hardstate(term=4)" || got[1] != "append(1..2)" {
		t.Errorf("storage saw %v, want [hardstate(term=4) append(1..2)]", got)
	}
}

// TestStorageWriter_ATermWithNoEntriesIsNotBatched pins that the seam is used
// only where it helps. A term change on its own has nothing to batch with, and
// wrapping it would mean a store implementing BatchWriter never saw a plain
// hard-state write at all.
func TestStorageWriter_ATermWithNoEntriesIsNotBatched(t *testing.T) {
	store := &batchSeamStore{}
	w := newStorageWriter(store)

	w.queue = []writeOp{
		{seq: 1, kind: writeHardState, hs: HardState{CurrentTerm: 7}},
		{seq: 2, kind: writeTruncateSuffix, index: 3},
	}

	batch, _ := w.take()
	if len(batch) != 1 || batch[0].kind != writeHardState {
		t.Fatalf("take returned %d operations, want the hard state alone", len(batch))
	}
	if err := w.run(batch); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := store.operations(); len(got) != 1 || got[0] != "hardstate(term=7)" {
		t.Errorf("storage saw %v, want a plain hardstate write", got)
	}
}
