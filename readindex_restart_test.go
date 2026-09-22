package raft_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// TestReadIndex_SingleVoterWaitsForItsOwnTerm is the case a single-node
// cluster makes easy to hit and hard to see.
//
// The commit index is not persisted: a restarting node learns it again from
// its leader, and a single-node cluster's leader is itself, so it comes back
// at zero with a log full of entries that certainly did commit. Raft §8 says
// a leader must not serve a read until it has committed an entry in its own
// term, and the reason is exactly this: until the no-op lands, the commit
// index is not a statement about what the cluster holds. A read answered from
// it reports an empty state machine to the client that filled it.
//
// The window is short in practice -- one log write -- which is why the log
// write is held open here rather than raced for.
func TestReadIndex_SingleVoterWaitsForItsOwnTerm(t *testing.T) {
	const committed = 5
	ctx := context.Background()

	// A log that a previous life committed, and no commit index, which is
	// what survives a restart.
	store := newGateStore()
	entries := make([]raft.LogEntry, 0, committed)
	for i := 1; i <= committed; i++ {
		entries = append(entries, raft.LogEntry{Index: raft.Index(i), Term: 1, Command: []byte("old")})
	}
	if err := store.Storage.AppendLogEntries(ctx, entries); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	if err := store.Storage.SaveHardState(ctx, raft.HardState{CurrentTerm: 1}); err != nil {
		t.Fatalf("seed hard state: %v", err)
	}

	net := memtransport.NewNetwork()
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = store
	cfg.StateMachine = &kvSM{data: make(map[string]string)}
	cfg.Transport = net.NewTransport("n1")
	cfg.TickInterval = 0
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.Register("n1", node.Handler())
	node.Start()
	// Registered before the gate, so that teardown runs in the only order
	// that terminates: cleanups are last-registered-first, and a node cannot
	// stop while a write it accepted is still held.
	t.Cleanup(node.Stop)

	// Hold the log so the no-op this node is about to append cannot land.
	// Hard-state writes are left alone, since the election needs them.
	release := store.hold(t)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && node.State() != raft.Leader {
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	if node.State() != raft.Leader {
		t.Fatal("the single voter did not become leader")
	}

	// It calls itself leader with its no-op still unwritten, which is the
	// whole of the window. A read taken now must not be answered yet.
	type result struct {
		idx raft.Index
		err error
	}
	done := make(chan result, 1)
	var answered atomic.Bool
	go func() {
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		idx, rerr := node.ReadIndex(readCtx)
		answered.Store(true)
		done <- result{idx, rerr}
	}()

	for range 50 {
		node.Tick()
		time.Sleep(2 * time.Millisecond)
	}
	if answered.Load() {
		r := <-done
		t.Fatalf("ReadIndex returned %d while this node's own term had committed nothing: "+
			"a commit index that restarted at zero was served as though it described "+
			"the cluster (err %v)", r.idx, r.err)
	}

	release()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("ReadIndex: %v", r.err)
			}
			if r.idx <= committed {
				t.Errorf("ReadIndex returned %d, want more than the %d entries that were "+
					"already committed, since the no-op is above them", r.idx, committed)
			}
			return
		case <-ticker.C:
			node.Tick()
		case <-time.After(10 * time.Second):
			t.Fatal("ReadIndex never returned after the log was released")
		}
	}
}
