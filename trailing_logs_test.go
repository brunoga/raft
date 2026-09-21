package raft_test

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

// snapshotCountingTransport counts snapshot transfers started.
type snapshotCountingTransport struct {
	raft.Transport
	installs atomic.Int64
}

func (t *snapshotCountingTransport) InstallSnapshot(ctx context.Context, to raft.NodeID, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	if req.Offset == 0 {
		t.installs.Add(1)
	}
	return t.Transport.InstallSnapshot(ctx, to, req)
}

type trailingSM struct{}

func (trailingSM) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }
func (trailingSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("state"))
	return err
}
func (trailingSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

type trailingCluster struct {
	ids        []raft.NodeID
	nodes      []*raft.Node
	stores     []raft.Storage
	transports []*snapshotCountingTransport
	net        *memtransport.Network
}

func newTrailingCluster(t *testing.T, n int, tune func(*raft.Config)) *trailingCluster {
	t.Helper()

	c := &trailingCluster{net: memtransport.NewNetwork()}
	for i := range n {
		c.ids = append(c.ids, raft.NodeID(fmt.Sprintf("n%d", i+1)))
	}
	for i := range n {
		var peers []raft.PeerConfig
		for j := range n {
			if i != j {
				peers = append(peers, raft.PeerConfig{ID: c.ids[j], Voter: true})
			}
		}
		tr := &snapshotCountingTransport{Transport: c.net.NewTransport(c.ids[i])}
		store := memstore.New()

		cfg := raft.DefaultConfig()
		cfg.ID = c.ids[i]
		cfg.Peers = peers
		cfg.Storage = store
		cfg.StateMachine = trailingSM{}
		cfg.Transport = tr
		cfg.TickInterval = 0
		if tune != nil {
			tune(&cfg)
		}

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(%s): %v", c.ids[i], err)
		}
		c.net.Register(cfg.ID, node.Handler())
		c.nodes = append(c.nodes, node)
		c.stores = append(c.stores, store)
		c.transports = append(c.transports, tr)
	}
	for _, node := range c.nodes {
		node.Start()
	}
	t.Cleanup(func() {
		for _, node := range c.nodes {
			node.Stop()
		}
	})
	return c
}

func (c *trailingCluster) tick(n int) {
	for range n {
		for _, node := range c.nodes {
			node.Tick()
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *trailingCluster) tickUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		c.tick(1)
	}
	return cond()
}

func (c *trailingCluster) leaderIndex() int {
	for i, n := range c.nodes {
		if n.State() == raft.Leader {
			return i
		}
	}
	return -1
}

func (c *trailingCluster) totalInstalls() int64 {
	var total int64
	for _, tr := range c.transports {
		total += tr.installs.Load()
	}
	return total
}

// TestTrailingLogs_CompactionDoesNotForceSnapshotsOnSlightlyBehindFollowers
// asserts that a follower which is a few entries behind when the leader
// compacts is still caught up by log replication.
//
// Compaction that keeps nothing behind the snapshot point makes a full state
// transfer the only way to catch up anyone who was even one entry behind at
// that instant — and with automatic snapshots, that instant comes round again
// and again. On a large state machine this is the difference between shipping
// a handful of entries and shipping gigabytes, repeatedly, to followers that
// were never meaningfully behind.
func TestTrailingLogs_CompactionDoesNotForceSnapshotsOnSlightlyBehindFollowers(t *testing.T) {
	ctx := context.Background()
	const threshold = 10
	c := newTrailingCluster(t, 3, func(cfg *raft.Config) {
		cfg.SnapshotThreshold = threshold
		cfg.TrailingLogs = 8
	})

	if !c.tickUntil(3*time.Second, func() bool { return c.leaderIndex() >= 0 }) {
		t.Fatal("no leader elected")
	}
	li := c.leaderIndex()
	leader := c.nodes[li]
	follower := (li + 1) % 3

	propose := func(n int) {
		t.Helper()
		for i := range n {
			if _, err := leader.Propose(ctx, fmt.Appendf(nil, "entry-%d", i)); err != nil {
				t.Fatalf("propose: %v", err)
			}
			c.tick(1)
		}
	}
	converge := func() {
		t.Helper()
		target := leader.LastApplied()
		if !c.tickUntil(5*time.Second, func() bool {
			for _, n := range c.nodes {
				if n.LastApplied() < target {
					return false
				}
			}
			return true
		}) {
			t.Fatalf("cluster never converged: %d/%d/%d",
				c.nodes[0].LastApplied(), c.nodes[1].LastApplied(), c.nodes[2].LastApplied())
		}
	}

	// Get everyone level, with the leader having compacted at least once so the
	// next compaction point is predictable.
	propose(threshold + 2)
	converge()
	if leader.SnapshotIndex() == 0 {
		t.Fatal("leader never compacted; the test would prove nothing")
	}

	// Advance to two entries short of the next compaction, everyone still level.
	remaining := int(leader.SnapshotIndex()+threshold) - int(leader.LastApplied()) - 2
	if remaining > 0 {
		propose(remaining)
	}
	converge()

	// Now the follower misses the last two entries, and the leader compacts
	// past it. It is two entries behind, well inside the retained tail.
	for j := range c.nodes {
		if j != follower {
			c.net.Drop(c.ids[follower], c.ids[j])
			c.net.Drop(c.ids[j], c.ids[follower])
		}
	}
	installsBefore := c.totalInstalls()
	boundaryBefore := leader.SnapshotIndex()
	propose(2)
	if !c.tickUntil(3*time.Second, func() bool { return leader.SnapshotIndex() > boundaryBefore }) {
		t.Fatalf("leader did not compact again (boundary %d, applied %d)",
			leader.SnapshotIndex(), leader.LastApplied())
	}
	gap := leader.SnapshotIndex() - c.nodes[follower].LastApplied()

	for j := range c.nodes {
		if j != follower {
			c.net.Restore(c.ids[follower], c.ids[j])
			c.net.Restore(c.ids[j], c.ids[follower])
		}
	}
	if !c.tickUntil(5*time.Second, func() bool {
		return c.nodes[follower].LastApplied() >= leader.LastApplied()
	}) {
		t.Fatalf("follower never caught up: %d vs %d",
			c.nodes[follower].LastApplied(), leader.LastApplied())
	}
	c.tick(5)

	if got := c.totalInstalls() - installsBefore; got != 0 {
		t.Errorf("follower was %d entries behind the compaction point and still needed "+
			"%d snapshot transfer(s); the retained tail should have covered it", gap, got)
	}
}

// TestTrailingLogs_LogIsStillCompacted is the counterweight: retaining entries
// behind the snapshot point must not stop the log from being reclaimed, or the
// log grows without bound and compaction is pointless.
func TestTrailingLogs_LogIsStillCompacted(t *testing.T) {
	ctx := context.Background()
	c := newTrailingCluster(t, 1, func(cfg *raft.Config) {
		cfg.SnapshotThreshold = 8
		cfg.TrailingLogs = 4
		tuneForManualTicks(cfg)
	})

	if !c.tickUntil(3*time.Second, func() bool { return c.leaderIndex() >= 0 }) {
		t.Fatal("no leader elected")
	}
	leader := c.nodes[0]

	for i := range 60 {
		if _, err := leader.Propose(ctx, fmt.Appendf(nil, "entry-%d", i)); err != nil {
			t.Fatalf("propose: %v", err)
		}
		c.tick(1)
	}
	if !c.tickUntil(3*time.Second, func() bool { return leader.SnapshotIndex() > 0 }) {
		t.Fatal("leader never compacted")
	}
	c.tick(20)

	first, err := c.stores[0].FirstIndex()
	if err != nil {
		t.Fatalf("FirstIndex: %v", err)
	}
	if first <= 1 {
		t.Errorf("log still starts at index %d after compacting through %d; "+
			"nothing was reclaimed", first, leader.SnapshotIndex())
	}
	// The retained tail should be close to the configured size, not unbounded.
	if kept := leader.SnapshotIndex() - first + 1; kept > 16 {
		t.Errorf("kept %d entries behind the snapshot point, want about 4", kept)
	}
}
