package raft_test

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// nullSM is a state machine with no state of its own; these tests care only
// about the membership the Raft layer keeps alongside it.
type nullSM struct{}

func (nullSM) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }

func (nullSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("state"))
	return err
}

func (nullSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// restartableNode builds a node over storage the caller owns, so the same
// storage can be handed to a fresh node to simulate a restart.
func restartableNode(t *testing.T, store raft.Storage, net *memtransport.Network, id raft.NodeID, peers []raft.PeerConfig, snapshotThreshold uint64) *raft.Node {
	t.Helper()

	cfg := raft.DefaultConfig()
	cfg.ID = id
	cfg.Peers = peers
	cfg.Storage = store
	cfg.StateMachine = nullSM{}
	cfg.Transport = net.NewTransport(id)
	cfg.TickInterval = 0
	cfg.SnapshotThreshold = snapshotThreshold
	if snapshotThreshold > 0 {
		// The engine caps trailing at SnapshotThreshold-1 so that compaction
		// reclaims something; say so here rather than make it warn.
		cfg.TrailingLogs = snapshotThreshold - 1
	}
	tuneForManualTicks(&cfg)

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New(%s): %v", id, err)
	}
	net.Register(id, node.Handler())
	node.Start()
	return node
}

func tickUntil(node *raft.Node, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func hasMember(members []raft.PeerConfig, id raft.NodeID) bool {
	for _, m := range members {
		if m.ID == id {
			return true
		}
	}
	return false
}

// TestMembership_SurvivesRestartAfterCompaction asserts that a committed
// membership change is still in effect after a restart that loads from a
// snapshot taken after the change.
//
// Cluster membership is agreed through the log, so it is cluster state, not
// configuration: once a change commits, every replica must recover it from its
// own durable state. If the membership lives only in log entries that
// compaction later discards, a restarting node silently falls back to whatever
// peer list the operator passed to New. A majority restarting with a stale list
// can then elect a leader under the old quorum rules while the rest of the
// cluster uses the new ones, which is how a cluster ends up with two leaders
// that each believe they have a majority.
func TestMembership_SurvivesRestartAfterCompaction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		store := memstore.New()
		net := memtransport.NewNetwork()

		// Bootstrap a single-node cluster and add a non-voting member. A non-voter
		// keeps this node the only voter, so it stays leader throughout and the
		// test does not depend on elections.
		node := restartableNode(t, store, net, "n1", nil, 2)
		if !tickUntil(node, 3*time.Second, func() bool { return node.State() == raft.Leader }) {
			t.Fatal("node never became leader")
		}
		if err := node.AddServer(ctx, raft.PeerConfig{ID: "n2", Voter: false}); err != nil {
			t.Fatalf("AddServer: %v", err)
		}
		if !hasMember(node.Members(), "n2") {
			t.Fatalf("member not added before restart: %v", node.Members())
		}

		// Commit enough entries that the config change is compacted into a
		// snapshot and is no longer present in the log.
		for range 6 {
			if _, err := node.Propose(ctx, []byte("entry")); err != nil {
				t.Fatalf("propose: %v", err)
			}
		}
		if !tickUntil(node, 3*time.Second, func() bool { return node.SnapshotIndex() > 0 }) {
			t.Fatal("node never took a snapshot")
		}
		node.Stop()

		// Restart with the peer list the operator originally bootstrapped with.
		// The membership must come from durable state, not from this argument.
		restarted := restartableNode(t, store, net, "n1", nil, 2)
		t.Cleanup(restarted.Stop)

		if !hasMember(restarted.Members(), "n2") {
			t.Errorf("committed membership lost across restart: members = %v, want n2 present",
				restarted.Members())
		}
	})
}

// TestMembership_SurvivesRestartFromLog is the same guarantee for a change that
// is still in the log rather than folded into a snapshot: it must be in effect
// immediately after New, before any entry is re-applied, because the node can
// be asked to vote before its apply loop has caught up.
func TestMembership_SurvivesRestartFromLog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		store := memstore.New()
		net := memtransport.NewNetwork()

		node := restartableNode(t, store, net, "n1", nil, 0) // no compaction
		if !tickUntil(node, 3*time.Second, func() bool { return node.State() == raft.Leader }) {
			t.Fatal("node never became leader")
		}
		if err := node.AddServer(ctx, raft.PeerConfig{ID: "n2", Voter: false}); err != nil {
			t.Fatalf("AddServer: %v", err)
		}
		node.Stop()

		restarted := restartableNode(t, store, net, "n1", nil, 0)
		t.Cleanup(restarted.Stop)

		// Checked before any tick: no election, no replay, nothing applied yet.
		if !hasMember(restarted.Members(), "n2") {
			t.Errorf("membership from the log not in effect after restart: members = %v, want n2 present",
				restarted.Members())
		}
	})
}
