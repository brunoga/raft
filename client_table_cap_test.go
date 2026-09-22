package raft_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/filestore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// waitTablesConverge ticks until every node reports the same client table
// size and bound, or fails.
func waitTablesConverge(t *testing.T, c *Cluster) {
	t.Helper()
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		same := true
		for _, n := range c.nodes[1:] {
			if n.ClientTableSize() != c.nodes[0].ClientTableSize() ||
				n.MaxClientTableSize() != c.nodes[0].MaxClientTableSize() {
				same = false
			}
		}
		if same {
			return
		}
	}
	for i, n := range c.nodes {
		t.Logf("%s: size %d bound %d", c.ids[i], n.ClientTableSize(), n.MaxClientTableSize())
	}
	t.Fatal("client tables did not converge")
}

// TestClientTableCap_MismatchedConfigsConverge is the case the old
// documentation warned about: nodes configured with different
// MaxClientTableSize values. The bound is now agreed through the log, so
// every replica keeps the same table whatever its own Config said.
func TestClientTableCap_MismatchedConfigsConverge(t *testing.T) {
	sizes := map[raft.NodeID]int{"n1": 2, "n2": 50, "n3": 1000}
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.MaxClientTableSize = sizes[cfg.ID]
	})
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]
	ctx := context.Background()

	// Before any ProposeOnce, every node is on its own configuration.
	for i, n := range c.nodes {
		if got := n.MaxClientTableSize(); got != sizes[c.ids[i]] {
			t.Fatalf("%s: MaxClientTableSize = %d before any agreement, want its own %d",
				c.ids[i], got, sizes[c.ids[i]])
		}
	}

	// Ten distinct clients: more than the smallest configured bound, fewer
	// than the others, so under per-node bounds n1 would have evicted and
	// the rest would not.
	for i := range 10 {
		client := raft.NodeID(fmt.Sprintf("client-%d", i))
		if _, err := leader.ProposeOnce(ctx, client, 1, []byte(fmt.Sprintf("k%d=v", i))); err != nil {
			t.Fatalf("ProposeOnce %s: %v", client, err)
		}
	}
	waitTablesConverge(t, c)

	want := sizes[leader.ID()]
	for i, n := range c.nodes {
		if got := n.MaxClientTableSize(); got != want {
			t.Errorf("%s: MaxClientTableSize = %d, want the leader's %d", c.ids[i], got, want)
		}
		wantSize := min(10, want)
		if got := n.ClientTableSize(); got != wantSize {
			t.Errorf("%s: ClientTableSize = %d, want %d", c.ids[i], got, wantSize)
		}
	}
}

// TestSetMaxClientTableSize_ChangesEveryReplica checks that the bound can be
// changed on a running cluster, that a smaller bound evicts everywhere, and
// that the change survives a leader change: the new leader carries the
// agreed bound, not its own Config.
func TestSetMaxClientTableSize_ChangesEveryReplica(t *testing.T) {
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.MaxClientTableSize = 100
	})
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]
	ctx := context.Background()

	for i := range 5 {
		if _, err := leader.ProposeOnce(ctx, raft.NodeID(fmt.Sprintf("c%d", i)), 1, []byte("k=v")); err != nil {
			t.Fatalf("ProposeOnce: %v", err)
		}
	}
	waitTablesConverge(t, c)

	if err := leader.SetMaxClientTableSize(ctx, 2); err != nil {
		t.Fatalf("SetMaxClientTableSize: %v", err)
	}
	waitTablesConverge(t, c)
	for i, n := range c.nodes {
		if n.MaxClientTableSize() != 2 || n.ClientTableSize() != 2 {
			t.Errorf("%s: bound %d size %d after SetMaxClientTableSize(2), want 2 and 2",
				c.ids[i], n.MaxClientTableSize(), n.ClientTableSize())
		}
	}

	// The two most recent clients survived; an older one was forgotten, so
	// its retry runs again -- which is what a smaller bound means.
	if _, err := leader.ProposeOnce(ctx, "c4", 1, []byte("k=v")); err != nil {
		t.Fatalf("retry of a surviving client: %v", err)
	}
	waitTablesConverge(t, c)
	if got := leader.ClientTableSize(); got != 2 {
		t.Errorf("a retry of a surviving client grew the table to %d", got)
	}

	// A negative size is refused before anything is proposed.
	if err := leader.SetMaxClientTableSize(ctx, -1); err == nil {
		t.Error("SetMaxClientTableSize(-1) succeeded")
	}

	// A follower cannot change it.
	follower := c.nodes[(leaderIdx+1)%3]
	if err := follower.SetMaxClientTableSize(ctx, 3); err == nil {
		t.Error("SetMaxClientTableSize on a follower succeeded")
	}
}

// TestClientTableCap_SnapshotCarriesTheBound checks that a node restarted
// from a snapshot uses the bound the group had agreed, not the one in the
// Config it was restarted with.
func TestClientTableCap_SnapshotCarriesTheBound(t *testing.T) {
	dir := t.TempDir()
	net := memtransport.NewNetwork()
	sm := &counterSM{}
	// One budget for the whole test, and it has to cover two elections, six
	// writes, a snapshot and a restart. Five seconds of it was enough only
	// while every wait before the last one finished quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := func(tableSize int) (*raft.Node, *filestore.FileStore) {
		fs := openFileStore(t, dir)
		cfg := raft.DefaultConfig()
		cfg.ID = "n1"
		cfg.Storage = fs
		cfg.StateMachine = sm
		cfg.Transport = net.NewTransport("n1")
		cfg.TickInterval = 0
		cfg.SnapshotThreshold = 3
		cfg.TrailingLogs = 0
		cfg.MaxClientTableSize = tableSize
		n, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		net.Register("n1", n.Handler())
		n.Start()
		// Generous on purpose. What is being waited for is a handful of
		// election timeouts, which is milliseconds of work; the budget is
		// for a machine running the rest of the suite beside this, where a
		// sleep between ticks is not the millisecond it asks for. A failure
		// here should mean stuck, not busy.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) && n.State() != raft.Leader {
			n.Tick()
			time.Sleep(time.Millisecond)
		}
		if n.State() != raft.Leader {
			t.Fatal("did not become leader")
		}
		return n, fs
	}

	// First life: the group agrees a bound of 3, then snapshots.
	n, fs := start(3)
	if got := n.MaxClientTableSize(); got != 3 {
		t.Fatalf("fresh node reports bound %d, want its Config's 3", got)
	}
	for i := range 6 {
		if _, err := n.ProposeOnce(ctx, raft.NodeID(fmt.Sprintf("c%d", i)), 1, []byte("x")); err != nil {
			t.Fatalf("ProposeOnce: %v", err)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && n.SnapshotIndex() == 0 {
		n.Tick()
		time.Sleep(time.Millisecond)
	}
	if n.SnapshotIndex() == 0 {
		t.Fatal("no snapshot was taken")
	}
	// Keep going so that the snapshot is the whole story and nothing after it
	// re-establishes the bound from the log. Its own budget, because unlike
	// the waits above this one has no assertion behind it: if the snapshot
	// never quite catches up with the applied index the test is still worth
	// running, and spinning to a shared deadline would leave nothing for what
	// comes after.
	settle := time.Now().Add(2 * time.Second)
	for time.Now().Before(settle) && n.SnapshotIndex() < n.LastApplied() {
		n.Tick()
		time.Sleep(time.Millisecond)
	}
	if n.ClientTableSize() != 3 {
		t.Fatalf("table size %d under a bound of 3", n.ClientTableSize())
	}
	n.Stop()
	net.Unregister("n1")
	if err := fs.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Second life, restarted with a Config that says 1000: the snapshot's 3
	// wins, and the table it loaded is still bounded by it.
	n, _ = start(1000)
	defer n.Stop()
	if got := n.MaxClientTableSize(); got != 3 {
		t.Fatalf("restarted node reports bound %d, want the snapshot's 3", got)
	}
	if got := n.ClientTableSize(); got != 3 {
		t.Fatalf("restarted node's table holds %d entries, want 3", got)
	}
	if _, err := n.ProposeOnce(ctx, "late", 1, []byte("x")); err != nil {
		t.Fatalf("ProposeOnce after restart: %v", err)
	}
	if got := n.ClientTableSize(); got != 3 {
		t.Fatalf("table grew to %d after restart; the agreed bound of 3 was lost", got)
	}
}
