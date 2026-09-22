package raft_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// limitedTransport reports a maximum message size, as a real network transport
// does, and refuses anything larger.
type limitedTransport struct {
	raft.Transport
	limit int
}

func (t *limitedTransport) MaxMessageBytes() int { return t.limit }

func (t *limitedTransport) AppendEntries(ctx context.Context, to raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	size := 0
	for _, e := range req.Entries {
		size += len(e.Command)
	}
	if size > t.limit {
		return nil, errors.New("message larger than the transport allows")
	}
	return t.Transport.AppendEntries(ctx, to, req)
}

type sizeSM2 struct{}

func (sizeSM2) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }
func (sizeSM2) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("s"))
	return err
}
func (sizeSM2) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// leaderWithLimit starts a single-voter leader whose transport enforces limit.
func leaderWithLimit(t *testing.T, limit int, tune func(*raft.Config)) *raft.Node {
	t.Helper()

	net := memtransport.NewNetwork()
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	// One non-voting peer, so the node leads alone but still replicates.
	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: false}}
	cfg.Storage = memstore.New()
	cfg.StateMachine = sizeSM2{}
	cfg.Transport = &limitedTransport{Transport: net.NewTransport("n1"), limit: limit}
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
	t.Cleanup(node.Stop)

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

// TestPropose_RejectsACommandTooLargeToReplicate asserts that a command the
// transport could never carry is refused at submission, rather than accepted
// and appended.
//
// Nothing bounded the size of a command. An oversized one was accepted, written
// durably to the log, and only then found to be unsendable: the transport
// rejects it, the leader treats that as an ordinary dropped RPC, and retries
// the identical message on every heartbeat for ever. The entry never commits,
// so every later proposal queues behind it and the group stops making progress
// — from nothing more than application data being too big. Refusing it at the
// door turns a silent, permanent stall into an error the caller can act on.
func TestPropose_RejectsACommandTooLargeToReplicate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const limit = 64 * 1024
	node := leaderWithLimit(t, limit, nil)

	_, err := node.Propose(ctx, make([]byte, limit*2))
	if !errors.Is(err, raft.ErrProposalTooLarge) {
		t.Fatalf("Propose of an unreplicable command returned %v, want ErrProposalTooLarge", err)
	}

	// The log must be untouched: a rejected proposal is not a log entry.
	before := node.LastApplied()
	if _, err := node.Propose(ctx, []byte("small")); err != nil {
		t.Fatalf("a later small proposal failed, so the group is stalled: %v", err)
	}
	if got := node.LastApplied(); got != before+1 {
		t.Errorf("applied index moved by %d, want 1: the rejected command reached the log", got-before)
	}
}

// TestPropose_AcceptsACommandThatFits is the companion guard: the limit must
// not reject commands the transport can carry.
func TestPropose_AcceptsACommandThatFits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const limit = 64 * 1024
	node := leaderWithLimit(t, limit, nil)

	if _, err := node.Propose(ctx, make([]byte, limit/2)); err != nil {
		t.Errorf("Propose of a command well inside the transport limit: %v", err)
	}
}

// TestPropose_ConfiguredLimitOverridesTheTransport asserts that an explicit
// limit is honoured, for transports that cannot report one.
func TestPropose_ConfiguredLimitOverridesTheTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	node := leaderWithLimit(t, 1<<20, func(cfg *raft.Config) {
		cfg.MaxProposalBytes = 4096
	})

	if _, err := node.Propose(ctx, make([]byte, 8192)); !errors.Is(err, raft.ErrProposalTooLarge) {
		t.Errorf("Propose past the configured limit returned %v, want ErrProposalTooLarge", err)
	}
	if _, err := node.Propose(ctx, make([]byte, 1024)); err != nil {
		t.Errorf("Propose inside the configured limit: %v", err)
	}
}

// TestProposeOnce_RejectsACommandTooLargeToReplicate covers the exactly-once
// entry point, whose dedup header makes the entry larger than the command.
func TestProposeOnce_RejectsACommandTooLargeToReplicate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const limit = 64 * 1024
	node := leaderWithLimit(t, limit, nil)

	if _, err := node.ProposeOnce(ctx, "client", 1, make([]byte, limit*2)); !errors.Is(err, raft.ErrProposalTooLarge) {
		t.Errorf("ProposeOnce of an unreplicable command returned %v, want ErrProposalTooLarge", err)
	}
}
