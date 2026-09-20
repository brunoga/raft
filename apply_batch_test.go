package raft_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
)

// A state machine backed by storage almost always has a way to group work:
// one transaction, one write batch, one fsync. Applying entries one at a time
// denies it that, and the entries already arrive in runs, because the apply
// loop is handed everything committed since it last looked.
//
// These tests are about that, and about the things the grouping must not
// change: the order results come back in, what a rejected command does, and
// the exactly-once guarantee.

// batchSM records the batches it was given.
type batchSM struct {
	idleSM

	mu      sync.Mutex
	batches [][]string
	applied []string
	// reject, when set, is the command the state machine refuses.
	reject string
	// failBatch, when set, fails the whole batch.
	failBatch error
}

func (s *batchSM) ApplyBatch(_ context.Context, entries []raft.LogEntry) ([]raft.ApplyOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failBatch != nil {
		return nil, s.failBatch
	}

	group := make([]string, 0, len(entries))
	out := make([]raft.ApplyOutcome, len(entries))
	for i, e := range entries {
		cmd := string(e.Command)
		group = append(group, cmd)
		if cmd == s.reject {
			out[i] = raft.ApplyOutcome{Err: errors.New("rejected: " + cmd)}
			continue
		}
		s.applied = append(s.applied, cmd)
		out[i] = raft.ApplyOutcome{Value: []byte("ok:" + cmd)}
	}
	s.batches = append(s.batches, group)
	return out, nil
}

func (s *batchSM) snapshotBatches() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.batches...)
}

func (s *batchSM) appliedCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.applied...)
}

// leaderWithSM starts a single-voter leader over sm.
func leaderWithSM(t *testing.T, sm raft.StateMachine) *raft.Node {
	t.Helper()

	cfg := safeBaseConfig(t, "n1")
	cfg.StateMachine = sm

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	stop := tickWhile(node)
	t.Cleanup(stop)
	deadline := time.Now().Add(5 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("the node never became leader")
		}
		time.Sleep(time.Millisecond)
	}
	return node
}

// TestApplyBatch_EntriesArriveTogether is the point of the interface.
func TestApplyBatch_EntriesArriveTogether(t *testing.T) {
	sm := &batchSM{}
	node := leaderWithSM(t, sm)

	// Propose concurrently so that several entries commit together and the
	// apply loop is handed more than one at a time.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = node.Propose(ctx, fmt.Appendf(nil, "cmd%02d", i))
		}(i)
	}
	wg.Wait()

	biggest := 0
	for _, b := range sm.snapshotBatches() {
		if len(b) > biggest {
			biggest = len(b)
		}
	}
	if biggest < 2 {
		t.Errorf("the largest batch the state machine saw held %d entries; "+
			"64 concurrent proposals should have grouped at least some of them", biggest)
	}
}

// TestApplyBatch_ResultsGoBackToTheRightProposer is the property that a batch
// must not disturb. Each caller waits on its own command and must get its own
// answer, not the answer to somebody else's.
func TestApplyBatch_ResultsGoBackToTheRightProposer(t *testing.T) {
	sm := &batchSM{}
	node := leaderWithSM(t, sm)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	bad := make(chan string, 64)
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := fmt.Sprintf("cmd%02d", i)
			got, err := node.Propose(ctx, []byte(cmd))
			if err != nil {
				bad <- fmt.Sprintf("%s: %v", cmd, err)
				return
			}
			if string(got) != "ok:"+cmd {
				bad <- fmt.Sprintf("%s got %q", cmd, got)
			}
		}(i)
	}
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Errorf("a proposal got the wrong answer: %s", msg)
	}
}

// TestApplyBatch_ARejectedCommandDoesNotFailItsNeighbours pins the difference
// between the state machine refusing one command and the batch failing.
//
// A rejection is an ordinary outcome: it is reported to the caller that
// proposed it, and every other entry in the batch still applies. Treating it
// as a batch failure would let one bad command undo the work of the entries
// around it.
func TestApplyBatch_ARejectedCommandDoesNotFailItsNeighbours(t *testing.T) {
	sm := &batchSM{reject: "poison"}
	node := leaderWithSM(t, sm)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	results := make(chan error, 3)
	for _, cmd := range []string{"before", "poison", "after"} {
		wg.Add(1)
		go func(cmd string) {
			defer wg.Done()
			_, err := node.Propose(ctx, []byte(cmd))
			if cmd == "poison" {
				results <- err
				return
			}
			if err != nil {
				results <- fmt.Errorf("%s was refused: %w", cmd, err)
				return
			}
			results <- nil
		}(cmd)
	}
	wg.Wait()
	close(results)

	sawRejection := false
	for err := range results {
		if err == nil {
			continue
		}
		if err.Error() == "rejected: poison" {
			sawRejection = true
			continue
		}
		t.Errorf("unexpected outcome: %v", err)
	}
	if !sawRejection {
		t.Error("the rejected command's proposer was not told")
	}

	applied := sm.appliedCommands()
	for _, want := range []string{"before", "after"} {
		found := false
		for _, got := range applied {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q was not applied; a rejected neighbour took it down with it", want)
		}
	}
}

// TestApplyBatch_AStateMachineWithoutItIsUnchanged pins that the interface is
// optional: a state machine that does not implement it sees exactly the calls
// it saw before, one entry at a time.
func TestApplyBatch_AStateMachineWithoutItIsUnchanged(t *testing.T) {
	sm := &countingSM{}
	node := leaderWithSM(t, sm)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := range 8 {
		if _, err := node.Propose(ctx, fmt.Appendf(nil, "cmd%d", i)); err != nil {
			t.Fatalf("propose %d: %v", i, err)
		}
	}

	// One call per entry, plus the no-op the leader appends on election.
	if got := sm.calls(); got < 8 {
		t.Errorf("Apply was called %d times for 8 proposals, want at least 8", got)
	}
}

// countingSM counts Apply calls and does not implement BatchApplier.
type countingSM struct {
	idleSM
	mu sync.Mutex
	n  int
}

func (s *countingSM) Apply(_ context.Context, _ raft.LogEntry) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return nil, nil
}

func (s *countingSM) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}
