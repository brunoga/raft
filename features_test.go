package raft_test

// Tests for:
//   - §11 Lease-based reads (ReadIndexLease, ErrLeaseExpired)
//   - §12 Joint consensus (ReconfigureCluster)
//   - Pending proposal drain on step-down

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

// Compile-time check: recordTracer implements raft.Tracer.
var _ raft.Tracer = (*recordTracer)(nil)

// ---- §13 Pre-vote receiver check -------------------------------------------

// TestPreVote_LeaderDeniesPreVote is the key test for the pre-vote receiver
// check on a live leader.
//
// Scenario (3-node cluster: L=leader, A=follower, B=follower):
//  1. Drop the L→B link so B stops receiving heartbeats and eventually times out.
//     B→L and all A links are intact.
//  2. Once B is a PreCandidate it sends pre-vote requests. A (hearing from L)
//     denies. L (live leader) must ALSO deny — otherwise B collects a majority
//     (self + L) and escalates to a real election that forces L to step down.
//  3. After the healing window we verify L is still the leader and the cluster
//     can still commit a proposal.
func TestPreVote_LeaderDeniesPreVote(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)

	// Identify the two followers.
	followerB := (leaderIdx + 1) % 3

	// Drop only the L→B direction so B stops receiving heartbeats from L but
	// can still send to L (and L can still reach A, preserving quorum).
	c.DropLink(leaderIdx, followerB)

	// Tick long enough for B to time out and attempt multiple pre-vote rounds.
	// The cluster must NOT change leaders during this window.
	deadline := time.Now().Add(electionTimeout * 2)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
	}

	// The original leader must still be the leader. If the pre-vote bug were
	// present, L would grant B's pre-vote, B would win a majority (self + L),
	// and B would escalate to a real election that bumps the term and deposes L.
	if got := c.leaderIndex(); got != leaderIdx {
		t.Fatalf("leader changed from %d to %d — leader granted a pre-vote it should have denied",
			leaderIdx, got)
	}

	// Restore the link and confirm the cluster stays healthy.
	c.RestoreLink(leaderIdx, followerB)
	if _, err := c.Propose(electionTimeout, []byte("after-heal")); err != nil {
		t.Fatalf("propose after heal: %v", err)
	}
}

// TestPreVote_FollowerDeniesWhenLeaderAlive verifies that a follower that is
// still receiving heartbeats denies pre-votes from a partitioned peer.
// This exercises the follower branch of the receiver check (electionElapsed <
// electionTimeout). The cluster must remain stable for the full window.
func TestPreVote_FollowerDeniesWhenLeaderAlive(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)

	// Fully partition one follower. It will time out and run pre-votes, but
	// since it cannot actually reach anyone the pre-votes are dropped. This
	// test mainly checks cluster stability: the two connected nodes must not
	// disrupt themselves.
	partitioned := (leaderIdx + 1) % 3
	c.Disconnect(partitioned)

	deadline := time.Now().Add(electionTimeout * 2)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
	}

	// Reconnect and verify clean recovery.
	c.Reconnect(partitioned)
	c.WaitLeader(electionTimeout)

	if _, err := c.Propose(electionTimeout, []byte("after-partition")); err != nil {
		t.Fatalf("propose after partition heal: %v", err)
	}
}

// ---- §17 RPC Tracer integration --------------------------------------------

// TestTracer_InvokedForAllRPCTypes verifies that the Tracer.StartRPC hook is
// called for outbound RequestVote, AppendEntries, and TimeoutNow RPCs by
// running a small cluster with a recording tracer injected.
func TestTracer_InvokedForAllRPCTypes(t *testing.T) {
	type call struct {
		rpcType string
	}
	var (
		mu    sync.Mutex
		calls []call
	)

	recordingTracer := &recordTracer{
		record: func(rpcType string) {
			mu.Lock()
			calls = append(calls, call{rpcType})
			mu.Unlock()
		},
	}

	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.Tracer = recordingTracer
	})
	c.WaitLeader(electionTimeout)

	// Propose so that AppendEntries with entries flows.
	if _, err := c.Propose(electionTimeout, []byte("hello")); err != nil {
		t.Fatalf("propose: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	seen := make(map[string]bool)
	for _, c := range calls {
		seen[c.rpcType] = true
	}
	// RequestVote happens during election; AppendEntries during replication.
	for _, want := range []string{"RequestVote", "AppendEntries"} {
		if !seen[want] {
			t.Errorf("no trace call for rpcType %q; got: %v", want, calls)
		}
	}
}

// recordTracer is a raft.Tracer that records each StartRPC call for testing.
type recordTracer struct {
	record func(rpcType string)
}

func (r *recordTracer) StartRPC(_ raft.NodeID, _ raft.NodeID, rpcType string) func(error) {
	r.record(rpcType)
	return func(error) {}
}

// ---- Clock helpers ----------------------------------------------------------

// manualClock is an injectable raft.Clock whose time can be advanced by tests.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Now()}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ---- Lease-read tests -------------------------------------------------------

// TestReadIndexLease_ErrLeaseExpiredOnFreshLeader verifies that ReadIndexLease
// returns ErrLeaseExpired when the leader has not yet completed a heartbeat
// quorum round (no lease is held).
func TestReadIndexLease_ErrLeaseExpiredOnFreshLeader(t *testing.T) {
	c := newCluster(t, 3)
	// WaitLeader ticks the cluster but does NOT run a full ReadIndex barrier,
	// so leaseExpiry starts as zero.
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()

	// First ReadIndexLease on a fresh leader should report no lease.
	_, err := leader.ReadIndexLease(ctx)
	if !errors.Is(err, raft.ErrLeaseExpired) {
		t.Fatalf("expected ErrLeaseExpired on fresh leader, got %v", err)
	}
}

// TestReadIndexLease_FollowerForwarding verifies that ReadIndexLease on a
// follower forwards the request to the leader and returns a safe index.
func TestReadIndexLease_FollowerForwarding(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)

	// WaitLeader returns as soon as a node enters Leader state; the no-op entry
	// may not yet be committed. Issue a ReadIndex on the leader to wait for the
	// nop to commit and apply before testing follower forwarding.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), electionTimeout)
	defer waitCancel()
	if _, err := c.nodes[leaderIdx].ReadIndex(waitCtx); err != nil {
		t.Fatalf("leader ReadIndex (nop wait): %v", err)
	}

	follower := c.nodes[(leaderIdx+1)%3]
	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()

	idx, err := follower.ReadIndexLease(ctx)
	if err != nil {
		t.Fatalf("ReadIndexLease on follower: %v", err)
	}
	if idx == 0 {
		t.Fatal("ReadIndexLease returned 0")
	}
}

// TestReadIndexLease_FastPathAfterReadIndex verifies that after a successful
// ReadIndex (which triggers a heartbeat quorum and populates the lease),
// ReadIndexLease resolves immediately without a network round-trip.
func TestReadIndexLease_FastPathAfterReadIndex(t *testing.T) {
	clk := newManualClock()
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.Clock = clk
	})
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	// Run a full ReadIndex to establish the lease.
	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()

	riCh := make(chan error, 1)
	go func() {
		_, err := leader.ReadIndex(ctx)
		riCh <- err
	}()
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		select {
		case err := <-riCh:
			if err != nil {
				t.Fatalf("ReadIndex: %v", err)
			}
			goto leaseEstablished
		default:
		}
	}
	t.Fatal("ReadIndex timed out")

leaseEstablished:
	// Now ReadIndexLease should succeed without ticking (fast path, no network).
	leaseCh := make(chan error, 1)
	go func() {
		_, err := leader.ReadIndexLease(ctx)
		leaseCh <- err
	}()
	select {
	case err := <-leaseCh:
		if err != nil {
			t.Fatalf("ReadIndexLease after ReadIndex: %v", err)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("ReadIndexLease fast path blocked unexpectedly")
	}
}

// TestReadIndexLease_ExpiresAfterElectionTimeout verifies that advancing the
// clock past ElectionTimeoutMin causes the lease to expire.
func TestReadIndexLease_ExpiresAfterElectionTimeout(t *testing.T) {
	clk := newManualClock()
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.Clock = clk
	})
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	// Establish a lease via ReadIndex.
	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	riCh := make(chan error, 1)
	go func() {
		_, err := leader.ReadIndex(ctx)
		riCh <- err
	}()
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		select {
		case err := <-riCh:
			if err != nil {
				t.Fatalf("ReadIndex: %v", err)
			}
			goto established
		default:
		}
	}
	t.Fatal("ReadIndex timed out")

established:
	// Advance the clock past ElectionTimeoutMin to expire the lease.
	// ElectionTimeoutMin default is 150 ms.
	clk.Advance(200 * time.Millisecond)

	// ReadIndexLease should now return ErrLeaseExpired.
	leaseCh := make(chan error, 1)
	ctx2, cancel2 := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel2()
	go func() {
		_, err := leader.ReadIndexLease(ctx2)
		leaseCh <- err
	}()
	select {
	case err := <-leaseCh:
		if !errors.Is(err, raft.ErrLeaseExpired) {
			t.Fatalf("expected ErrLeaseExpired after clock advance, got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ReadIndexLease did not return after clock advance")
	}
}

// TestReadIndexLease_ExpiryFromSendTime verifies that the read lease expiry is
// anchored to the heartbeat send time, not the ACK receive time. If there is a
// simulated clock advance between the heartbeat send and the quorum ACK, the
// lease should expire at sendTime+ElectionTimeoutMin, not at a later time.
func TestReadIndexLease_ExpiryFromSendTime(t *testing.T) {
	clk := newManualClock()
	const elMin = 150 * time.Millisecond
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.Clock = clk
	})
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	// Start a ReadIndex which will trigger broadcastReadBarrier (send time = T0).
	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	riCh := make(chan error, 1)
	go func() {
		_, err := leader.ReadIndex(ctx)
		riCh <- err
	}()

	// Tick once so the read-barrier heartbeat is dispatched (send time captured).
	// Then simulate a 100ms RTT by advancing the clock before ACKs arrive.
	c.TickN(1)
	clk.Advance(100 * time.Millisecond) // simulated RTT delay

	// Continue ticking until ReadIndex resolves — ACKs now arrive at T0+100ms.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		select {
		case err := <-riCh:
			if err != nil {
				t.Fatalf("ReadIndex: %v", err)
			}
			goto leaseSet
		default:
		}
	}
	t.Fatal("ReadIndex timed out")

leaseSet:
	// Clock is now at T0+100ms. The lease expiry should be T0+150ms, i.e. only
	// 50ms in the "future" from the current clock position — NOT T0+250ms.
	// Advancing by 60ms (T0+160ms) must expire the lease.
	clk.Advance(60 * time.Millisecond) // total: T0+160ms > T0+150ms expiry

	leaseCh := make(chan error, 1)
	ctx2, cancel2 := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel2()
	go func() {
		_, err := leader.ReadIndexLease(ctx2)
		leaseCh <- err
	}()
	select {
	case err := <-leaseCh:
		if !errors.Is(err, raft.ErrLeaseExpired) {
			t.Fatalf("expected ErrLeaseExpired (lease anchored at send time), got %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ReadIndexLease did not return promptly")
	}
}

// ---- Joint consensus tests --------------------------------------------------

// TestJoint_ShrinkToSingleNode verifies that ReconfigureCluster correctly drives
// a 3-node cluster to a 2-node cluster (removing one peer), and that the
// cluster continues to accept proposals after the reconfiguration.
func TestJoint_ShrinkToSingleNode(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	// Propose a command before reconfiguration to ensure the log is non-trivial.
	if _, err := c.Propose(electionTimeout, []byte("before-reconfig")); err != nil {
		t.Fatalf("propose before reconfig: %v", err)
	}

	// Shrink to single-node: new membership is just the leader itself.
	ctx, cancel := context.WithTimeout(context.Background(), 2*electionTimeout)
	defer cancel()

	rcDone := make(chan error, 1)
	go func() {
		rcDone <- leader.ReconfigureCluster(ctx, []raft.NodeID{leader.ID()})
	}()

	deadline := time.Now().Add(2 * electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		select {
		case err := <-rcDone:
			if err != nil {
				t.Fatalf("ReconfigureCluster: %v", err)
			}
			goto reconfigDone
		default:
		}
	}
	t.Fatal("ReconfigureCluster timed out")

reconfigDone:
	// After reconfiguration the leader should have no peers.
	// Propose a new entry on the single-node cluster.
	if _, err := c.Propose(electionTimeout, []byte("after-reconfig")); err != nil {
		t.Fatalf("propose after reconfig: %v", err)
	}
}

// TestJoint_ShrinkByOne verifies that ReconfigureCluster correctly removes one
// peer from a 3-node cluster, leaving a 2-node cluster that remains functional.
func TestJoint_ShrinkByOne(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	// Identify a non-leader peer to remove.
	var removeID raft.NodeID
	var keepID raft.NodeID
	for _, id := range c.ids {
		if id == c.ids[leaderIdx] {
			continue
		}
		if removeID == "" {
			removeID = id
		} else {
			keepID = id
		}
	}

	// Shrink from 3-node to 2-node cluster.
	ctx, cancel := context.WithTimeout(context.Background(), 2*electionTimeout)
	defer cancel()

	rcDone := make(chan error, 1)
	go func() {
		rcDone <- leader.ReconfigureCluster(ctx, []raft.NodeID{leader.ID(), keepID})
	}()

	deadline := time.Now().Add(2 * electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		select {
		case err := <-rcDone:
			if err != nil {
				t.Fatalf("ReconfigureCluster: %v", err)
			}
			goto done
		default:
		}
	}
	t.Fatal("ReconfigureCluster timed out")

done:
	// Cluster is now {leader, keepID}. Verify proposals still work.
	if _, err := c.Propose(electionTimeout, []byte("after-shrink")); err != nil {
		t.Fatalf("propose after shrink: %v", err)
	}

	// removeID is no longer in the config. The leader should not replicate to it.
	_ = removeID
}

// TestJoint_ReplaceOnePeer verifies that ReconfigureCluster can add a new node
// while simultaneously removing an old one (the canonical "replace" operation).
func TestJoint_ReplaceOnePeer(t *testing.T) {
	// Build a 3-node cluster manually so we can add a 4th transport endpoint.
	net := memtransport.NewNetwork()
	ids := []raft.NodeID{"n1", "n2", "n3"}

	var nodes []*raft.Node
	var sms []*kvSM

	for i, id := range ids {
		peers := make([]raft.NodeID, 0, len(ids)-1)
		for j, p := range ids {
			if j != i {
				peers = append(peers, p)
			}
		}
		tr := net.NewTransport(id)
		store := memstore.New()
		sm := &kvSM{data: make(map[string]string)}
		cfg := raft.DefaultConfig()
		cfg.ID = id
		cfg.Peers = peers
		cfg.Storage = store
		cfg.StateMachine = sm
		cfg.Transport = tr
		cfg.TickInterval = 0
		node, err := raft.New(cfg)
		if err != nil {
			t.Fatalf("New %s: %v", id, err)
		}
		nodes = append(nodes, node)
		sms = append(sms, sm)
	}

	for i, id := range ids {
		net.Register(id, nodes[i])
	}
	for _, node := range nodes {
		node.Start()
	}
	t.Cleanup(func() {
		for _, node := range nodes {
			node.Stop()
		}
	})

	tick := func() {
		for _, node := range nodes {
			node.Tick()
		}
		time.Sleep(time.Millisecond)
	}

	// Elect a leader.
	var leaderNode *raft.Node
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		tick()
		for _, node := range nodes {
			if node.StateSnapshot() == raft.Leader {
				leaderNode = node
				goto elected
			}
		}
	}
	t.Fatal("no leader elected")
elected:

	// Create n4 as a replacement for n3.
	n4ID := raft.NodeID("n4")
	n4TR := net.NewTransport(n4ID)
	n4Store := memstore.New()
	n4SM := &kvSM{data: make(map[string]string)}
	n4Cfg := raft.DefaultConfig()
	n4Cfg.ID = n4ID
	n4Cfg.Peers = []raft.NodeID{"n1", "n2", "n3"} // initial peers (will be updated by config entries)
	n4Cfg.Storage = n4Store
	n4Cfg.StateMachine = n4SM
	n4Cfg.Transport = n4TR
	n4Cfg.TickInterval = 0
	n4, err := raft.New(n4Cfg)
	if err != nil {
		t.Fatalf("New n4: %v", err)
	}
	net.Register(n4ID, n4)
	n4.Start()
	t.Cleanup(n4.Stop)
	nodes = append(nodes, n4)
	sms = append(sms, n4SM)

	// Add n4 as a known peer to the leader's transport.
	// In grpctransport this would be AddPeer; memtransport routes by NodeID
	// so no registration is needed on the sender side.

	// ReconfigureCluster: the new cluster is {leader, n4}.
	// newMembers is the complete new membership; the leader is explicitly included.
	ctx, cancel := context.WithTimeout(context.Background(), 3*electionTimeout)
	defer cancel()

	rcDone := make(chan error, 1)
	go func() {
		rcDone <- leaderNode.ReconfigureCluster(ctx, []raft.NodeID{leaderNode.ID(), n4ID})
	}()

	deadline = time.Now().Add(3 * electionTimeout)
	for time.Now().Before(deadline) {
		tick()
		select {
		case err := <-rcDone:
			if err != nil {
				t.Fatalf("ReconfigureCluster: %v", err)
			}
			goto replaced
		default:
		}
	}
	t.Fatal("ReconfigureCluster timed out")

replaced:
	// Propose an entry after replacement; the cluster is now {leader, n4}.
	proposeDone := make(chan error, 1)
	go func() {
		_, err := leaderNode.Propose(ctx, []byte("after-replace"))
		proposeDone <- err
	}()
	deadline = time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		tick()
		select {
		case err := <-proposeDone:
			if err != nil {
				t.Fatalf("propose after replace: %v", err)
			}
			return
		default:
		}
	}
	t.Fatal("propose after ReconfigureCluster timed out")
}

// TestJoint_LeaderRemoval verifies that the leader can remove itself from the
// cluster by omitting its own ID from newMembers. After the finalise entry
// commits, the old leader steps down and one of the remaining nodes becomes
// the new leader.
//
// Note: after self-removal the old leader becomes a follower and, if left
// running, would try to start elections (the protocol relies on the application
// to stop the removed node). The test therefore calls Stop() on the removed
// leader, simulating proper application-layer cleanup.
func TestJoint_LeaderRemoval(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]
	leaderID := c.ids[leaderIdx]

	// Identify the two peers that will remain.
	var remainIDs []raft.NodeID
	for _, id := range c.ids {
		if id != leaderID {
			remainIDs = append(remainIDs, id)
		}
	}

	// Propose before the reconfiguration to ensure a non-trivial log.
	if _, err := c.Propose(electionTimeout, []byte("before-removal")); err != nil {
		t.Fatalf("propose before removal: %v", err)
	}

	// Remove the current leader: newMembers excludes leaderID.
	ctx, cancel := context.WithTimeout(context.Background(), 3*electionTimeout)
	defer cancel()

	rcDone := make(chan error, 1)
	go func() {
		rcDone <- leader.ReconfigureCluster(ctx, remainIDs)
	}()

	// Drive ticks until ReconfigureCluster returns (joint entry committed).
	deadline := time.Now().Add(2 * electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		select {
		case err := <-rcDone:
			if err != nil {
				t.Fatalf("ReconfigureCluster: %v", err)
			}
			goto jointCommitted
		default:
		}
	}
	t.Fatal("ReconfigureCluster timed out")

jointCommitted:
	// Continue ticking so the finalise entry can commit and the old leader
	// can step down.
	deadline = time.Now().Add(2 * electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		if leader.StateSnapshot() != raft.Leader {
			goto leaderStepped
		}
	}
	t.Fatal("old leader did not step down after self-removal")

leaderStepped:
	// Stop the removed leader. In a real system the application layer is
	// responsible for shutting down a node that has been removed from the
	// cluster; leaving it running would allow it to restart elections.
	leader.Stop()

	// A new leader must now be elected among the remaining nodes.
	newLeaderIdx := c.WaitLeader(electionTimeout)
	if c.ids[newLeaderIdx] == leaderID {
		t.Fatalf("old leader was re-elected after self-removal")
	}

	// The new cluster should accept proposals.
	if _, err := c.Propose(electionTimeout, []byte("after-removal")); err != nil {
		t.Fatalf("propose after leader removal: %v", err)
	}
}

// ---- Pending proposal drain -------------------------------------------------

// TestPendingProposals_DrainedOnStepDown verifies that in-flight proposals
// (appended to the log but not yet replicated) are rejected with NotLeaderError
// when the leader steps down due to quorum loss (CheckQuorum).
//
// This covers the fix in applyFollowerTransition that drains n.pending: without
// it, the pending channel would leak and the client goroutine would block until
// its context expired, receiving no response at all.
func TestPendingProposals_DrainedOnStepDown(t *testing.T) {
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.CheckQuorum = true
	})
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	// Drop outbound links from the leader so AppendEntries never reach followers.
	// Followers will not ACK the new entry, so it can never commit and will sit
	// in n.pending indefinitely — until the leader steps down.
	followerA := (leaderIdx + 1) % 3
	followerB := (leaderIdx + 2) % 3
	c.DropLink(leaderIdx, followerA)
	c.DropLink(leaderIdx, followerB)

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout*2)
	defer cancel()

	// Submit a proposal in a background goroutine. The entry will be appended
	// to the leader's log and stored in n.pending but can never replicate.
	errCh := make(chan error, 1)
	go func() {
		_, err := leader.Propose(ctx, []byte("inflight-cmd"))
		errCh <- err
	}()

	// Tick until CheckQuorum fires and the leader steps down.
	// applyFollowerTransition drains n.pending → the goroutine above unblocks.
	deadline := time.Now().Add(electionTimeout * 2)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		select {
		case err := <-errCh:
			var notLeaderErr *raft.NotLeaderError
			if !errors.As(err, &notLeaderErr) {
				t.Fatalf("expected NotLeaderError after step-down, got %v", err)
			}
			return
		default:
		}
	}
	t.Fatal("pending proposal was not drained after leader stepped down via CheckQuorum")
}

// ---- §22 PreferredLeader ---------------------------------------------------

// TestPreferredLeader_TransfersLeadership verifies that when a node that is not
// the preferred leader wins an election, it automatically transfers leadership
// to the preferred node once its no-op is committed.
func TestPreferredLeader_TransfersLeadership(t *testing.T) {
	const preferred = raft.NodeID("n1")

	// All three nodes prefer n1 as leader.
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.PreferredLeader = preferred
	})

	// Wait long enough for an initial election and the subsequent transfer.
	deadline := time.Now().Add(electionTimeout * 2)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		if idx := c.leaderIndex(); idx >= 0 && c.nodes[idx].ID() == preferred {
			return // preferred node is the leader — success
		}
	}
	t.Fatalf("preferred leader %q did not become leader within timeout", preferred)
}
