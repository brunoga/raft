package raft_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
)

// A state machine that is itself a database already has, on its own disk, the
// effect of every entry it has applied. Without a way to say so it is rebuilt
// from a snapshot and then replayed over on every restart, which is work that
// has already been done -- and for operations that are not idempotent, work
// that must not be done twice.

// durableSM keeps a durable applied index and records what it is asked to
// apply, so a test can see what was replayed.
type durableSM struct {
	idleSM

	mu      sync.Mutex
	at      raft.Index
	err     error
	applied []raft.Index
}

func (s *durableSM) AppliedIndex(context.Context) (raft.Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.at, s.err
}

func (s *durableSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, e.Index)
	return nil, nil
}

func (s *durableSM) appliedIndices() []raft.Index {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]raft.Index(nil), s.applied...)
}

// seededStore returns a store already holding entries 1..n in term 1, as a
// node that had been running would have.
func seededStore(t *testing.T, n int) raft.Storage {
	t.Helper()
	store := memstore.New()
	entries := make([]raft.LogEntry, 0, n)
	for i := 1; i <= n; i++ {
		entries = append(entries, raft.LogEntry{
			Index: raft.Index(i), Term: 1, Command: fmt.Appendf(nil, "cmd%d", i),
		})
	}
	if err := store.AppendLogEntries(context.Background(), entries); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	if err := store.SaveHardState(context.Background(), raft.HardState{CurrentTerm: 1}); err != nil {
		t.Fatalf("seed hard state: %v", err)
	}
	return store
}

// TestDurableStateMachine_IsNotReplayedOverWhatItAlreadyHas is the point.
func TestDurableStateMachine_IsNotReplayedOverWhatItAlreadyHas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sm := &durableSM{at: 3}

		cfg := safeBaseConfig(t, "n1")
		cfg.Storage = seededStore(t, 5)
		cfg.StateMachine = sm

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()
		t.Cleanup(node.Stop)

		// A single voter commits its whole log as soon as it leads.
		stop := tickWhile(node)
		defer stop()
		deadline := time.Now().Add(5 * time.Second)
		for node.LastApplied() < 5 {
			if time.Now().After(deadline) {
				t.Fatalf("the node never applied past %d", node.LastApplied())
			}
			time.Sleep(time.Millisecond)
		}

		for _, idx := range sm.appliedIndices() {
			if idx <= 3 {
				t.Errorf("entry %d was applied again although the state machine already had it", idx)
			}
		}
	})
}

// TestDurableStateMachine_WithoutTheInterfaceEverythingIsReplayed pins that
// the interface is what changes the behaviour, not the seeded log.
func TestDurableStateMachine_WithoutTheInterfaceEverythingIsReplayed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sm := &replayCountingSM{}

		cfg := safeBaseConfig(t, "n1")
		cfg.Storage = seededStore(t, 5)
		cfg.StateMachine = sm

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()
		t.Cleanup(node.Stop)

		stop := tickWhile(node)
		defer stop()
		deadline := time.Now().Add(5 * time.Second)
		for node.LastApplied() < 5 {
			if time.Now().After(deadline) {
				t.Fatal("the node never applied the seeded log")
			}
			time.Sleep(time.Millisecond)
		}

		saw := sm.appliedIndices()
		for want := raft.Index(1); want <= 5; want++ {
			found := false
			for _, got := range saw {
				if got == want {
					found = true
				}
			}
			if !found {
				t.Errorf("entry %d was not applied; a state machine with no durable state needs all of them", want)
			}
		}
	})
}

// replayCountingSM records applies and does not implement DurableStateMachine.
type replayCountingSM struct {
	idleSM
	mu      sync.Mutex
	applied []raft.Index
}

func (s *replayCountingSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, e.Index)
	return nil, nil
}

func (s *replayCountingSM) appliedIndices() []raft.Index {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]raft.Index(nil), s.applied...)
}

// TestDurableStateMachine_AheadOfTheLogIsRefused covers the state that cannot
// be reconciled.
//
// A state machine claiming entries this node does not have cannot be replayed
// up to, and ignoring the claim would apply those indices a second time when
// the log caught up. Refusing to start is the only honest answer, and it names
// both numbers so the operator can see which storage is the odd one out.
func TestDurableStateMachine_AheadOfTheLogIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		cfg.Storage = seededStore(t, 5)
		cfg.StateMachine = &durableSM{at: 9}

		_, err := raft.New(&cfg)
		if err == nil {
			t.Fatal("a state machine ahead of the log was accepted")
		}
		if !strings.Contains(err.Error(), "ahead of this node's log") {
			t.Errorf("New returned %v, want it to say the state machine is ahead of the log", err)
		}
	})
}

// TestDurableStateMachine_AFailedQueryStopsConstruction pins that the engine
// does not guess. A state machine that cannot say what it has is one whose
// storage is in an unknown state, and starting anyway means choosing between
// replaying what it already had and skipping what it did not.
func TestDurableStateMachine_AFailedQueryStopsConstruction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		cfg.Storage = seededStore(t, 3)
		cfg.StateMachine = &durableSM{err: errors.New("disk unreadable")}

		_, err := raft.New(&cfg)
		if err == nil {
			t.Fatal("a state machine that could not report its applied index was accepted")
		}
		if !strings.Contains(err.Error(), "disk unreadable") {
			t.Errorf("New returned %v, want it to carry the state machine's error", err)
		}
	})
}
