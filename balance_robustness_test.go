package raft_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft"
)

// stubProvider is a NodeProvider driven entirely by the test.
type stubProvider struct {
	statuses []raft.GroupStatus

	statusDelay time.Duration
	statusCalls atomic.Int64

	mu        sync.Mutex
	transfers []raft.Transfer
	transfer  func(ctx context.Context, groupID uint64, to raft.NodeID) error
}

func (p *stubProvider) StatusAll(ctx context.Context) []raft.GroupStatus {
	p.statusCalls.Add(1)
	if p.statusDelay > 0 {
		select {
		case <-time.After(p.statusDelay):
		case <-ctx.Done():
			return nil
		}
	}
	return p.statuses
}

func (p *stubProvider) TransferGroupLeadership(ctx context.Context, groupID uint64, to raft.NodeID) error {
	p.mu.Lock()
	p.transfers = append(p.transfers, raft.Transfer{GroupID: groupID, To: to})
	fn := p.transfer
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, groupID, to)
	}
	return nil
}

func (p *stubProvider) seen() []raft.Transfer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]raft.Transfer(nil), p.transfers...)
}

// TestBalancer_NeverTargetsANonVoter asserts that leadership is never planned
// onto a replica that cannot be elected. A non-voter does not vote and cannot
// win an election, so the transfer fails, and it is retried on every interval
// for as long as the imbalance lasts.
func TestBalancer_NeverTargetsANonVoter(t *testing.T) {
	view := map[raft.HostID][]raft.GroupStatus{
		"phys0": {
			{GroupID: 1, NodeID: "g1-p0", State: raft.Leader, Voter: true},
			{GroupID: 2, NodeID: "g2-p0", State: raft.Leader, Voter: true},
			{GroupID: 3, NodeID: "g3-p0", State: raft.Leader, Voter: true},
		},
		"phys1": {
			// Witnesses: they replicate, but they cannot lead.
			{GroupID: 1, NodeID: "g1-p1", State: raft.Follower, Voter: false},
			{GroupID: 2, NodeID: "g2-p1", State: raft.Follower, Voter: false},
			{GroupID: 3, NodeID: "g3-p1", State: raft.Follower, Voter: false},
		},
	}

	if plan := (raft.LeastLeadersBalancer{}).Plan(view); len(plan) != 0 {
		t.Errorf("planned %v; none of the destinations can be elected", plan)
	}
}

// TestBalancer_NeverTargetsALaggingReplica asserts that leadership is not moved
// to a replica that is far behind. Such a replica has to catch up before it can
// serve anything, so a rebalance would turn into an outage for that group.
func TestBalancer_NeverTargetsALaggingReplica(t *testing.T) {
	view := map[raft.HostID][]raft.GroupStatus{
		"phys0": {
			{GroupID: 1, NodeID: "g1-p0", State: raft.Leader, Voter: true, LastApplied: 100_000},
			{GroupID: 2, NodeID: "g2-p0", State: raft.Leader, Voter: true, LastApplied: 100_000},
			{GroupID: 3, NodeID: "g3-p0", State: raft.Leader, Voter: true, LastApplied: 100_000},
		},
		"phys1": {
			{GroupID: 1, NodeID: "g1-p1", State: raft.Follower, Voter: true, LastApplied: 5},
			{GroupID: 2, NodeID: "g2-p1", State: raft.Follower, Voter: true, LastApplied: 5},
			{GroupID: 3, NodeID: "g3-p1", State: raft.Follower, Voter: true, LastApplied: 5},
		},
	}

	if plan := (raft.LeastLeadersBalancer{}).Plan(view); len(plan) != 0 {
		t.Errorf("planned %v; every destination is far behind its leader", plan)
	}

	// The same view is fine once the replicas have caught up.
	for _, s := range view["phys1"] {
		_ = s
	}
	caught := map[raft.HostID][]raft.GroupStatus{
		"phys0": view["phys0"],
		"phys1": {
			{GroupID: 1, NodeID: "g1-p1", State: raft.Follower, Voter: true, LastApplied: 100_000},
			{GroupID: 2, NodeID: "g2-p1", State: raft.Follower, Voter: true, LastApplied: 99_999},
			{GroupID: 3, NodeID: "g3-p1", State: raft.Follower, Voter: true, LastApplied: 100_000},
		},
	}
	if plan := (raft.LeastLeadersBalancer{}).Plan(caught); len(plan) == 0 {
		t.Error("planned nothing for an imbalanced view whose destinations are caught up")
	}
}

// TestBalanceController_UnresponsiveNodeDoesNotStallRebalancing asserts that a
// node which never answers is bounded. Collecting the view one node at a time,
// with no deadline, lets a single black-holed node stop rebalancing for every
// group in the cluster.
func TestBalanceController_UnresponsiveNodeDoesNotStallRebalancing(t *testing.T) {
	slow := &stubProvider{statusDelay: time.Hour}
	fast := &stubProvider{statuses: []raft.GroupStatus{
		{GroupID: 1, NodeID: "g1-p1", State: raft.Leader, Voter: true},
	}}

	c := raft.NewBalanceController(
		map[raft.HostID]raft.NodeProvider{"phys0": slow, "phys1": fast},
		raft.LeastLeadersBalancer{},
		10*time.Millisecond,
		raft.WithStatusTimeout(50*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	// The healthy node must be polled repeatedly rather than once, which is
	// what happens if the loop is stuck waiting on the unresponsive one.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for fast.statusCalls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("healthy node was polled %d times; rebalancing is stalled behind "+
				"the node that never answers", fast.statusCalls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestBalanceController_RunWaitsForInFlightTransfers asserts that Run does what
// it documents: it returns only once the transfers it started have finished.
// Returning early leaves goroutines moving leadership around a cluster the
// caller believes it has finished rebalancing.
func TestBalanceController_RunWaitsForInFlightTransfers(t *testing.T) {
	release := make(chan struct{})
	var finished atomic.Bool

	busy := &stubProvider{
		statuses: []raft.GroupStatus{
			{GroupID: 1, NodeID: "g1-p0", State: raft.Leader, Voter: true},
			{GroupID: 2, NodeID: "g2-p0", State: raft.Leader, Voter: true},
			{GroupID: 3, NodeID: "g3-p0", State: raft.Leader, Voter: true},
		},
		transfer: func(ctx context.Context, _ uint64, _ raft.NodeID) error {
			<-release
			finished.Store(true)
			return nil
		},
	}
	idle := &stubProvider{statuses: []raft.GroupStatus{
		{GroupID: 1, NodeID: "g1-p1", State: raft.Follower, Voter: true},
		{GroupID: 2, NodeID: "g2-p1", State: raft.Follower, Voter: true},
		{GroupID: 3, NodeID: "g3-p1", State: raft.Follower, Voter: true},
	}}

	c := raft.NewBalanceController(
		map[raft.HostID]raft.NodeProvider{"phys0": busy, "phys1": idle},
		raft.LeastLeadersBalancer{},
		5*time.Millisecond,
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for len(busy.seen()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no transfer was ever started")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
		t.Fatal("Run returned while a transfer it started was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its transfer finished")
	}
	if !finished.Load() {
		t.Error("Run returned before the transfer completed")
	}
}

// TestBalanceController_CooldownStopsRepeatedMoves asserts that a group is left
// alone for a while after it is moved.
//
// The view is assembled from several nodes at slightly different moments, and a
// cluster may run more than one controller. Without a cooldown, two views that
// disagree hand the same group back and forth, and every move costs an
// election.
func TestBalanceController_CooldownStopsRepeatedMoves(t *testing.T) {
	// A view that stays imbalanced no matter what, so the balancer keeps
	// planning the same transfer on every interval.
	busy := &stubProvider{statuses: []raft.GroupStatus{
		{GroupID: 1, NodeID: "g1-p0", State: raft.Leader, Voter: true},
		{GroupID: 2, NodeID: "g2-p0", State: raft.Leader, Voter: true},
		{GroupID: 3, NodeID: "g3-p0", State: raft.Leader, Voter: true},
	}}
	idle := &stubProvider{statuses: []raft.GroupStatus{
		{GroupID: 1, NodeID: "g1-p1", State: raft.Follower, Voter: true},
		{GroupID: 2, NodeID: "g2-p1", State: raft.Follower, Voter: true},
		{GroupID: 3, NodeID: "g3-p1", State: raft.Follower, Voter: true},
	}}

	c := raft.NewBalanceController(
		map[raft.HostID]raft.NodeProvider{"phys0": busy, "phys1": idle},
		raft.LeastLeadersBalancer{},
		5*time.Millisecond,
		raft.WithGroupCooldown(10*time.Second),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c.Run(ctx)

	moves := map[uint64]int{}
	for _, tr := range busy.seen() {
		moves[tr.GroupID]++
	}
	for gid, n := range moves {
		if n > 1 {
			t.Errorf("group %d was moved %d times in %d intervals despite a cooldown "+
				"longer than the whole test", gid, n, 60)
		}
	}
	if len(moves) == 0 {
		t.Fatal("no transfer was attempted at all")
	}
}
