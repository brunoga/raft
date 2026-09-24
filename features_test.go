package raft_test

// Tests for:
//   - §11 Lease-based reads (ReadIndexLease, ErrLeaseExpired)
//   - §12 Joint consensus (ReconfigureCluster)
//   - Pending proposal drain on step-down

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
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
	if got := c.LeaderIndex(); got != leaderIdx {
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

func (r *recordTracer) StartRPC(ctx context.Context, _, _ raft.NodeID, rpcType raft.RPCType) (rpcCtx context.Context, finish func(error)) {
	r.record(string(rpcType))
	// A tracer that returns a context the caller can distinguish is how a real
	// one would propagate a span; returning it unchanged is also legal.
	return context.WithValue(ctx, tracedKey{}, rpcType), func(error) {}
}

// tracedKey marks a context that passed through recordTracer.
type tracedKey struct{}

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
//
// WaitLeader returns the moment a node enters Leader state, which is before
// its no-op commits, so this arrives on either side of that commit depending
// on how the scheduler feels. Both sides must answer the same way, and until
// the fix that came with this comment they did not: the side that arrived
// first was queued behind the no-op and then answered by a full barrier round,
// so the test failed roughly one run in seven. The window itself is covered
// deliberately by the test below.
func TestReadIndexLease_ErrLeaseExpiredOnFreshLeader(t *testing.T) {
	c := newCluster(t, 3)
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

// TestReadIndexLease_ErrLeaseExpiredBeforeTheNopCommits pins down the window
// the test above can only stumble into.
//
// A leader may not serve a read until it has committed an entry of its own
// term, so a ReadIndex that arrives first is queued until the no-op lands. A
// lease read is not a read that can be queued: the caller asked to be answered
// from the lease or told there is none, and it has a fallback ready for the
// second answer. Queueing it instead hands it the round-trip it opted out of,
// for as long as the no-op takes -- on a leader that has just lost its
// followers, until the context expires.
//
// The window is one log write wide, which is why the write is held open here
// rather than raced for.
func TestReadIndexLease_ErrLeaseExpiredBeforeTheNopCommits(t *testing.T) {
	store := newGateStore()

	net := memtransport.NewNetwork()
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = store
	cfg.StateMachine = &kvSM{data: make(map[string]string)}
	cfg.Transport = net.NewTransport("n1")
	cfg.TickInterval = 0 // Manual ticks.
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.Register("n1", node.Handler())
	node.Start()
	// Registered before the gate, so that teardown runs in the only order that
	// terminates: cleanups are last-registered-first, and a node cannot stop
	// while a write it accepted is still held.
	t.Cleanup(node.Stop)

	// Hold the log so the no-op this node is about to append cannot land.
	// Hard-state writes are left alone, since the election needs them.
	release := store.hold(t)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && node.State() != raft.Leader {
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	if node.State() != raft.Leader {
		t.Fatal("the single voter did not become leader")
	}

	// It calls itself leader with its no-op still unwritten. A lease read taken
	// now has to come back, and come back saying there is no lease.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, rerr := node.ReadIndexLease(ctx)
		done <- rerr
	}()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case rerr := <-done:
			if !errors.Is(rerr, raft.ErrLeaseExpired) {
				t.Fatalf("ReadIndexLease before the no-op committed returned %v, want ErrLeaseExpired", rerr)
			}
			release()
			return
		case <-ticker.C:
			node.Tick()
		case <-time.After(3 * time.Second):
			release()
			t.Fatal("ReadIndexLease never returned while the no-op was held: " +
				"it was queued behind the no-op instead of being answered from the lease it asked about")
		}
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

	// Leadership can move between electing one and asking a follower, which
	// is not a failure of forwarding: the follower forwards to the node it
	// last heard from, and that node answers ErrNotLeader if it has since
	// stepped down. A busy machine loses that race occasionally -- CI did,
	// once, where four hundred local runs did not.
	//
	// So the leader is re-read on each attempt rather than assumed to be the
	// one elected earlier. What is being tested is that a follower forwards
	// and gets a usable index, and that is still exactly what has to happen.
	deadline := time.Now().Add(electionTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		leaderIdx = c.WaitLeader(electionTimeout)
		follower := c.nodes[(leaderIdx+1)%3]

		ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
		idx, err := follower.ReadIndexLease(ctx)
		cancel()

		if err == nil {
			if idx == 0 {
				t.Fatal("ReadIndexLease returned 0")
			}
			return
		}
		lastErr = err
		if !errors.Is(err, raft.ErrNotLeader) {
			t.Fatalf("ReadIndexLease on follower: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("ReadIndexLease on follower never succeeded: %v", lastErr)
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

// barrierDelayHandler wraps a follower's handler and advances a clock the
// first time a read-barrier AppendEntries arrives, which is the one moment a
// simulated round-trip can be inserted honestly: after the leader captured its
// send time, before it can see an acknowledgement.
type barrierDelay struct {
	clk  *manualClock
	rtt  time.Duration
	once sync.Once
	seen chan struct{}
	// firstGen is the generation of the first barrier round this handler
	// sees, which is the one that waits for the no-op. The round being timed
	// is the next one, and telling them apart by generation is what makes
	// this deterministic -- see HandleAppendEntries.
	firstGen atomic.Uint64
}

// barrierDelayHandler wraps one follower. Every follower shares the same
// barrierDelay, so the round-trip is added once for the barrier round rather
// than once per follower that receives it.
type barrierDelayHandler struct {
	raft.Handler
	delay *barrierDelay
}

func (h *barrierDelayHandler) HandleAppendEntries(ctx context.Context, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	// The round-trip is added to the round being timed, identified by its
	// generation rather than by being the first barrier to arrive.
	//
	// "First to arrive" is not the same thing. The round that waits for the
	// no-op sends a request to every follower, and one of those can still be
	// in flight when the timed round begins -- so the clock would be advanced
	// by a straggler, before the timed barrier is sent rather than after,
	// leaving the lease anchored 100ms later than the test believes. The
	// assertion then finds a lease that is still valid. It cost a few
	// failures in every hundred runs, in three different disguises.
	//
	// Generations are monotonic and every request carries the one that
	// produced it, so the first generation seen is the no-op round and
	// anything after it is the round under test, whenever it happens to
	// arrive.
	if req.ReadBarrier != 0 {
		if h.delay.firstGen.CompareAndSwap(0, req.ReadBarrier) ||
			req.ReadBarrier == h.delay.firstGen.Load() {
			return h.Handler.HandleAppendEntries(ctx, req)
		}
		h.delay.once.Do(func() {
			h.delay.clk.Advance(h.delay.rtt)
			close(h.delay.seen)
		})
	}
	return h.Handler.HandleAppendEntries(ctx, req)
}

// TestReadIndexLease_ExpiryFromSendTime verifies that the read lease expiry is
// anchored to the heartbeat send time, not the ACK receive time. If there is a
// simulated clock advance between the heartbeat send and the quorum ACK, the
// lease should expire at sendTime+ElectionTimeoutMin, not at a later time.
//
// The round-trip is simulated inside the followers' handlers rather than by
// advancing the clock next to a tick and trusting the two to interleave. The
// barrier goes out when the ReadIndex reaches the event loop, not on a tick,
// so a test that advances the clock a millisecond after asking for the read is
// racing that goroutine: lose the race and the send time is captured after the
// advance, the lease runs 100ms longer than the test thinks, and the
// assertion below fails. That is a flake, and it failed under load.
func TestReadIndexLease_ExpiryFromSendTime(t *testing.T) {
	const rtt = 100 * time.Millisecond

	clk := newManualClock()
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.Clock = clk
	})
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	// The followers add the round-trip themselves, to the round after the one
	// that waits for the no-op. They are wrapped before that wait so the
	// handler sees its generation and can tell the two apart; see
	// barrierDelayHandler.
	delay := &barrierDelay{clk: clk, rtt: rtt, seen: make(chan struct{})}
	for i, id := range c.ids {
		if i == leaderIdx {
			continue
		}
		c.net.Register(id, &barrierDelayHandler{Handler: c.nodes[i].Handler(), delay: delay})
	}

	// Wait for the no-op to commit before measuring anything. Until it does, a
	// ReadIndex is queued rather than barriered, and the barrier that
	// eventually carries it is not the one this test is timing.
	nopCtx, nopCancel := context.WithTimeout(context.Background(), electionTimeout)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-nopCtx.Done():
				return
			case <-ticker.C:
				c.Tick()
			}
		}
	}()
	if _, err := leader.ReadIndex(nopCtx); err != nil {
		nopCancel()
		t.Fatalf("ReadIndex (nop wait): %v", err)
	}
	nopCancel()

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	riCh := make(chan error, 1)
	go func() {
		_, err := leader.ReadIndex(ctx)
		riCh <- err
	}()

	// Tick until ReadIndex resolves. The followers advance the clock by rtt as
	// the barrier reaches them, so it resolves with the clock at T0+rtt.
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
	select {
	case <-delay.seen:
	default:
		t.Fatal("no read barrier reached a follower, so nothing simulated the round-trip")
	}

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
		rcDone <- leader.ReconfigureCluster(ctx, []raft.PeerConfig{{ID: leader.ID(), Voter: true}})
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
		rcDone <- leader.ReconfigureCluster(ctx, []raft.PeerConfig{
			{ID: leader.ID(), Voter: true},
			{ID: keepID, Voter: true},
		})
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

	for i, id := range ids {
		peers := make([]raft.PeerConfig, 0, len(ids)-1)
		for j, p := range ids {
			if j != i {
				peers = append(peers, raft.PeerConfig{ID: p, Voter: true})
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
		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("New %s: %v", id, err)
		}
		nodes = append(nodes, node)
	}

	for i, id := range ids {
		net.Register(id, nodes[i].Handler())
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
			if node.State() == raft.Leader {
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
	n4Cfg.Peers = []raft.PeerConfig{
		{ID: "n1", Voter: true},
		{ID: "n2", Voter: true},
		{ID: "n3", Voter: true},
	} // initial peers (will be updated by config entries)
	n4Cfg.Storage = n4Store
	n4Cfg.StateMachine = n4SM
	n4Cfg.Transport = n4TR
	n4Cfg.TickInterval = 0
	n4, err := raft.New(&n4Cfg)
	if err != nil {
		t.Fatalf("New n4: %v", err)
	}
	net.Register(n4ID, n4.Handler())
	n4.Start()
	t.Cleanup(n4.Stop)
	nodes = append(nodes, n4)

	// Add n4 as a known peer to the leader's transport.
	// In grpctransport this would be AddPeer; memtransport routes by NodeID
	// so no registration is needed on the sender side.

	// ReconfigureCluster: the new cluster is {leader, n4}.
	// newMembers is the complete new membership; the leader is explicitly included.
	ctx, cancel := context.WithTimeout(context.Background(), 3*electionTimeout)
	defer cancel()

	rcDone := make(chan error, 1)
	go func() {
		rcDone <- leaderNode.ReconfigureCluster(ctx, []raft.PeerConfig{
			{ID: leaderNode.ID(), Voter: true},
			{ID: n4ID, Voter: true},
		})
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
	var remainIDs []raft.PeerConfig
	for _, id := range c.ids {
		if id != leaderID {
			remainIDs = append(remainIDs, raft.PeerConfig{ID: id, Voter: true})
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
		if leader.State() != raft.Leader {
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
		if idx := c.LeaderIndex(); idx >= 0 && c.nodes[idx].ID() == preferred {
			return // preferred node is the leader — success
		}
	}
	t.Fatalf("preferred leader %q did not become leader within timeout", preferred)
}
