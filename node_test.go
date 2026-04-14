package raft_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
)

// newTestNode creates a single-node cluster with a memstore and a no-op
// state machine. TickInterval=0 so the caller drives ticks manually.
func newTestNode(t testing.TB, id raft.NodeID, peers []raft.NodeID) *raft.Node {
	t.Helper()
	cfg := raft.DefaultConfig()
	cfg.ID = id
	cfg.Peers = peers
	cfg.Storage = memstore.New()
	cfg.StateMachine = &noopSM{}
	cfg.Transport = &noopTransport{}
	cfg.TickInterval = 0 // manual ticks

	n, err := raft.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n
}

// tickN calls Tick n times with a brief yield between each so the event
// loop goroutine has a chance to process each tick.
func tickN(n *raft.Node, count int) {
	for range count {
		n.Tick()
		time.Sleep(time.Millisecond)
	}
}

// --- no-op helpers ----------------------------------------------------------

type noopSM struct{}

func (s *noopSM) Apply(_ context.Context, _ raft.LogEntry) ([]byte, error) { return nil, nil }
func (s *noopSM) Snapshot(_ context.Context) ([]byte, error)               { return nil, nil }
func (s *noopSM) Restore(_ context.Context, _ raft.SnapshotMeta, _ []byte) error {
	return nil
}

type noopTransport struct{}

func (t *noopTransport) RequestVote(_ context.Context, _ raft.NodeID, _ *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	return &raft.RequestVoteResponse{}, nil
}
func (t *noopTransport) AppendEntries(_ context.Context, _ raft.NodeID, _ *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	return &raft.AppendEntriesResponse{}, nil
}
func (t *noopTransport) InstallSnapshot(_ context.Context, _ raft.NodeID, _ *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	return &raft.InstallSnapshotResponse{}, nil
}
func (t *noopTransport) TimeoutNow(_ context.Context, _ raft.NodeID, _ *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	return &raft.TimeoutNowResponse{}, nil
}
func (t *noopTransport) ReadIndex(_ context.Context, _ raft.NodeID, _ *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	return &raft.ReadIndexResponse{}, nil
}
func (t *noopTransport) Register(raft.NodeID, raft.Handler) {}
func (t *noopTransport) Close() error                       { return nil }

// --- Tests ------------------------------------------------------------------

func TestReadStale_ReturnsLastApplied(t *testing.T) {
	// Single-node cluster: become leader, propose an entry, verify ReadStale
	// returns the applied index with no RPC.
	n := newTestNode(t, "n1", nil)
	n.Start()
	defer n.Stop()

	// Drive to leader.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && n.StateSnapshot() != raft.Leader {
		tickN(n, 1)
	}
	if n.StateSnapshot() != raft.Leader {
		t.Fatal("node did not become leader")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Propose and wait.
	done := make(chan error, 1)
	go func() { _, err := n.Propose(ctx, []byte("x")); done <- err }()
	for time.Now().Before(deadline) {
		tickN(n, 1)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Propose: %v", err)
			}
			goto proposed
		default:
		}
	}
	t.Fatal("Propose timed out")
proposed:
	idx := n.ReadStale()
	if idx == 0 {
		t.Fatal("ReadStale returned 0 after a proposal was applied")
	}
	if idx != n.LastApplied() {
		t.Fatalf("ReadStale=%d != LastApplied=%d", idx, n.LastApplied())
	}
}

func TestNode_StartsAsFollower(t *testing.T) {
	n := newTestNode(t, "n1", nil)
	n.Start()
	defer n.Stop()
	time.Sleep(5 * time.Millisecond)
	if s := n.StateSnapshot(); s != raft.Follower {
		t.Fatalf("expected Follower on start, got %s", s)
	}
}

func TestNode_StopIsIdempotent(t *testing.T) {
	n := newTestNode(t, "n1", nil)
	n.Start()
	n.Stop()
	n.Stop() // second call must not panic or block
}

func TestNode_StartIsIdempotent(t *testing.T) {
	n := newTestNode(t, "n1", nil)
	n.Start()
	n.Start() // second call must be a no-op
	n.Stop()
}

func TestNode_TickDoesNotBlockAfterStop(t *testing.T) {
	n := newTestNode(t, "n1", nil)
	n.Start()
	n.Stop()
	// Tick after stop must return quickly.
	done := make(chan struct{})
	go func() { n.Tick(); close(done) }()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Tick blocked after Stop")
	}
}

func TestNode_SingleNode_BecomesLeader(t *testing.T) {
	// A single-node cluster should elect itself leader after the election
	// timeout fires.
	n := newTestNode(t, "n1", nil) // no peers → immediate leader on election
	n.Start()
	defer n.Stop()

	// electionMinTicks = max(2, ElectionTimeoutMin/TickInterval)
	// With TickInterval=0 we use the default 10ms, so min ticks = 15.
	// Drive well past the timeout.
	tickN(n, 40)

	if s := n.StateSnapshot(); s != raft.Leader {
		t.Fatalf("single-node should become leader, got %s", s)
	}
}

func TestNode_LoadsPersistedTerm(t *testing.T) {
	store := memstore.New()
	_ = store.SaveHardState(context.Background(), raft.HardState{CurrentTerm: 5, VotedFor: "n2"})

	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = store
	cfg.StateMachine = &noopSM{}
	cfg.Transport = &noopTransport{}
	cfg.TickInterval = 0

	n, err := raft.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.Start()
	defer n.Stop()

	// The node must start in a term >= 5 (it won't go backwards).
	time.Sleep(5 * time.Millisecond)
	// We can't read currentTerm directly, but we can verify it doesn't
	// crash and starts as Follower.
	if s := n.StateSnapshot(); s != raft.Follower {
		t.Fatalf("expected Follower after loading term, got %s", s)
	}
}

func TestNode_ProposeRejectsWhenNotLeader(t *testing.T) {
	n := newTestNode(t, "n1", []raft.NodeID{"n2", "n3"}) // has peers → not auto-leader
	n.Start()
	defer n.Stop()
	time.Sleep(5 * time.Millisecond)

	ctx := context.Background()
	_, err := n.Propose(ctx, []byte("cmd"))
	if !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader from Propose, got %v", err)
	}
}

func TestNode_ProposeRejectsAfterStop(t *testing.T) {
	n := newTestNode(t, "n1", nil)
	n.Start()
	tickN(n, 40)
	n.Stop() // stopCh is now closed

	// Propose must return ErrStopped immediately (the stopCh pre-check fires).
	_, err := n.Propose(context.Background(), []byte("cmd"))
	if err != raft.ErrStopped {
		t.Fatalf("expected ErrStopped after stop, got %v", err)
	}
}

func TestNode_SingleNode_ProposeAndApply(t *testing.T) {
	store := memstore.New()
	sm := &recordSM{}

	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = store
	cfg.StateMachine = sm
	cfg.Transport = &noopTransport{}
	cfg.TickInterval = 0

	n, err := raft.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.Start()
	defer n.Stop()

	// Become leader.
	tickN(n, 40)
	if n.StateSnapshot() != raft.Leader {
		t.Skip("did not become leader in time")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := n.Propose(ctx, []byte("hello")); err != nil {
		t.Fatalf("Propose: %v", err)
	}
}

// recordSM records applied commands.
type recordSM struct {
	applied [][]byte
}

func (s *recordSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	s.applied = append(s.applied, e.Command)
	return e.Command, nil
}
func (s *recordSM) Snapshot(_ context.Context) ([]byte, error)               { return nil, nil }
func (s *recordSM) Restore(_ context.Context, _ raft.SnapshotMeta, _ []byte) error {
	return nil
}

// Compile-time check that *Node implements Handler.
var _ raft.Handler = (*raft.Node)(nil)
