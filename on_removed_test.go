package raft_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
)

// nodeByID returns the cluster node with the given ID.
func (c *Cluster) nodeByID(id raft.NodeID) *raft.Node {
	for i, nid := range c.ids {
		if nid == id {
			return c.nodes[i]
		}
	}
	return nil
}

// removalRecorder counts OnRemoved invocations per node.
type removalRecorder struct {
	mu    sync.Mutex
	calls map[raft.NodeID]int
	fired chan raft.NodeID
}

func newRemovalRecorder() *removalRecorder {
	return &removalRecorder{calls: make(map[raft.NodeID]int), fired: make(chan raft.NodeID, 16)}
}

func (r *removalRecorder) callback(id raft.NodeID) func() {
	return func() {
		r.mu.Lock()
		r.calls[id]++
		r.mu.Unlock()
		r.fired <- id
	}
}

func (r *removalRecorder) count(id raft.NodeID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[id]
}

// TestOnRemoved_FollowerRemovedByLeader checks that a node removed by the
// leader has its OnRemoved callback invoked once, that no other node's is,
// and that the callback may stop the node without deadlocking.
func TestOnRemoved_FollowerRemovedByLeader(t *testing.T) {
	rec := newRemovalRecorder()
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.OnRemoved = rec.callback(cfg.ID)
	})

	leader := c.Leader(5 * time.Second)
	if leader == nil {
		t.Fatal("no leader")
	}
	// Pick a follower to remove.
	var victim raft.NodeID
	for _, id := range c.ids {
		if id != leader.ID() {
			victim = id
			break
		}
	}

	stopTicking := tickWhile(c.nodes...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := leader.RemoveServer(ctx, victim); err != nil {
		cancel()
		stopTicking()
		t.Fatalf("RemoveServer(%s): %v", victim, err)
	}
	cancel()

	select {
	case id := <-rec.fired:
		if id != victim {
			stopTicking()
			t.Fatalf("OnRemoved fired on %s; want %s", id, victim)
		}
	case <-time.After(5 * time.Second):
		stopTicking()
		t.Fatalf("OnRemoved did not fire on %s", victim)
	}
	// Let a few more ticks through so a second, spurious invocation would
	// have had its chance.
	time.Sleep(50 * time.Millisecond)
	stopTicking()

	for _, id := range c.ids {
		want := 0
		if id == victim {
			want = 1
		}
		if got := rec.count(id); got != want {
			t.Errorf("OnRemoved on %s fired %d times, want %d", id, got, want)
		}
	}

	// A removed node must not go on campaigning: it is no longer a voter in
	// its own view, so ticking it should leave it a follower in the same term.
	v := c.nodeByID(victim)
	if v.State() != raft.Follower {
		t.Fatalf("removed node is %v, want Follower", v.State())
	}
	termBefore := v.Term()
	for range 10 * 30 {
		v.Tick()
	}
	time.Sleep(20 * time.Millisecond)
	if v.State() != raft.Follower || v.Term() != termBefore {
		t.Fatalf("removed node campaigned: state %v term %d (was %d)",
			v.State(), v.Term(), termBefore)
	}

	// Stopping from inside the callback is the documented use, so the removed
	// node must be stoppable while the callback goroutine is still around.
	done := make(chan struct{})
	go func() {
		c.nodeByID(victim).Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop on the removed node did not return")
	}
}

// TestOnRemoved_LeaderRemovesItself checks that a leader that leaves the
// cluster through ReconfigureCluster gets the callback once the finalise
// entry commits, and that the callback can call Stop on it.
func TestOnRemoved_LeaderRemovesItself(t *testing.T) {
	rec := newRemovalRecorder()
	var nodes sync.Map // NodeID -> *raft.Node, for the callback to find its node
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		id := cfg.ID
		record := rec.callback(id)
		cfg.OnRemoved = func() {
			record()
			if n, ok := nodes.Load(id); ok {
				n.(*raft.Node).Stop()
			}
		}
	})
	for i, id := range c.ids {
		nodes.Store(id, c.nodes[i])
	}

	leader := c.Leader(5 * time.Second)
	if leader == nil {
		t.Fatal("no leader")
	}
	var remaining []raft.PeerConfig
	for _, id := range c.ids {
		if id != leader.ID() {
			remaining = append(remaining, raft.PeerConfig{ID: id, Voter: true})
		}
	}

	stopTicking := tickWhile(c.nodes...)
	defer stopTicking()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := leader.ReconfigureCluster(ctx, remaining); err != nil {
		t.Fatalf("ReconfigureCluster: %v", err)
	}

	select {
	case id := <-rec.fired:
		if id != leader.ID() {
			t.Fatalf("OnRemoved fired on %s; want the old leader %s", id, leader.ID())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("OnRemoved did not fire on the old leader %s", leader.ID())
	}

	// The callback stopped the node; that must complete, and the rest of the
	// cluster must go on to elect a leader among the remaining members.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if leader.State() == raft.Follower {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var newLeader *raft.Node
	for time.Now().Before(deadline) {
		for _, n := range c.nodes {
			if n.ID() != leader.ID() && n.State() == raft.Leader {
				newLeader = n
			}
		}
		if newLeader != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if newLeader == nil {
		t.Fatal("no new leader elected among the remaining members")
	}
	if got := rec.count(leader.ID()); got != 1 {
		t.Errorf("OnRemoved on the old leader fired %d times, want 1", got)
	}
}
