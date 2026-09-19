package raft_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

var errDiskFailure = errors.New("disk is gone")

// faultyStore wraps a working store and can be told to start failing the
// durable writes Raft's safety argument depends on.
type faultyStore struct {
	raft.Storage
	failHardState atomic.Bool
	failAppend    atomic.Bool
	failTruncate  atomic.Bool
}

func (s *faultyStore) SaveHardState(ctx context.Context, hs raft.HardState) error {
	if s.failHardState.Load() {
		return errDiskFailure
	}
	return s.Storage.SaveHardState(ctx, hs)
}

func (s *faultyStore) AppendLogEntries(ctx context.Context, entries []raft.LogEntry) error {
	if s.failAppend.Load() {
		return errDiskFailure
	}
	return s.Storage.AppendLogEntries(ctx, entries)
}

func (s *faultyStore) TruncateSuffix(ctx context.Context, from raft.Index) error {
	if s.failTruncate.Load() {
		return errDiskFailure
	}
	return s.Storage.TruncateSuffix(ctx, from)
}

type idleSM struct{}

func (idleSM) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }
func (idleSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("s"))
	return err
}
func (idleSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// newFaultyNode starts a follower with two peers, so it never becomes a
// single-voter cluster that elects itself before the test is ready.
func newFaultyNode(t *testing.T) (*raft.Node, *faultyStore) {
	t.Helper()

	store := &faultyStore{Storage: memstore.New()}
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
	cfg.Storage = store
	cfg.StateMachine = idleSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport("n1")
	cfg.TickInterval = 0

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)
	return node, store
}

func waitFatal(t *testing.T, node *raft.Node) error {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := node.FatalError(); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

// TestStorage_HardStateFailureStopsTheNode asserts that a node which cannot
// persist its term and vote stops participating.
//
// Raft's one-vote-per-term rule rests entirely on that write reaching disk
// before the node acts on it. A node that carries on with an unpersisted term
// can vote for one candidate, crash, come back at the old term, and vote for a
// different candidate in the same term. Two leaders in one term follows, and
// with it the loss of committed entries. There is nothing useful such a node
// can do, so it stops.
func TestStorage_HardStateFailureStopsTheNode(t *testing.T) {
	node, store := newFaultyNode(t)
	store.failHardState.Store(true)

	// A higher-term vote request forces the node to persist a new term.
	_, _ = node.Handler().HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
		Term:         5,
		CandidateID:  "n2",
		LastLogIndex: 0,
		LastLogTerm:  0,
	})

	fatal := waitFatal(t, node)
	if fatal == nil {
		t.Fatal("node kept running after it failed to persist its term and vote")
	}
	if !errors.Is(fatal, errDiskFailure) {
		t.Errorf("FatalError() = %v, want it to wrap %v", fatal, errDiskFailure)
	}
	if !errors.Is(fatal, raft.ErrNodeFailed) {
		t.Errorf("FatalError() = %v, want it to match raft.ErrNodeFailed", fatal)
	}

	// Every subsequent operation reports the failure rather than pretending to
	// work or reporting a plain shutdown.
	if _, err := node.Propose(context.Background(), []byte("x")); !errors.Is(err, raft.ErrNodeFailed) {
		t.Errorf("Propose after failure returned %v, want raft.ErrNodeFailed", err)
	}
}

// TestStorage_AppendFailureStopsTheNode asserts the same for the log itself: a
// follower that acknowledges entries it did not durably store lets the leader
// count it towards a commit quorum for entries that can vanish.
func TestStorage_AppendFailureStopsTheNode(t *testing.T) {
	node, store := newFaultyNode(t)
	store.failAppend.Store(true)

	resp, err := node.Handler().HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
		Term:         1,
		LeaderID:     "n2",
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries:      []raft.LogEntry{{Index: 1, Term: 1, Command: []byte("x")}},
	})
	if err == nil && resp != nil && resp.Success {
		t.Fatal("follower acknowledged entries it could not store")
	}

	if fatal := waitFatal(t, node); fatal == nil {
		t.Error("node kept running after it failed to store log entries")
	}
}

// TestStorage_HealthyNodeReportsNoFatalError guards against the failure path
// firing during ordinary operation.
func TestStorage_HealthyNodeReportsNoFatalError(t *testing.T) {
	node, _ := newFaultyNode(t)

	_, _ = node.Handler().HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
		Term:         1,
		LeaderID:     "n2",
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries:      []raft.LogEntry{{Index: 1, Term: 1, Command: []byte("x")}},
	})
	time.Sleep(50 * time.Millisecond)

	if err := node.FatalError(); err != nil {
		t.Errorf("healthy node reported a fatal error: %v", err)
	}
}
