package raft_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

var errRejected = errors.New("state machine rejected the command")

// countingRejectSM fails any command beginning with "reject" and counts how
// many times each command actually reached it. The counts are what make
// re-execution visible to a test.
type countingRejectSM struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingRejectSM() *countingRejectSM {
	return &countingRejectSM{counts: make(map[string]int)}
}

func (s *countingRejectSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	cmd := string(e.Command)
	s.mu.Lock()
	s.counts[cmd]++
	s.mu.Unlock()
	if strings.HasPrefix(cmd, "reject") {
		return nil, errRejected
	}
	return []byte("ok:" + cmd), nil
}

func (s *countingRejectSM) Snapshot(_ context.Context, w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for cmd, n := range s.counts {
		if _, err := fmt.Fprintf(w, "%s\t%d\n", cmd, n); err != nil {
			return err
		}
	}
	return nil
}

func (s *countingRejectSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

func (s *countingRejectSM) count(cmd string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[cmd]
}

// startLeader brings up a one-node cluster over the supplied storage and waits
// for it to elect itself. A single node never loses leadership, so tests do not
// have to tolerate redirection.
func startLeader(t *testing.T, store raft.Storage, sm raft.StateMachine, tune func(*raft.Config)) *raft.Node {
	t.Helper()

	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = store
	cfg.StateMachine = sm
	cfg.Transport = memtransport.NewNetwork().NewTransport("n1")
	cfg.TickInterval = 0
	tuneForManualTicks(&cfg)
	if tune != nil {
		tune(&cfg)
	}

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()

	deadline := time.Now().Add(3 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("node never became leader")
		}
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	return node
}

// TestProposeOnce_FailedApplyIsNotCachedAsSuccess asserts that retrying a
// request whose state machine returned an error reports that error again,
// rather than a successful empty result.
//
// The dedup table exists so a client that retries after a timeout sees the
// original outcome exactly once. Recording a failed apply as though it had
// succeeded inverts that: the retry is answered with a nil error and a nil
// result, so a caller whose write was rejected — a duplicate key, an
// insufficient balance — is told it went through.
func TestProposeOnce_FailedApplyIsNotCachedAsSuccess(t *testing.T) {
	ctx := context.Background()
	sm := newCountingRejectSM()
	node := startLeader(t, memstore.New(), sm, nil)
	t.Cleanup(node.Stop)

	_, firstErr := node.ProposeOnce(ctx, "client-a", 1, []byte("reject-me"))
	if !errors.Is(firstErr, errRejected) {
		t.Fatalf("first attempt: err = %v, want %v", firstErr, errRejected)
	}

	// The client never saw the response and retries with the same sequence
	// number.
	result, retryErr := node.ProposeOnce(ctx, "client-a", 1, []byte("reject-me"))
	if !errors.Is(retryErr, errRejected) {
		t.Errorf("retry of a rejected request returned err = %v, result = %q; "+
			"want the original error %v", retryErr, result, errRejected)
	}
}

// TestProposeOnce_SuccessfulResultIsCached is the companion guarantee: a retry
// of a request that did succeed returns the original result without running the
// command a second time.
func TestProposeOnce_SuccessfulResultIsCached(t *testing.T) {
	ctx := context.Background()
	sm := newCountingRejectSM()
	node := startLeader(t, memstore.New(), sm, nil)
	t.Cleanup(node.Stop)

	first, err := node.ProposeOnce(ctx, "client-a", 1, []byte("accept-me"))
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	second, err := node.ProposeOnce(ctx, "client-a", 1, []byte("accept-me"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("retry returned %q, want the cached %q", second, first)
	}
	if got := sm.count("accept-me"); got != 1 {
		t.Errorf("command reached the state machine %d times, want exactly 1", got)
	}
}

// TestProposeOnce_RetryAfterSnapshotRestoreIsStillDeduplicated asserts that the
// exactly-once state a snapshot carries is still usable after a restart: a
// retry of a request recorded in that snapshot must not run the command again.
func TestProposeOnce_RetryAfterSnapshotRestoreIsStillDeduplicated(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	tune := func(cfg *raft.Config) {
		cfg.SnapshotThreshold = 4
		cfg.TrailingLogs = 3
		cfg.MaxClientTableSize = 16
	}

	sm := newCountingRejectSM()
	node := startLeader(t, store, sm, tune)
	for i := range 6 {
		id := raft.NodeID(fmt.Sprintf("client-%02d", i))
		if _, err := node.ProposeOnce(ctx, id, 1, fmt.Appendf(nil, "cmd-%d", i)); err != nil {
			t.Fatalf("ProposeOnce(%s): %v", id, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for node.SnapshotIndex() == 0 && time.Now().Before(deadline) {
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	if node.SnapshotIndex() == 0 {
		t.Fatal("node never took a snapshot")
	}
	node.Stop()

	// A fresh state machine: anything applied after the restart shows up as a
	// count of 1 here, and anything correctly deduplicated stays at 0.
	restartedSM := newCountingRejectSM()
	restarted := startLeader(t, store, restartedSM, tune)
	t.Cleanup(restarted.Stop)

	if _, err := restarted.ProposeOnce(ctx, "client-00", 1, []byte("cmd-0")); err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if got := restartedSM.count("cmd-0"); got != 0 {
		t.Errorf("command re-executed %d times after a restart that restored the "+
			"snapshot recording it; exactly-once state did not survive", got)
	}
}
