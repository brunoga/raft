package raft_test

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// countingTransport records how much work a leader does to bring a peer up to
// date: how many log entries it ships and how many snapshot transfers it
// starts.
type countingTransport struct {
	raft.Transport
	entriesSent      atomic.Int64
	installSnapshots atomic.Int64
}

func (t *countingTransport) AppendEntries(ctx context.Context, to raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	t.entriesSent.Add(int64(len(req.Entries)))
	return t.Transport.AppendEntries(ctx, to, req)
}

func (t *countingTransport) InstallSnapshot(ctx context.Context, to raft.NodeID, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	if req.Offset == 0 {
		t.installSnapshots.Add(1)
	}
	return t.Transport.InstallSnapshot(ctx, to, req)
}

// inertSM applies nothing and produces a small snapshot.
type inertSM struct{ applied atomic.Int64 }

func (s *inertSM) Apply(_ context.Context, _ raft.LogEntry) ([]byte, error) {
	s.applied.Add(1)
	return nil, nil
}

func (s *inertSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("state"))
	return err
}

func (s *inertSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// backoffCluster is a manually wired cluster that exposes each node's transport
// so tests can observe the RPCs a leader actually sends.
type backoffCluster struct {
	ids        []raft.NodeID
	nodes      []*raft.Node
	transports []*countingTransport
	net        *memtransport.Network
}

func newBackoffCluster(t *testing.T, n int, snapshotThreshold uint64) *backoffCluster {
	t.Helper()

	c := &backoffCluster{net: memtransport.NewNetwork()}
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

		tr := &countingTransport{Transport: c.net.NewTransport(c.ids[i])}
		cfg := raft.DefaultConfig()
		cfg.ID = c.ids[i]
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = &inertSM{}
		cfg.Transport = tr
		cfg.TickInterval = 0
		cfg.SnapshotThreshold = snapshotThreshold
		if snapshotThreshold > 0 {
			// The engine caps trailing at SnapshotThreshold-1 so that compaction
			// reclaims something; say so here rather than make it warn.
			cfg.TrailingLogs = snapshotThreshold - 1
		}

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(%s): %v", c.ids[i], err)
		}
		c.net.Register(cfg.ID, node.Handler())
		c.nodes = append(c.nodes, node)
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

func (c *backoffCluster) tick(n int) {
	for range n {
		for _, node := range c.nodes {
			node.Tick()
		}
		time.Sleep(time.Millisecond)
	}
}

// tickUntil drives the cluster until cond holds or the deadline expires.
func (c *backoffCluster) tickUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		c.tick(1)
	}
	return cond()
}

func (c *backoffCluster) leaderIndex() int {
	for i, node := range c.nodes {
		if node.State() == raft.Leader {
			return i
		}
	}
	return -1
}

func (c *backoffCluster) isolate(i int) {
	for j := range c.nodes {
		if i != j {
			c.net.Drop(c.ids[i], c.ids[j])
			c.net.Drop(c.ids[j], c.ids[i])
		}
	}
}

func (c *backoffCluster) rejoin(i int) {
	for j := range c.nodes {
		if i != j {
			c.net.Restore(c.ids[i], c.ids[j])
			c.net.Restore(c.ids[j], c.ids[i])
		}
	}
}

// TestReplication_NewLeaderResumesFromConflictHint asserts that a new leader
// resumes replication to a slightly lagging follower from the point the
// follower reports, rather than from the beginning of the log.
//
// A new leader initialises nextIndex for every peer to its own last index + 1,
// so its first contact with a lagging follower is rejected. The rejection
// carries the conflict hints that say where to resume. If those hints are lost
// on the way back to the event loop — as they were for every response that
// arrives through the heartbeat pump or the read-barrier path — nextIndex falls
// to 1 and the leader re-ships its entire log. Worse, on a leader that has ever
// compacted, index 1 is at or below the snapshot boundary, so the leader sends
// a full snapshot instead: a routine leader change turns into a full state
// transfer to every follower that happened to be briefly behind.
func TestReplication_NewLeaderResumesFromConflictHint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		const backlog = 30
		// Snapshots are disabled so that compaction cannot interfere: this test is
		// about where replication resumes, not about snapshot transfer.
		c := newBackoffCluster(t, 3, 0)

		if !c.tickUntil(3*time.Second, func() bool { return c.leaderIndex() >= 0 }) {
			t.Fatal("no leader elected")
		}
		li := c.leaderIndex()
		leader := c.nodes[li]
		follower := (li + 1) % 3
		successor := (li + 2) % 3

		// Build a backlog so that "resume from the start" is clearly distinguishable
		// from "resume from the hint".
		for i := range backlog {
			if _, err := leader.Propose(ctx, fmt.Appendf(nil, "entry-%d", i)); err != nil {
				t.Fatalf("propose: %v", err)
			}
		}
		target := leader.LastApplied()
		if !c.tickUntil(5*time.Second, func() bool {
			return c.nodes[follower].LastApplied() >= target && c.nodes[successor].LastApplied() >= target
		}) {
			t.Fatalf("cluster never converged: %d/%d/%d",
				c.nodes[0].LastApplied(), c.nodes[1].LastApplied(), c.nodes[2].LastApplied())
		}

		// Isolate a follower, commit two more entries, then hand leadership over so
		// the new leader starts with an optimistic nextIndex for everyone.
		c.isolate(follower)
		for i := range 2 {
			if _, err := leader.Propose(ctx, fmt.Appendf(nil, "late-%d", i)); err != nil {
				t.Fatalf("propose while partitioned: %v", err)
			}
		}
		if err := leader.TransferLeadership(ctx, c.ids[successor]); err != nil {
			t.Fatalf("transfer leadership: %v", err)
		}
		newLeader := c.nodes[successor]
		if !c.tickUntil(3*time.Second, func() bool { return newLeader.State() == raft.Leader }) {
			t.Fatal("successor never became leader")
		}

		gap := newLeader.LastApplied() - c.nodes[follower].LastApplied()
		if gap == 0 || gap > 5 {
			t.Fatalf("follower is %d entries behind; the test intends a small, non-zero gap", gap)
		}

		before := c.transports[successor].entriesSent.Load()
		c.rejoin(follower)

		if !c.tickUntil(5*time.Second, func() bool {
			return c.nodes[follower].LastApplied() >= newLeader.LastApplied()
		}) {
			t.Fatalf("follower never caught up: follower=%d leader=%d",
				c.nodes[follower].LastApplied(), newLeader.LastApplied())
		}
		c.tick(5)

		// Catching up a follower that is a handful of entries behind must cost a
		// handful of entries, not the whole log.
		sent := c.transports[successor].entriesSent.Load() - before
		if sent >= backlog {
			t.Errorf("new leader shipped %d entries to catch up a follower %d entries behind; "+
				"replication restarted from the beginning of the log instead of the conflict hint",
				sent, gap)
		}
	})
}

// TestReplication_LaggingFollowerDoesNotTriggerSnapshot is the consequence of
// the same defect on a leader that has compacted: a collapsed nextIndex lands
// at or below the snapshot boundary, so the leader ships its entire state
// machine rather than the few entries the follower is missing.
func TestReplication_LaggingFollowerDoesNotTriggerSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := newBackoffCluster(t, 3, 4)

		if !c.tickUntil(3*time.Second, func() bool { return c.leaderIndex() >= 0 }) {
			t.Fatal("no leader elected")
		}
		li := c.leaderIndex()
		leader := c.nodes[li]
		follower := (li + 1) % 3
		successor := (li + 2) % 3

		// Compact, then let everyone converge past the compaction boundary so that
		// the entries the follower later misses are still in the leader's log.
		for i := range 12 {
			if _, err := leader.Propose(ctx, fmt.Appendf(nil, "entry-%d", i)); err != nil {
				t.Fatalf("propose: %v", err)
			}
		}
		target := leader.LastApplied()
		if !c.tickUntil(5*time.Second, func() bool {
			return leader.SnapshotIndex() > 0 &&
				c.nodes[follower].LastApplied() >= target &&
				c.nodes[successor].LastApplied() >= target
		}) {
			t.Skipf("cluster did not reach a compacted, converged state in time (applied %d/%d/%d, snapshot %d)",
				c.nodes[0].LastApplied(), c.nodes[1].LastApplied(), c.nodes[2].LastApplied(), leader.SnapshotIndex())
		}

		c.isolate(follower)
		if _, err := leader.Propose(ctx, []byte("late")); err != nil {
			t.Fatalf("propose while partitioned: %v", err)
		}
		if err := leader.TransferLeadership(ctx, c.ids[successor]); err != nil {
			t.Fatalf("transfer leadership: %v", err)
		}
		newLeader := c.nodes[successor]
		if !c.tickUntil(3*time.Second, func() bool { return newLeader.State() == raft.Leader }) {
			t.Fatal("successor never became leader")
		}

		// Only meaningful while every entry the follower needs is still in the
		// leader's log.
		firstNeeded := c.nodes[follower].LastApplied() + 1
		if firstNeeded <= newLeader.SnapshotIndex() {
			t.Skipf("follower needs compacted entries (needs from %d, leader compacted through %d); "+
				"a snapshot is legitimately required here", firstNeeded, newLeader.SnapshotIndex())
		}

		before := c.transports[successor].installSnapshots.Load()
		c.rejoin(follower)
		if !c.tickUntil(5*time.Second, func() bool {
			return c.nodes[follower].LastApplied() >= newLeader.LastApplied()
		}) {
			t.Fatalf("follower never caught up: follower=%d leader=%d",
				c.nodes[follower].LastApplied(), newLeader.LastApplied())
		}
		c.tick(5)

		if sent := c.transports[successor].installSnapshots.Load() - before; sent != 0 {
			t.Errorf("leader started %d snapshot transfers for a follower whose missing entries "+
				"were all still in the log", sent)
		}
	})
}
