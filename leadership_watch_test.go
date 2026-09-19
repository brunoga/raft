package raft_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

type watchSM struct{}

func (watchSM) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }
func (watchSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("s"))
	return err
}
func (watchSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// watchNode starts a single-node cluster that elects itself.
func watchNode(t *testing.T, id raft.NodeID, net *memtransport.Network, peers []raft.PeerConfig) *raft.Node {
	t.Helper()

	cfg := raft.DefaultConfig()
	cfg.ID = id
	cfg.Peers = peers
	cfg.Storage = memstore.New()
	cfg.StateMachine = watchSM{}
	cfg.Transport = net.NewTransport(id)
	cfg.TickInterval = 0
	cfg.ElectionTimeoutMin = 20 * time.Millisecond
	cfg.ElectionTimeoutMax = 40 * time.Millisecond
	cfg.HeartbeatInterval = 10 * time.Millisecond

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New(%s): %v", id, err)
	}
	net.Register(id, node.Handler())
	node.Start()
	return node
}

func awaitChange(t *testing.T, ch <-chan raft.LeadershipChange, want func(raft.LeadershipChange) bool, what string) raft.LeadershipChange {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case change, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed while waiting for %s", what)
			}
			if want(change) {
				return change
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestLeadershipChanges_ReportsGainingAndLosingLeadership asserts that a
// subscriber is told when this node becomes leader and when it stops being one.
//
// Applications start and stop work on that edge: a scheduler, a compaction, an
// external lease. Polling State answers late, and cannot distinguish a brief
// leadership change from no change at all, so work carries on running on a node
// that is no longer entitled to do it.
func TestLeadershipChanges_ReportsGainingAndLosingLeadership(t *testing.T) {
	net := memtransport.NewNetwork()
	node := watchNode(t, "n1", net, nil)
	t.Cleanup(node.Stop)

	changes, stop := node.LeadershipChanges()
	defer stop()

	// The first value is the status at subscription time, so a subscriber does
	// not have to wait for a change to learn where it stands.
	select {
	case initial := <-changes:
		if initial.IsLeader {
			t.Fatalf("node reported itself leader before any election: %+v", initial)
		}
	case <-time.After(time.Second):
		t.Fatal("no initial status delivered on subscribe")
	}

	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for node.State() != raft.Leader && time.Now().Before(deadline) {
			node.Tick()
			time.Sleep(time.Millisecond)
		}
	}()

	became := awaitChange(t, changes, func(c raft.LeadershipChange) bool { return c.IsLeader },
		"this node to report itself leader")
	if became.Leader != "n1" {
		t.Errorf("leader reported as %q while claiming leadership, want n1", became.Leader)
	}
	if became.Term == 0 {
		t.Error("leadership reported at term 0")
	}

	// A higher term from elsewhere deposes it.
	if _, err := node.Handler().HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
		Term:     became.Term + 5,
		LeaderID: "n2",
	}); err != nil {
		t.Fatalf("HandleAppendEntries: %v", err)
	}

	lost := awaitChange(t, changes, func(c raft.LeadershipChange) bool { return !c.IsLeader },
		"this node to report that it is no longer leader")
	if lost.Leader != "n2" {
		t.Errorf("leader reported as %q after stepping down, want n2", lost.Leader)
	}
}

// TestLeadershipChanges_SlowSubscriberSeesCurrentStatus asserts the coalescing
// contract: a subscriber that stops reading is never handed a stale status when
// it comes back. Acting on an out-of-date leadership status is the failure this
// signal exists to prevent.
func TestLeadershipChanges_SlowSubscriberSeesCurrentStatus(t *testing.T) {
	net := memtransport.NewNetwork()
	node := watchNode(t, "n1", net, nil)
	t.Cleanup(node.Stop)

	changes, stop := node.LeadershipChanges()
	defer stop()

	// Never read while the node churns through several transitions.
	deadline := time.Now().Add(3 * time.Second)
	for node.State() != raft.Leader && time.Now().Before(deadline) {
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	if node.State() != raft.Leader {
		t.Fatal("node never became leader")
	}
	for range 5 {
		if _, err := node.Handler().HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
			Term:     node.Term() + 1,
			LeaderID: "n2",
		}); err != nil {
			t.Fatalf("HandleAppendEntries: %v", err)
		}
	}

	// Whatever is waiting must describe where the node actually is now.
	select {
	case change := <-changes:
		if change.IsLeader {
			t.Errorf("subscriber was handed a stale status claiming leadership: %+v", change)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no status available to a subscriber that fell behind")
	}
}

// TestLeadershipChanges_ChannelClosesWhenTheNodeStops asserts that a consumer
// ranging over the channel is released when the node shuts down, rather than
// blocking for ever.
func TestLeadershipChanges_ChannelClosesWhenTheNodeStops(t *testing.T) {
	net := memtransport.NewNetwork()
	node := watchNode(t, "n1", net, nil)

	changes, stop := node.LeadershipChanges()
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range changes { //nolint:revive // draining until close is the point
		}
	}()

	node.Stop()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("subscription channel was not closed when the node stopped")
	}
}

// TestLeadershipChanges_StopEndsTheSubscription asserts that unsubscribing
// releases the subscription and does not panic when called twice.
func TestLeadershipChanges_StopEndsTheSubscription(t *testing.T) {
	net := memtransport.NewNetwork()
	node := watchNode(t, "n1", net, nil)
	t.Cleanup(node.Stop)

	changes, stop := node.LeadershipChanges()
	stop()
	stop() // must be safe

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				return // closed, as expected
			}
		case <-deadline:
			t.Fatal("channel was not closed after the subscription was stopped")
		}
	}
}
