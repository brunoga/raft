package raft_test

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
)

// applyTallySM records how many times each command was applied. It is the only
// state machine here that can see a double-apply: one that overwrites a key
// with the same value cannot tell the second application from the first, which
// is exactly why a forgotten client's retry is so hard to notice in the field.
type applyTallySM struct {
	mu     sync.Mutex
	counts map[string]int
}

func newApplyTallySM() *applyTallySM { return &applyTallySM{counts: make(map[string]int)} }

func (sm *applyTallySM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	if len(e.Command) == 0 {
		return nil, nil // the leader's no-op
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.counts[string(e.Command)]++
	return []byte(fmt.Sprintf("%d", sm.counts[string(e.Command)])), nil
}

func (sm *applyTallySM) count(cmd string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.counts[cmd]
}

func (sm *applyTallySM) Snapshot(_ context.Context, _ io.Writer) error { return nil }
func (sm *applyTallySM) Restore(_ context.Context, _ raft.SnapshotMeta, _ io.Reader) error {
	return nil
}

// forgetMetrics records the ClientTableMetrics callbacks.
type forgetMetrics struct {
	mu        sync.Mutex
	forgotten []raft.NodeID
}

func (m *forgetMetrics) StateChange(raft.NodeID, raft.State, raft.State, raft.Term) {}
func (m *forgetMetrics) CommitAdvanced(raft.NodeID, raft.Index)                     {}
func (m *forgetMetrics) SnapshotTaken(raft.NodeID, raft.Index, int)                 {}

func (m *forgetMetrics) ClientForgotten(_, clientID raft.NodeID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forgotten = append(m.forgotten, clientID)
}

func (m *forgetMetrics) snapshot() []raft.NodeID {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]raft.NodeID(nil), m.forgotten...)
}

var _ raft.ClientTableMetrics = (*forgetMetrics)(nil)

// forgetNode builds a single-node cluster with a client table of the given
// size. Single node so that "what the table holds" is not racing replication.
func forgetNode(t *testing.T, tableSize int, m raft.Metrics) (*raft.Node, *applyTallySM) {
	t.Helper()
	sm := newApplyTallySM()
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = sm
	cfg.Transport = &noopTransport{}
	cfg.TickInterval = 0
	cfg.MaxClientTableSize = tableSize
	cfg.Metrics = m
	tuneForManualTicks(&cfg)

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)
	if !tickUntil(node, 5*time.Second, func() bool { return node.State() == raft.Leader }) {
		t.Fatal("node never became leader")
	}
	return node, sm
}

// TestClientForgotten_TheDoubleApplyIsAnnounced is the whole point of the
// eviction report.
//
// A client dropped from the table has its next retry executed a second time.
// That much is a consequence of bounding the table and cannot be avoided while
// clients choose their own IDs -- a client the node has forgotten and a client
// it has never seen are the same observation. What can be avoided is finding
// out from the duplicate: the command applies cleanly, the log stays
// consistent, every replica agrees, and the only other evidence is whatever
// the second execution did.
//
// So this checks both halves together: that the retry really does run twice,
// and that the node said so at the moment it stopped being able to prevent it.
func TestClientForgotten_TheDoubleApplyIsAnnounced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		m := &forgetMetrics{}
		node, sm := forgetNode(t, 2, m)

		events, stopEvents := node.Events()
		defer stopEvents()

		const cmdA = "work-for-A"

		// A is admitted, then pushed out by B and C at a table size of two.
		if _, err := node.ProposeOnce(ctx, "A", 1, []byte(cmdA)); err != nil {
			t.Fatalf("A seq=1: %v", err)
		}
		if _, err := node.ProposeOnce(ctx, "B", 1, []byte("work-for-B")); err != nil {
			t.Fatalf("B seq=1: %v", err)
		}
		if _, err := node.ProposeOnce(ctx, "C", 1, []byte("work-for-C")); err != nil {
			t.Fatalf("C seq=1: %v", err)
		}

		if got := sm.count(cmdA); got != 1 {
			t.Fatalf("A's command applied %d times before any retry, want 1", got)
		}

		// A retries the request it never got an answer to. It is not in the table
		// any more, so the node cannot tell this from a first attempt.
		if _, err := node.ProposeOnce(ctx, "A", 1, []byte(cmdA)); err != nil {
			t.Fatalf("A seq=1 retry: %v", err)
		}
		if got := sm.count(cmdA); got != 2 {
			t.Fatalf("A's command applied %d times after its retry, want 2: this test is about "+
				"the duplicate, and without one there is nothing to report", got)
		}

		// The node must have said, at the eviction, that A was the client it could
		// no longer answer for.
		forgotten := m.snapshot()
		if len(forgotten) == 0 {
			t.Fatal("Metrics saw no eviction; the guarantee was lost with no signal at all")
		}
		if forgotten[0] != "A" {
			t.Errorf("first client forgotten = %q, want A: it was the least recently written",
				forgotten[0])
		}

		// And the same on the event stream, for consumers that use it instead.
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev := <-events:
				if ev.Type != raft.EventClientForgotten {
					continue
				}
				if ev.Client != "A" {
					t.Errorf("EventClientForgotten.Client = %q, want A", ev.Client)
				}
				return
			case <-deadline:
				t.Fatal("no EventClientForgotten within 5s")
			}
		}
	})
}

// TestClientForgotten_ReportedOncePerEviction guards the one way this could be
// wrong in a way nobody would notice.
//
// There are two copies of the client table -- the event loop's and the apply
// loop's -- and they evict in lockstep, because both are driven by the same
// entries in the same order. Reporting from both would double every count, and
// a doubled count is worse than none: it is the number an operator sizes the
// table from.
func TestClientForgotten_ReportedOncePerEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		m := &forgetMetrics{}
		node, _ := forgetNode(t, 1, m)

		// A table of one evicts on every new client after the first, so three
		// clients is exactly two evictions.
		for i, id := range []raft.NodeID{"A", "B", "C"} {
			if _, err := node.ProposeOnce(ctx, id, 1, fmt.Appendf(nil, "cmd%d", i)); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
		}

		forgotten := m.snapshot()
		want := []raft.NodeID{"A", "B"}
		if len(forgotten) != len(want) {
			t.Fatalf("%d evictions reported %v, want exactly %d %v; the event loop and the apply "+
				"loop each hold a copy of this table and only one of them may report",
				len(forgotten), forgotten, len(want), want)
		}
		for i := range want {
			if forgotten[i] != want[i] {
				t.Errorf("eviction %d = %q, want %q", i, forgotten[i], want[i])
			}
		}
	})
}

// TestClientTableSize_IsTheSignalBeforeTheCliff checks the measurement that
// arrives in time to do something about it.
//
// An eviction report says the guarantee has already been lost for that client.
// Occupancy is what says it is about to be: a table at 90% of its bound is one
// busy minute away from forgetting somebody, and that is the point at which
// raising MaxClientTableSize still costs nothing.
func TestClientTableSize_IsTheSignalBeforeTheCliff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		node, _ := forgetNode(t, 3, nil)

		if got := node.ClientTableSize(); got != 0 {
			t.Errorf("ClientTableSize() = %d on a node that has served no client, want 0", got)
		}

		for i, id := range []raft.NodeID{"A", "B", "C"} {
			if _, err := node.ProposeOnce(ctx, id, 1, fmt.Appendf(nil, "cmd%d", i)); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			if got := node.ClientTableSize(); got != i+1 {
				t.Errorf("after %d clients, ClientTableSize() = %d, want %d", i+1, got, i+1)
			}
		}

		// At the bound it stays at the bound: every further client costs one.
		if _, err := node.ProposeOnce(ctx, "D", 1, []byte("cmd3")); err != nil {
			t.Fatalf("D: %v", err)
		}
		if got := node.ClientTableSize(); got != 3 {
			t.Errorf("ClientTableSize() = %d past MaxClientTableSize 3, want it pinned at the bound", got)
		}

		// A repeat client updates rather than inserts, so it must not move.
		if _, err := node.ProposeOnce(ctx, "D", 2, []byte("cmd4")); err != nil {
			t.Fatalf("D seq=2: %v", err)
		}
		if got := node.ClientTableSize(); got != 3 {
			t.Errorf("ClientTableSize() = %d after a client's second request, want 3", got)
		}
	})
}
