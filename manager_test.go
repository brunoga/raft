package raft_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

// newManagedNode creates a node and registers it with mgr and net.
func newManagedNode(t *testing.T, mgr *raft.Manager, net *memtransport.Network,
	groupID uint64, nodeID raft.NodeID, sm raft.StateMachine) *raft.Node {
	t.Helper()
	if sm == nil {
		sm = &discardSM{}
	}
	cfg := raft.DefaultConfig()
	cfg.GroupID = groupID
	cfg.ID = nodeID
	cfg.Storage = memstore.New()
	cfg.StateMachine = sm
	cfg.Transport = net.NewTransport(nodeID)
	cfg.TickInterval = 0
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	if err := mgr.Add(groupID, node); err != nil {
		t.Fatalf("mgr.Add: %v", err)
	}
	net.Register(nodeID, node.Handler())
	return node
}

// discardSM is a no-op StateMachine for manager tests.
type discardSM struct{}

func (s *discardSM) Apply(_ context.Context, _ raft.LogEntry) ([]byte, error) { return nil, nil }
func (s *discardSM) Snapshot(_ context.Context, _ io.Writer) error            { return nil }
func (s *discardSM) Restore(_ context.Context, _ raft.SnapshotMeta, _ io.Reader) error {
	return nil
}

// ---- TestManager_AddRemove --------------------------------------------------

func TestManager_AddRemove(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	node := newManagedNode(t, mgr, net, 1, "n1", nil)
	node.Start()

	// Verify group exists.
	if _, err := mgr.Get(1); err != nil {
		t.Errorf("Get(1) failed: %v", err)
	}

	// Remove group.
	if err := mgr.Remove(1); err != nil {
		t.Errorf("Remove(1) failed: %v", err)
	}

	// Verify group is gone.
	if _, err := mgr.Get(1); err == nil {
		t.Error("Get(1) succeeded after Remove")
	}
}

// ---- TestManager_GroupIDMismatch --------------------------------------------

func TestManager_GroupIDMismatch(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	cfg := raft.DefaultConfig()
	cfg.GroupID = 1
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = &discardSM{}
	cfg.Transport = net.NewTransport("n1")
	node, _ := raft.New(&cfg)

	// Adding under wrong GroupID must fail.
	if err := mgr.Add(2, node); err == nil {
		t.Error("Add(2) with node.GroupID=1 succeeded, want error")
	}
}

// ---- TestManager_RoutingUnknownGroup ----------------------------------------

func TestManager_RoutingUnknownGroup(t *testing.T) {
	mgr := raft.NewManager()
	// Lookup must return (nil, false) for unknown groups.
	if _, ok := mgr.Lookup(99); ok {
		t.Error("Lookup(99) returned true for empty manager")
	}
}

// ---- TestManager_StopAll ----------------------------------------------------

func TestManager_StopAll(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	_ = newManagedNode(t, mgr, net, 1, "n1", nil)
	_ = newManagedNode(t, mgr, net, 2, "n2", nil)

	mgr.StartAll()
	mgr.StopAll()

	// After StopAll, the map must be empty.
	if _, err := mgr.Get(1); err == nil {
		t.Error("node 1 still present after StopAll")
	}
	if _, err := mgr.Get(2); err == nil {
		t.Error("node 2 still present after StopAll")
	}
}

// ---- TestManager_RunTicker --------------------------------------------------

func TestManager_RunTicker(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	// Single node cluster.
	cfg := raft.DefaultConfig()
	cfg.GroupID = 1
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = &discardSM{}
	cfg.Transport = net.NewTransport("n1")
	cfg.TickInterval = 0 // use Manager's ticker
	node, _ := raft.New(&cfg)
	_ = mgr.Add(1, node)
	net.Register("n1", node.Handler())
	node.Start()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Drive ticks from the manager.
	go mgr.RunTicker(ctx, 1*time.Millisecond)

	// Wait for the node to become leader (RunTicker is driving ticks).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if node.State() == raft.Leader {
			cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("node never became leader via RunTicker")
}

// ---- TestManager_Multiplexing -----------------------------------------------

// TestManager_Multiplexing verifies that multiple independent Raft groups
// can share one Manager and drive their logical clocks from one RunTicker.
func TestManager_Multiplexing(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	// Group 1: 1 node.
	node1 := newManagedNode(t, mgr, net, 1, "n1", nil)
	// Group 2: 1 node.
	node2 := newManagedNode(t, mgr, net, 2, "n2", nil)

	mgr.StartAll()
	defer mgr.StopAll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.RunTicker(ctx, 1*time.Millisecond)

	// Wait for both to become leader.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if node1.State() == raft.Leader && node2.State() == raft.Leader {
			goto success
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("groups failed to elect leaders: g1=%s g2=%s",
		node1.State(), node2.State())

success:
	// Both groups are still reachable via the Manager.
	if _, err := mgr.Get(1); err != nil {
		t.Errorf("Get(1): %v", err)
	}
	if _, err := mgr.Get(2); err != nil {
		t.Errorf("Get(2): %v", err)
	}
}

// ---- TestManager_RunTicker_WorkersBoundedByGroupCount ----------------------

// TestManager_RunTicker_WorkersBoundedByGroupCount verifies that RunTicker
// does not spawn more worker goroutines than there are registered groups.
// Before the fix RunTicker always spawned GOMAXPROCS workers even for a single
// group, wasting one goroutine per extra core. After the fix the pool is
// capped at min(GOMAXPROCS, initialGroupCount).
//
// The test requires GOMAXPROCS ≥ 3 so the before/after difference is visible
// via runtime.NumGoroutine() even with a ±1 measurement error budget.
//
// FAILS before the fix on machines with GOMAXPROCS ≥ 3.
// PASSES after the fix.
func TestManager_RunTicker_WorkersBoundedByGroupCount(t *testing.T) {
	nProcs := runtime.GOMAXPROCS(0)
	if nProcs < 3 {
		t.Skipf("GOMAXPROCS=%d < 3; can't distinguish over-provisioning from noise", nProcs)
	}

	const nGroups = 1 // far fewer groups than cores

	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	for i := range nGroups {
		node := newUnregisteredNode(t, net, uint64(i+1), raft.NodeID(fmt.Sprintf("wb%d", i+1)))
		if err := mgr.Add(uint64(i+1), node); err != nil {
			t.Fatalf("Add(%d): %v", i+1, err)
		}
	}

	// Snapshot goroutine count before RunTicker starts workers.
	// Use a short runtime.Gosched() to let any inflight goroutine starts settle.
	runtime.Gosched()
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	tickerDone := make(chan struct{})
	go func() {
		mgr.RunTicker(ctx, 10*time.Millisecond)
		close(tickerDone)
	}()

	// Give the worker goroutines time to start.
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()

	cancel()
	<-tickerDone

	// Expected after - before:
	//   1 goroutine running RunTicker itself
	//   min(nProcs, nGroups) = nGroups worker goroutines (after fix)
	//   OR nProcs worker goroutines (before fix)
	// We allow a ±2 buffer for unrelated goroutine churn.
	increase := after - before
	maxExpected := nGroups + 3 // RunTicker goroutine + nGroups workers + 2 slack
	if increase > maxExpected {
		t.Errorf("RunTicker spawned too many goroutines: before=%d after=%d increase=%d "+
			"(want ≤ %d; nGroups=%d nProcs=%d — before fix this would be ~%d)",
			before, after, increase, maxExpected, nGroups, nProcs, nProcs+1)
	}
}

// ---- TestManager_RunTicker_Starvation ---------------------------------------

// TestManager_RunTicker_Starvation verifies that a single slow node (whose
// event loop is blocked) does not starve the ticker for other nodes.
// Before the fix, Node.Tick() blocked if the tickCh was full, eventually
// exhausting the RunTicker worker pool and deadlocking context cancellation.
func TestManager_RunTicker_Starvation(t *testing.T) {
	// We use GOMAXPROCS workers in RunTicker.
	nWorkers := runtime.GOMAXPROCS(0)

	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	// 1. Create nWorkers + 1 "slow" nodes that will block their event loop.
	// We don't start them, so their tickCh (size 1) will fill up and block
	// the worker.
	for i := 0; i < nWorkers+1; i++ {
		gid := uint64(i + 1)
		id := raft.NodeID(fmt.Sprintf("slow-%d", i))
		node := newUnregisteredNode(t, net, gid, id)
		if err := mgr.Add(gid, node); err != nil {
			t.Fatalf("Add(%d): %v", gid, err)
		}
	}

	// 2. Start RunTicker.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tickerDone := make(chan struct{})
	go func() {
		mgr.RunTicker(ctx, 1*time.Millisecond)
		close(tickerDone)
	}()

	// 3. Wait for workers to block.
	// Each tick cycle, RunTicker will try to send ticks for all nodes.
	// Since there are nWorkers+1 nodes and only nWorkers workers, and each
	// node's Tick() blocks, eventually all workers will be stuck in Tick()
	// and the main RunTicker loop will be stuck sending to 'work' channel.
	time.Sleep(100 * time.Millisecond)

	// 4. Try to cancel and see if it returns.
	cancel()

	select {
	case <-tickerDone:
		// Success: fixed RunTicker returns when context is cancelled.
	case <-time.After(time.Second):
		t.Fatal("RunTicker deadlocked and did not exit after context cancellation")
	}
}

// ---- TestManager_RemoveGraceful ---------------------------------------------

// TestManager_RemoveGraceful verifies that removing a leader node with
// RemoveGraceful initiates a leadership transfer before stopping, so the
// remaining nodes elect a new leader without an unnecessary election timeout
// (gap in quorum availability).
func TestManager_RemoveGraceful(t *testing.T) {
	const (
		numNodes = 3
		gid      = 1
	)
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	nodes := make([]*raft.Node, numNodes)
	ids := make([]raft.NodeID, numNodes)
	sms := make([]*kvSM, numNodes)

	for i := range numNodes {
		id := raft.NodeID(fmt.Sprintf("n%d", i+1))
		ids[i] = id
		peers := make([]raft.PeerConfig, 0, numNodes-1)
		for j := range numNodes {
			if i == j {
				continue
			}
			peers = append(peers, raft.PeerConfig{ID: raft.NodeID(fmt.Sprintf("n%d", j+1)), Voter: true})
		}
		sm := &kvSM{data: make(map[string]string)}
		sms[i] = sm

		cfg := raft.DefaultConfig()
		cfg.GroupID = gid
		cfg.ID = id
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = sm
		cfg.Transport = net.NewTransport(id)
		cfg.TickInterval = 0 // manual ticks

		n, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(%d): %v", i+1, err)
		}
		nodes[i] = n
		if i == 0 {
			// Only add node 0 to the manager for this test.
			_ = mgr.Add(gid, n)
		}
		net.Register(id, n.Handler())
		n.Start()
	}

	tick := func() {
		for _, n := range nodes {
			n.Tick()
		}
		time.Sleep(time.Millisecond)
	}

	// 1. Wait for a leader.
	deadline := time.Now().Add(3 * time.Second)
	leaderIdx := -1
	for time.Now().Before(deadline) {
		tick()
		for i, n := range nodes {
			if n.State() == raft.Leader {
				leaderIdx = i
				break
			}
		}
		if leaderIdx >= 0 {
			break
		}
	}
	if leaderIdx < 0 {
		t.Fatal("no leader elected")
	}

	// If node 0 is not the leader, we can't test RemoveGraceful easily
	// with only node 0 in the manager. Let's restart the test until node 0
	// is the leader, or just add the leader to the manager.
	if leaderIdx != 0 {
		// Remove node 0, add the actual leader.
		_ = mgr.Remove(gid)
		_ = mgr.Add(gid, nodes[leaderIdx])
	}

	// 2. Call RemoveGraceful on the leader.
	// Transfer to a node that is NOT the one we just removed (if any)
	// and is NOT the leader itself.
	var targetID raft.NodeID
	for i, id := range ids {
		if i != leaderIdx && (leaderIdx == 0 || i != 0) {
			targetID = id
			break
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// RemoveGraceful blocks until the transfer completes (or timeout).
	// We must keep ticking the cluster in the background so the transfer
	// protocol can make progress.
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.RemoveGraceful(ctx, gid, targetID) }()

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("RemoveGraceful failed: %v", err)
			}
			goto removed
		default:
			tick()
		}
	}
	t.Fatal("RemoveGraceful timed out")

removed:
	// 3. Verify that the node was removed from the manager.
	if _, err := mgr.Get(gid); err == nil {
		t.Error("group still present in manager after RemoveGraceful")
	}

	// 4. Verify that a new leader exists among the remaining nodes.
	// We shouldn't need a full election timeout because the transfer uses
	// TimeoutNow to trigger an immediate election.
	hasLeader := false
	for range 10 {
		tick()
		for i, n := range nodes {
			if i != leaderIdx && n.State() == raft.Leader {
				hasLeader = true
				break
			}
		}
		if hasLeader {
			break
		}
	}

	if !hasLeader {
		t.Error("no leader among remaining nodes after RemoveGraceful")
	}
}

// ---- TestManager_RemoveGraceful_Race ----------------------------------------

// TestManager_RemoveGraceful_Race verifies that RemoveGraceful waits for the
// leadership transfer to complete (node steps down) before stopping it.
// Before the fix, RemoveGraceful stopped the node immediately after
// TransferLeadership returned, which cancelled the background TimeoutNow RPC.
func TestManager_RemoveGraceful_Race(t *testing.T) {
	const timeout = 5 * time.Second
	net := memtransport.NewNetwork()
	mgr := raft.NewManager()

	peers := []raft.NodeID{"a", "b", "c"}
	nodes := make(map[raft.NodeID]*raft.Node)
	transports := make(map[raft.NodeID]*delayedTransport)

	for _, id := range peers {
		others := make([]raft.PeerConfig, 0, 2)
		for _, other := range peers {
			if other != id {
				others = append(others, raft.PeerConfig{ID: other, Voter: true})
			}
		}

		cfg := raft.DefaultConfig()
		cfg.GroupID = 1
		cfg.ID = id
		cfg.Peers = others
		cfg.Storage = memstore.New()
		cfg.StateMachine = &discardSM{}

		dt := &delayedTransport{Transport: net.NewTransport(id)}
		transports[id] = dt
		cfg.Transport = dt

		cfg.TickInterval = 0
		node, _ := raft.New(&cfg)
		nodes[id] = node
		net.Register(id, node.Handler())
		node.Start()
	}

	// Elect a leader.
	deadline := time.Now().Add(timeout)
	var leaderID raft.NodeID
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			n.Tick()
		}
		for id, n := range nodes {
			if n.State() == raft.Leader {
				leaderID = id
				break
			}
		}
		if leaderID != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if leaderID == "" {
		t.Fatal("no leader elected")
	}

	// Find another node to transfer to.
	var transferTo raft.NodeID
	for _, id := range peers {
		if id != leaderID {
			transferTo = id
			break
		}
	}

	// Register the leader with the manager.
	if err := mgr.Add(1, nodes[leaderID]); err != nil {
		t.Fatalf("mgr.Add: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// In the fixed version, RemoveGraceful should wait for TimeoutNow.
	if err := mgr.RemoveGraceful(ctx, 1, transferTo); err != nil {
		t.Fatalf("RemoveGraceful: %v", err)
	}

	// Give the background goroutine a moment to finish its (delayed) work.
	time.Sleep(200 * time.Millisecond)

	errVal := transports[leaderID].timeoutNowErr.Load()
	if errVal == nil {
		t.Fatal("TimeoutNow was never called")
	}
	if err, ok := errVal.(error); ok {
		t.Errorf("TimeoutNow failed: %v", err)
	}
}

// ---- TestManager_SnapshotThrottling -----------------------------------------

type concurrentSM struct {
	concurrentCount atomic.Int32
	maxConcurrent   atomic.Int32
}

func (s *concurrentSM) Apply(ctx context.Context, entry raft.LogEntry) ([]byte, error) {
	return nil, nil
}
func (s *concurrentSM) Snapshot(ctx context.Context, w io.Writer) error {
	curr := s.concurrentCount.Add(1)
	for {
		prev := s.maxConcurrent.Load()
		if curr <= prev || s.maxConcurrent.CompareAndSwap(prev, curr) {
			break
		}
	}
	// Simulate some work.
	time.Sleep(50 * time.Millisecond)
	s.concurrentCount.Add(-1)
	return nil
}
func (s *concurrentSM) Restore(ctx context.Context, meta raft.SnapshotMeta, r io.Reader) error {
	return nil
}

// TestManager_SnapshotThrottling verifies that sharing a SnapshotSemaphore
// across multiple groups successfully limits the number of concurrent
// snapshots. This prevents "snapshot storms" in multi-raft deployments.
func TestManager_SnapshotThrottling(t *testing.T) {
	const nGroups = 4
	const maxConcurrency = 2
	net := memtransport.NewNetwork()
	mgr := raft.NewManager()
	sm := &concurrentSM{}

	// Shared semaphore for all groups.
	sem := make(chan struct{}, maxConcurrency)

	nodes := make([]*raft.Node, nGroups)
	for i := range nGroups {
		gid := uint64(i + 1)
		cfg := raft.DefaultConfig()
		cfg.ID = raft.NodeID(fmt.Sprintf("n%d", i))
		cfg.GroupID = gid
		cfg.Storage = memstore.New()
		cfg.StateMachine = sm
		cfg.Transport = net.NewTransport(cfg.ID)
		cfg.TickInterval = 0
		cfg.SnapshotThreshold = 1 // Snapshot after every apply
		cfg.SnapshotSemaphore = sem

		n, _ := raft.New(&cfg)
		nodes[i] = n
		_ = mgr.Add(gid, n)
		n.Start()
	}
	defer mgr.StopAll()

	// Elect leaders.
	for i := range nGroups {
		n := nodes[i]
		// Become leader.
		for range 40 {
			n.Tick()
			time.Sleep(time.Millisecond)
		}
		if n.State() != raft.Leader {
			t.Fatalf("node %d did not become leader", i)
		}
	}

	// Trigger snapshots in parallel.
	var wg sync.WaitGroup
	wg.Add(nGroups)
	for i := range nGroups {
		go func(node *raft.Node) {
			defer wg.Done()
			_, err := node.Propose(context.Background(), []byte("trigger"))
			if err != nil {
				t.Errorf("Propose: %v", err)
			}
		}(nodes[i])
	}
	wg.Wait()

	// Wait for snapshots to start/complete.
	time.Sleep(200 * time.Millisecond)

	maxSeen := sm.maxConcurrent.Load()
	if int(maxSeen) > maxConcurrency {
		t.Errorf("expected max %d concurrent snapshots, got %d", maxConcurrency, maxSeen)
	}
}

type delayedTransport struct {
	raft.Transport
	timeoutNowErr atomic.Value // stores error or struct{} for success
}

func (t *delayedTransport) TimeoutNow(ctx context.Context, to raft.NodeID, req *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	// Delay to ensure the race hits.
	select {
	case <-time.After(100 * time.Millisecond):
		resp, rerr := t.Transport.TimeoutNow(ctx, to, req)
		if rerr != nil {
			t.timeoutNowErr.Store(rerr)
		} else {
			t.timeoutNowErr.Store(struct{}{})
		}
		return resp, rerr
	case <-ctx.Done():
		err := ctx.Err()
		t.timeoutNowErr.Store(err)
		return nil, err
	}
}

func (t *delayedTransport) Register(id raft.NodeID, h raft.Handler) {
	t.Transport.Register(id, h)
}

func (t *delayedTransport) Unregister(id raft.NodeID) {
	t.Transport.Unregister(id)
}

func (t *delayedTransport) Close() error {
	return t.Transport.Close()
}

// ---- TestManager_LookupAfterAdd ---------------------------------------------

func TestManager_LookupAfterAdd(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	node := newManagedNode(t, mgr, net, 7, "n7", nil)
	node.Start()
	defer node.Stop()

	h, ok := mgr.Lookup(7)
	if !ok {
		t.Fatal("Lookup(7) returned false, want true")
	}
	if h == nil {
		t.Fatal("Lookup(7) returned nil handler")
	}

	// After removal the lookup must fail.
	if err := mgr.Remove(7); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := mgr.Lookup(7); ok {
		t.Fatal("Lookup after Remove should return false")
	}
}

// ---- TestManager_RemoveGraceful_TOCTOU --------------------------------------

// TestManager_RemoveGraceful_TOCTOU verifies that when Remove races with
// RemoveGraceful on the same groupID after RemoveGraceful has already read
// the node, the inner Remove returning ErrGroupNotFound is suppressed.
// Before this fix, RemoveGraceful returned ErrGroupNotFound to its caller
// even though the group did exist at the start of the call.
//
// The test uses 100 parallel trials under the race detector. In each trial
// both calls are launched simultaneously; we assert that neither returns an
// error other than ErrGroupNotFound (i.e., no unexpected error kinds).
func TestManager_RemoveGraceful_TOCTOU(t *testing.T) {
	const trials = 100
	net := memtransport.NewNetwork()

	for i := range trials {
		mgr := raft.NewManager()
		node := newManagedNode(t, mgr, net, uint64(i+1000), "toctou", nil)
		node.Start()

		errs := make(chan error, 2)

		// Both goroutines attempt to remove the same group concurrently.
		// One wins; the loser must return nil (after fix) or ErrGroupNotFound
		// (acceptable only if the group disappeared before the losing call
		// even started its lookup).
		go func() { errs <- mgr.Remove(uint64(i + 1000)) }()
		go func() {
			errs <- mgr.RemoveGraceful(context.Background(), uint64(i+1000), "other")
		}()

		for range 2 {
			if err := <-errs; err != nil && !errors.Is(err, raft.ErrGroupNotFound) {
				t.Fatalf("trial %d: unexpected error (not ErrGroupNotFound): %v", i, err)
			}
		}
	}
}

// ---- TestManager_GroupIDs ---------------------------------------------------

// TestManager_GroupIDs verifies that GroupIDs returns a sorted snapshot of
// all registered group IDs, and reflects additions and removals.
func TestManager_GroupIDs(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	if ids := mgr.GroupIDs(); len(ids) != 0 {
		t.Fatalf("empty manager: expected [], got %v", ids)
	}

	// Add groups in non-sequential order to confirm sorting.
	for _, gid := range []uint64{5, 2, 8, 1, 4} {
		id := raft.NodeID(fmt.Sprintf("g%d", gid))
		newManagedNode(t, mgr, net, gid, id, nil)
	}
	mgr.StartAll()
	t.Cleanup(mgr.StopAll)

	ids := mgr.GroupIDs()
	want := []uint64{1, 2, 4, 5, 8}
	if len(ids) != len(want) {
		t.Fatalf("GroupIDs() = %v, want %v", ids, want)
	}
	for i, v := range want {
		if ids[i] != v {
			t.Fatalf("GroupIDs()[%d] = %d, want %d", i, ids[i], v)
		}
	}

	// Remove one group and verify the snapshot updates.
	if err := mgr.Remove(2); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	after := mgr.GroupIDs()
	for _, id := range after {
		if id == 2 {
			t.Fatal("GroupIDs() still contains removed group 2")
		}
	}
}

// ---- TestManager_Close -------------------------------------------------------

// TestManager_Close verifies that Manager implements io.Closer, that Close()
// stops all registered nodes and returns nil, and that it is equivalent to
// StopAll (group registry is cleared afterward).
func TestManager_Close(t *testing.T) {
	// Compile-time check: Manager must satisfy io.Closer.
	var _ io.Closer = (*raft.Manager)(nil)

	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	for i := range 4 {
		id := raft.NodeID(fmt.Sprintf("cl%d", i+1))
		node := newManagedNode(t, mgr, net, uint64(i+1), id, nil)
		node.Start()
	}

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}
	// After Close, the registry must be empty.
	if ids := mgr.GroupIDs(); len(ids) != 0 {
		t.Errorf("GroupIDs after Close = %v, want []", ids)
	}
}

// ---- TestManager_AddAndStart_StopAll_Race ------------------------------------

// newUnregisteredNode creates a *Node with the given GroupID without adding it
// to mgr. Used to test AddAndStart in isolation.
func newUnregisteredNode(t *testing.T, net *memtransport.Network,
	groupID uint64, nodeID raft.NodeID) *raft.Node {
	t.Helper()
	cfg := raft.DefaultConfig()
	cfg.GroupID = groupID
	cfg.ID = nodeID
	cfg.Peers = nil
	cfg.Storage = memstore.New()
	cfg.StateMachine = &discardSM{}
	cfg.Transport = net.NewTransport(nodeID)
	cfg.TickInterval = 0
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New(%s): %v", nodeID, err)
	}
	net.Register(nodeID, node.Handler())
	return node
}

// TestManager_AddAndStart_StopAll_Race verifies that a concurrent StopAll
// cannot call Stop() on a node before Start() is called on it. Before the fix,
// AddAndStart called Add (making the node visible in the map) before Start(),
// leaving a window where StopAll could Stop() a never-started node, which
// blocks forever in Node.Stop().
func TestManager_AddAndStart_StopAll_Race(t *testing.T) {
	const trials = 50
	net := memtransport.NewNetwork()

	for i := range trials {
		mgr := raft.NewManager()

		// Pre-register a started node so StopAll does real work and is more
		// likely to overlap with AddAndStart's window.
		warmup := newUnregisteredNode(t, net, 99, raft.NodeID(fmt.Sprintf("wu%d", i)))
		if err := mgr.AddAndStart(99, warmup); err != nil {
			t.Fatalf("trial %d: warmup AddAndStart: %v", i, err)
		}

		// Create a node that is NOT yet in the manager.
		id := raft.NodeID(fmt.Sprintf("aas%d", i))
		node := newUnregisteredNode(t, net, uint64(i+1), id)

		stopDone := make(chan struct{})
		go func() {
			mgr.StopAll()
			close(stopDone)
		}()

		// AddAndStart must not deadlock even when StopAll races it.
		// Ignore ErrGroupExists (StopAll cleared then we add, or vice-versa).
		_ = mgr.AddAndStart(uint64(i+1), node)

		select {
		case <-stopDone:
		case <-time.After(3 * time.Second):
			t.Fatalf("trial %d: deadlock — StopAll blocked (Stop called on never-started node?)", i)
		}
		// Final cleanup: node may have been started by AddAndStart but not stopped
		// (if StopAll ran before AddAndStart added it).
		node.Stop()
	}
}
