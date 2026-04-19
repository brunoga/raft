package raft_test

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
)

// newTestNode creates a single-node cluster with a memstore and no-op transport.
func newTestNode(t testing.TB, id raft.NodeID, sm raft.StateMachine) *raft.Node {
	if sm == nil {
		sm = &noopSM{}
	}
	cfg := raft.DefaultConfig()
	cfg.ID = id
	cfg.Storage = memstore.New()
	cfg.StateMachine = sm
	cfg.Transport = &noopTransport{}
	cfg.TickInterval = 0 // Manual ticks for deterministic tests
	n, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	return n
}

// newTestNodeWithPeers creates a node with the given peers and a no-op state machine.
func newTestNodeWithPeers(t testing.TB, id raft.NodeID, peers []raft.PeerConfig) *raft.Node {
	cfg := raft.DefaultConfig()
	cfg.ID = id
	cfg.Peers = peers
	cfg.Storage = memstore.New()
	cfg.StateMachine = &noopSM{}
	cfg.Transport = &noopTransport{}
	cfg.TickInterval = 0
	n, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	return n
}

type noopSM struct{}

func (s *noopSM) Apply(_ context.Context, _ raft.LogEntry) ([]byte, error) { return nil, nil }
func (s *noopSM) Snapshot(_ context.Context, _ io.Writer) error            { return nil }
func (s *noopSM) Restore(_ context.Context, _ raft.SnapshotMeta, _ io.Reader) error {
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
func (t *noopTransport) Unregister(raft.NodeID)             {}
func (t *noopTransport) Close() error                       { return nil }

type trackingTransport struct {
	noopTransport
	unregistered atomic.Bool
}

func (t *trackingTransport) Unregister(id raft.NodeID) {
	if id == "n1" {
		t.unregistered.Store(true)
	}
}

// --- Tests ------------------------------------------------------------------

func TestNode_Stop_UnregistersFromTransport(t *testing.T) {
	tr := &trackingTransport{}
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = &noopSM{}
	cfg.Transport = tr
	cfg.TickInterval = 0
	n, _ := raft.New(&cfg)
	n.Start()
	n.Stop()
	if !tr.unregistered.Load() {
		t.Error("Node.Stop() did not call Transport.Unregister()")
	}
}

func TestReadStale_ReturnsLastApplied(t *testing.T) {
	// Single-node cluster: become leader, propose an entry, verify ReadStale
	// returns the applied index with no RPC.
	n := newTestNode(t, "n1", nil)
	n.Start()
	defer n.Stop()

	// Advance ticks to become leader.
	for range 40 {
		n.Tick()
		time.Sleep(10 * time.Millisecond)
	}

	if n.StateSnapshot() != raft.Leader {
		t.Fatal("node should be leader")
	}

	_, err := n.Propose(context.Background(), []byte("cmd1"))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if n.ReadStale() != 2 {
		t.Errorf("ReadStale: got %d, want 2", n.ReadStale())
	}
}

func TestReadIndex_BlocksUntilApplied(t *testing.T) {
	// Single-node cluster: become leader, propose an entry, verify ReadIndex
	// returns 2. (Single-node ReadIndex resolves immediately with commitIndex).
	n := newTestNode(t, "n1", nil)
	n.Start()
	defer n.Stop()

	for range 40 {
		n.Tick()
		time.Sleep(10 * time.Millisecond)
	}

	_, _ = n.Propose(context.Background(), []byte("cmd1"))

	idx, err := n.ReadIndex(context.Background())
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if idx != 2 {
		t.Errorf("ReadIndex index: got %d, want 2", idx)
	}
}

func TestReadIndex_StallsOnDelayedApply(t *testing.T) {
	// Linearizable read must wait until lastApplied >= readIndex.
	// We use a slow state machine to verify that ReadIndex blocks until
	// the proposed entry actually reaches SM.Apply.
	sm := &slowSM{applyDone: make(chan struct{})}
	n := newTestNode(t, "n1", sm)
	n.Start()
	defer n.Stop()

	for range 40 {
		n.Tick()
		time.Sleep(10 * time.Millisecond)
	}

	// Start proposal in background (it will block on SM.Apply).
	propDone := make(chan struct{})
	go func() {
		_, _ = n.Propose(context.Background(), []byte("cmd1"))
		close(propDone)
	}()

	// ReadIndex should also block.
	readDone := make(chan struct{})
	go func() {
		idx, _ := n.ReadIndex(context.Background())
		// It must return at least the commit index we saw earlier.
		if idx < 1 {
			t.Errorf("ReadIndex returned %d, want >= 1", idx)
		}
		close(readDone)
	}()

	time.Sleep(50 * time.Millisecond)
	select {
	case <-readDone:
		t.Fatal("ReadIndex returned before entry was applied")
	default:
	}

	// Release the state machine.
	close(sm.applyDone)
	<-propDone

	select {
	case <-readDone:
	case <-time.After(1 * time.Second):
		t.Fatal("ReadIndex did not return after entry was applied")
	}
}

type slowSM struct {
	applyDone chan struct{}
}

func (s *slowSM) Apply(_ context.Context, _ raft.LogEntry) ([]byte, error) {
	<-s.applyDone
	return nil, nil
}
func (s *slowSM) Snapshot(_ context.Context, _ io.Writer) error { return nil }
func (s *slowSM) Restore(_ context.Context, _ raft.SnapshotMeta, _ io.Reader) error {
	return nil
}

// Compile-time check that *Node implements Handler.
var _ raft.Handler = (*raft.Node)(nil)
