package raft_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
)

// These tests are about the one shape that arrives at storage twice when it
// could arrive once: a follower learning of a new term and taking entries in
// the same message records the term, then appends, back to back. A store that
// keeps both in one log can write them as one record and pay one fsync where
// it used to pay two.
//
// The engine cannot use such a store without a way to say "these belong
// together". Storage has thirteen methods and no room to add a fourteenth
// after a release, so the seam is an optional interface rather than a new
// method.

// batchStore records how storage was called, and can be built with or without
// the batch seam so the same assertions can be made both ways.
type batchStore struct {
	raft.Storage

	mu  sync.Mutex
	ops []string
}

func (s *batchStore) record(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, op)
}

func (s *batchStore) operations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

func (s *batchStore) SaveHardState(ctx context.Context, hs raft.HardState) error {
	s.record(fmt.Sprintf("hardstate(term=%d)", hs.CurrentTerm))
	return s.Storage.SaveHardState(ctx, hs)
}

func (s *batchStore) AppendLogEntries(ctx context.Context, entries []raft.LogEntry) error {
	s.record(fmt.Sprintf("append(%d..%d)", entries[0].Index, entries[len(entries)-1].Index))
	return s.Storage.AppendLogEntries(ctx, entries)
}

// batchingStore is batchStore plus the optional seam.
type batchingStore struct{ batchStore }

func (s *batchingStore) SaveState(ctx context.Context, hs *raft.HardState, entries []raft.LogEntry) error {
	switch {
	case hs != nil && len(entries) > 0:
		s.record(fmt.Sprintf("savestate(term=%d,%d..%d)",
			hs.CurrentTerm, entries[0].Index, entries[len(entries)-1].Index))
	case hs != nil:
		s.record(fmt.Sprintf("savestate(term=%d)", hs.CurrentTerm))
	default:
		s.record(fmt.Sprintf("savestate(%d..%d)", entries[0].Index, entries[len(entries)-1].Index))
	}
	if hs != nil {
		if err := s.Storage.SaveHardState(ctx, *hs); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		return nil
	}
	return s.Storage.AppendLogEntries(ctx, entries)
}

// followerTakingANewTerm starts a follower over store and delivers one
// AppendEntries that both raises its term and carries entries.
func followerTakingANewTerm(t *testing.T, store raft.Storage) *raft.Node {
	t.Helper()

	cfg := safeBaseConfig(t, "n1")
	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
	cfg.Storage = store

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := node.Handler().HandleAppendEntries(ctx, appendFrom(4, 1, 3, 0))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !resp.Success {
		t.Fatalf("append was rejected: %+v", resp)
	}
	return node
}

// TestBatchWrite_AStoreWithoutTheSeamIsUnchanged pins that the optional
// interface is optional. A store that does not implement it must see exactly
// what it saw before, in the same order: the term first, so that a log
// recovered after a crash never holds entries from a term the node does not
// believe it reached.
func TestBatchWrite_AStoreWithoutTheSeamIsUnchanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &batchStore{Storage: memstore.New()}
		followerTakingANewTerm(t, store)

		ops := store.operations()
		if len(ops) != 2 {
			t.Fatalf("storage was called %d times: %v; want two, the term then the entries", len(ops), ops)
		}
		if ops[0] != "hardstate(term=4)" || ops[1] != "append(1..3)" {
			t.Errorf("storage saw %v, want [hardstate(term=4) append(1..3)]", ops)
		}
	})
}

// TestBatchWrite_AStoreWithTheSeamSeesTheSameWork pins the other half: batching
// changes how many calls the work arrives in, never what the work is.
//
// Whether any particular pair is batched is up to timing -- the writer takes
// whatever is queued when it looks, and the term is queued a moment before the
// entries -- so this asserts the outcome rather than the call count. The
// batching itself is tested directly in storage_writer_internal_test.go, where
// the queue can be held still.
func TestBatchWrite_AStoreWithTheSeamSeesTheSameWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &batchingStore{batchStore{Storage: memstore.New()}}
		node := followerTakingANewTerm(t, store)

		deadline := time.Now().Add(5 * time.Second)
		for node.Term() != 4 {
			if time.Now().After(deadline) {
				t.Fatal("the node never reached term 4")
			}
			time.Sleep(time.Millisecond)
		}

		last, err := store.LastIndex()
		if err != nil {
			t.Fatalf("LastIndex: %v", err)
		}
		for last < 3 {
			if time.Now().After(deadline) {
				t.Fatalf("the store holds up to index %d, want 3", last)
			}
			time.Sleep(time.Millisecond)
			if last, err = store.LastIndex(); err != nil {
				t.Fatalf("LastIndex: %v", err)
			}
		}

		hs, err := store.LoadHardState(context.Background())
		if err != nil {
			t.Fatalf("LoadHardState: %v", err)
		}
		if hs.CurrentTerm != 4 {
			t.Errorf("the store holds term %d, want 4", hs.CurrentTerm)
		}
		if ops := store.operations(); len(ops) == 0 {
			t.Error("the store was never written to")
		}
	})
}
