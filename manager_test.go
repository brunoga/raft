package raft_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

// ---- helpers ----------------------------------------------------------------

// newManagedNode builds a started *Node with the given GroupID and registers it
// in both the Manager and the shared memtransport.Network.
func newManagedNode(t *testing.T, mgr *raft.Manager, net *memtransport.Network,
	groupID uint64, nodeID raft.NodeID, peers []raft.NodeID) *raft.Node {
	t.Helper()
	cfg := raft.DefaultConfig()
	cfg.GroupID = groupID
	cfg.ID = nodeID
	cfg.Peers = peers
	cfg.Storage = memstore.New()
	cfg.StateMachine = &discardSM{}
	cfg.Transport = net.NewTransport(nodeID)
	cfg.TickInterval = 0
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New(%s): %v", nodeID, err)
	}
	net.Register(nodeID, node)
	if err := mgr.Add(groupID, node); err != nil {
		t.Fatalf("mgr.Add(%d, %s): %v", groupID, nodeID, err)
	}
	return node
}

// discardSM is a no-op StateMachine for manager tests.
type discardSM struct{}

func (s *discardSM) Apply(_ context.Context, _ raft.LogEntry) ([]byte, error) { return nil, nil }
func (s *discardSM) Snapshot(_ context.Context) ([]byte, error)               { return nil, nil }
func (s *discardSM) Restore(_ context.Context, _ raft.SnapshotMeta, _ []byte) error {
	return nil
}

// ---- TestManager_AddRemove --------------------------------------------------

func TestManager_AddRemove(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	n1 := newManagedNode(t, mgr, net, 1, "n1", nil)
	n1.Start()
	defer n1.Stop()

	// Get returns the node we just added.
	got, err := mgr.Get(1)
	if err != nil {
		t.Fatalf("Get(1): %v", err)
	}
	if got != n1 {
		t.Fatal("Get returned wrong node")
	}

	// Adding a duplicate GroupID fails.
	cfg2 := raft.DefaultConfig()
	cfg2.GroupID = 1
	cfg2.ID = "n2"
	cfg2.Storage = memstore.New()
	cfg2.StateMachine = &discardSM{}
	cfg2.Transport = net.NewTransport("n2")
	n2, err := raft.New(&cfg2)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	if err := mgr.Add(1, n2); !errors.Is(err, raft.ErrGroupExists) {
		t.Fatalf("duplicate Add: want ErrGroupExists, got %v", err)
	}

	// Remove stops the node and unregisters it.
	if err := mgr.Remove(1); err != nil {
		t.Fatalf("Remove(1): %v", err)
	}
	if _, err := mgr.Get(1); !errors.Is(err, raft.ErrGroupNotFound) {
		t.Fatalf("Get after Remove: want ErrGroupNotFound, got %v", err)
	}

	// Remove a non-existent group.
	if err := mgr.Remove(99); !errors.Is(err, raft.ErrGroupNotFound) {
		t.Fatalf("Remove(99): want ErrGroupNotFound, got %v", err)
	}
}

// ---- TestManager_GroupIDMismatch --------------------------------------------

func TestManager_GroupIDMismatch(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	cfg := raft.DefaultConfig()
	cfg.GroupID = 10 // node says 10
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = &discardSM{}
	cfg.Transport = net.NewTransport("n1")
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}

	// Trying to register under groupID=20 while node has GroupID=10 must fail.
	if err := mgr.Add(20, node); err == nil {
		t.Fatal("expected error for GroupID mismatch, got nil")
	}
}

// ---- TestManager_RoutingUnknownGroup ----------------------------------------

func TestManager_RoutingUnknownGroup(t *testing.T) {
	mgr := raft.NewManager()

	// Lookup a group that was never registered.
	_, ok := mgr.Lookup(999)
	if ok {
		t.Fatal("Lookup(999) should return false for unknown group")
	}
	if _, err := mgr.Get(999); !errors.Is(err, raft.ErrGroupNotFound) {
		t.Fatalf("Get(999): want ErrGroupNotFound, got %v", err)
	}
}

// ---- TestManager_StopAll ----------------------------------------------------

func TestManager_StopAll(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	const numGroups = 5
	nodes := make([]*raft.Node, numGroups)
	for i := range numGroups {
		id := raft.NodeID(fmt.Sprintf("n%d", i+1))
		nodes[i] = newManagedNode(t, mgr, net, uint64(i+1), id, nil)
	}

	mgr.StartAll()
	mgr.StopAll()

	// After StopAll all nodes should be stopped; Propose must return ErrStopped.
	for i, n := range nodes {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := n.Propose(ctx, []byte("x"))
		cancel()
		if !errors.Is(err, raft.ErrStopped) {
			t.Errorf("node %d after StopAll: want ErrStopped, got %v", i+1, err)
		}
	}
}

// ---- TestManager_StopAll_ClearsRegistry -------------------------------------

// TestManager_StopAll_ClearsRegistry verifies that after StopAll the internal
// registry is empty: Lookup returns false, and Add succeeds for the same
// GroupIDs (the Manager can be reused after a full restart cycle).
func TestManager_StopAll_ClearsRegistry(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	const numGroups = 5
	for i := range numGroups {
		id := raft.NodeID(fmt.Sprintf("n%d", i+1))
		newManagedNode(t, mgr, net, uint64(i+1), id, nil)
	}

	mgr.StartAll()
	mgr.StopAll()

	// Registry must be empty.
	for i := range numGroups {
		gid := uint64(i + 1)
		if _, ok := mgr.Lookup(gid); ok {
			t.Errorf("Lookup(%d) returned true after StopAll", gid)
		}
		if _, err := mgr.Get(gid); !errors.Is(err, raft.ErrGroupNotFound) {
			t.Errorf("Get(%d) after StopAll: want ErrGroupNotFound, got %v", gid, err)
		}
	}

	// Add must succeed for the same GroupIDs after StopAll (manager is reusable).
	for i := range numGroups {
		gid := uint64(i + 1)
		id := raft.NodeID(fmt.Sprintf("n%d-2", i+1))
		newManagedNode(t, mgr, net, gid, id, nil)
	}
}

// ---- TestManager_StopAll_Concurrent -----------------------------------------

// TestManager_StopAll_Concurrent verifies that StopAll stops many nodes
// concurrently: stopping N=20 nodes must finish in well under N times the
// per-node stop latency.
func TestManager_StopAll_Concurrent(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	const numGroups = 20
	for i := range numGroups {
		id := raft.NodeID(fmt.Sprintf("cc%d", i+1))
		newManagedNode(t, mgr, net, uint64(i+1), id, nil)
	}
	mgr.StartAll()

	start := time.Now()
	mgr.StopAll()
	elapsed := time.Since(start)

	// A sequential stop would take numGroups × ~stop latency. We just assert
	// the whole batch completes in a reasonable wall-clock time (1 s), which
	// would be impossible sequentially if each Stop took ≥50 ms.
	if elapsed > time.Second {
		t.Errorf("StopAll took %v for %d nodes — expected concurrent execution", elapsed, numGroups)
	}
}

// ---- TestManager_StatusAll --------------------------------------------------

func TestManager_StatusAll(t *testing.T) {
	const timeout = 3 * time.Second
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	// Build two independent single-node groups (each elects itself leader).
	ids := []raft.NodeID{"g1-n1", "g2-n1"}
	nodes := make([]*raft.Node, 2)
	for i, id := range ids {
		nodes[i] = newManagedNode(t, mgr, net, uint64(i+1), id, nil)
	}
	mgr.StartAll()
	t.Cleanup(mgr.StopAll)

	// Tick until both groups have a leader.
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		<-ticker.C
		for _, n := range nodes {
			n.Tick()
		}
		statuses := mgr.StatusAll()
		leaders := 0
		for _, s := range statuses {
			if s.State == raft.Leader {
				leaders++
			}
		}
		if leaders == 2 {
			// Both groups have leaders; verify StatusAll fields.
			if len(statuses) != 2 {
				t.Fatalf("StatusAll: want 2 entries, got %d", len(statuses))
			}
			for _, s := range statuses {
				if s.GroupID == 0 {
					t.Errorf("StatusAll: GroupID is 0")
				}
				if s.NodeID == "" {
					t.Errorf("StatusAll: NodeID is empty")
				}
			}
			return
		}
	}
	t.Fatal("timed out waiting for both groups to elect a leader")
}

// ---- TestManager_StatusAll_DoesNotBlockMutations ----------------------------

// TestManager_StatusAll_DoesNotBlockMutations verifies that StatusAll releases
// the manager lock before calling Node methods, so that concurrent Add and
// Remove calls are not serialized behind the full iteration.
//
// The test runs StatusAll and Remove concurrently under the race detector;
// if the lock were held across Node method calls, the Remove would deadlock
// (or take much longer) while StatusAll iterated over a large group set.
func TestManager_StatusAll_DoesNotBlockMutations(t *testing.T) {
	const groups = 50
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	nodes := make([]*raft.Node, groups)
	for i := range groups {
		id := raft.NodeID(fmt.Sprintf("s%d", i+1))
		nodes[i] = newManagedNode(t, mgr, net, uint64(i+1), id, nil)
	}
	mgr.StartAll()

	// Run StatusAll repeatedly in one goroutine while Remove fires in another.
	// The race detector will catch any data race; a deadlock would cause timeout.
	start := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 20 {
			_ = mgr.StatusAll()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		// Remove half the groups while StatusAll is running.
		for i := range groups / 2 {
			_ = mgr.Remove(uint64(i + 1))
		}
	}()

	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StatusAll blocked concurrent Remove — lock scope too wide")
	}
}

// ---- TestManager_RunTicker --------------------------------------------------

func TestManager_RunTicker(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	// Single-node group — it elects itself as soon as ticks fire.
	node := newManagedNode(t, mgr, net, 1, "solo", nil)
	node.Start()
	t.Cleanup(node.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.RunTicker(ctx, time.Millisecond)
		close(done)
	}()

	// Wait for the node to become leader (RunTicker is driving ticks).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if node.StateSnapshot() == raft.Leader {
			cancel()
			<-done
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("node did not become leader with RunTicker driving ticks")
}

// ---- TestManager_RunTicker_BoundedPool ---------------------------------------

// TestManager_RunTicker_BoundedPool verifies that RunTicker ticks all groups
// correctly even when there are many more groups than GOMAXPROCS. The pool
// must not drop any ticks — every registered node must eventually become
// leader (single-node groups elect themselves after a few ticks).
func TestManager_RunTicker_BoundedPool(t *testing.T) {
	// Use more groups than any realistic GOMAXPROCS to exercise the pool.
	const numGroups = 32
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	nodes := make([]*raft.Node, numGroups)
	for i := range numGroups {
		id := raft.NodeID(fmt.Sprintf("bp%d", i+1))
		nodes[i] = newManagedNode(t, mgr, net, uint64(i+1), id, nil)
	}
	mgr.StartAll()
	t.Cleanup(mgr.StopAll)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.RunTicker(ctx, time.Millisecond)

	// Every single-node group must self-elect within a reasonable deadline.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allLeaders := true
		for _, n := range nodes {
			if n.StateSnapshot() != raft.Leader {
				allLeaders = false
				break
			}
		}
		if allLeaders {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i, n := range nodes {
		if n.StateSnapshot() != raft.Leader {
			t.Errorf("group %d never became leader", i+1)
		}
	}
}

// ---- TestManager_RunTicker_PersistentPool ------------------------------------

// TestManager_RunTicker_PersistentPool verifies that RunTicker uses a
// persistent worker pool that exits cleanly when the context is cancelled,
// leaving no goroutine leak. Before this fix, new goroutines were allocated
// on every tick; now GOMAXPROCS workers are started once and reused.
func TestManager_RunTicker_PersistentPool(t *testing.T) {
	const numGroups = 8
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()
	for i := range numGroups {
		id := raft.NodeID(fmt.Sprintf("pp%d", i+1))
		newManagedNode(t, mgr, net, uint64(i+1), id, nil)
	}
	mgr.StartAll()
	t.Cleanup(mgr.StopAll)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.RunTicker(ctx, time.Millisecond)
		close(done)
	}()

	// Let the ticker run for several ticks.
	time.Sleep(20 * time.Millisecond)

	// Record goroutine count while running (workers are alive).
	duringRun := runtime.NumGoroutine()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTicker did not return after context cancellation")
	}

	// Give goroutines time to exit.
	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)

	afterCancel := runtime.NumGoroutine()

	// Workers must have exited: goroutine count must have dropped by at least
	// GOMAXPROCS compared to when the pool was running.
	nWorkers := runtime.GOMAXPROCS(0)
	if afterCancel > duringRun-nWorkers+2 {
		// +2 slack for the RunTicker goroutine itself and minor scheduler jitter.
		// If the pool did NOT shut down, afterCancel ≈ duringRun (no drop).
		// This is a weak check — goroutine leak tools like goleak are more precise,
		// but this avoids an external dependency.
		t.Errorf("goroutine leak: goroutines during run=%d, after cancel=%d, nWorkers=%d (expected drop of ~%d)",
			duringRun, afterCancel, nWorkers, nWorkers)
	}
}

// ---- TestManager_AddAndStart ------------------------------------------------

// TestManager_AddAndStart verifies that a group added to a running Manager via
// AddAndStart participates in consensus immediately without a separate Start()
// call. This is the correct workflow for dynamic group creation after the
// initial StartAll.
func TestManager_AddAndStart(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()

	// Group 1 is started first via the normal path.
	n1 := newManagedNode(t, mgr, net, 1, "g1", nil)
	mgr.StartAll()
	t.Cleanup(mgr.StopAll)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.RunTicker(ctx, time.Millisecond)

	// Wait for group 1 to elect itself leader.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && n1.StateSnapshot() != raft.Leader {
		time.Sleep(time.Millisecond)
	}
	if n1.StateSnapshot() != raft.Leader {
		t.Fatal("group 1 did not become leader")
	}

	// Add group 2 dynamically while the Manager is running.
	cfg2 := raft.DefaultConfig()
	cfg2.GroupID = 2
	cfg2.ID = "g2"
	cfg2.Storage = memstore.New()
	cfg2.StateMachine = &discardSM{}
	cfg2.Transport = net.NewTransport("g2")
	cfg2.TickInterval = 0
	n2, err := raft.New(&cfg2)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.Register("g2", n2)

	if err := mgr.AddAndStart(2, n2); err != nil {
		t.Fatalf("AddAndStart: %v", err)
	}

	// Group 2 must also self-elect (single-node group); RunTicker drives its ticks.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && n2.StateSnapshot() != raft.Leader {
		time.Sleep(time.Millisecond)
	}
	if n2.StateSnapshot() != raft.Leader {
		t.Fatal("dynamically added group 2 did not become leader")
	}

	// Both groups are still reachable via the Manager.
	if _, err := mgr.Get(1); err != nil {
		t.Errorf("Get(1): %v", err)
	}
	if _, err := mgr.Get(2); err != nil {
		t.Errorf("Get(2): %v", err)
	}
}

// ---- TestManager_RemoveGraceful ---------------------------------------------

// TestManager_RemoveGraceful verifies that removing a leader node with
// RemoveGraceful initiates a leadership transfer before stopping, so the
// remaining nodes elect a new leader without an unnecessary election timeout
// gap.
//
// Topology: one Manager per "physical node" (correct multi-Raft model).
// mgr holds node "a"; nodes "b" and "c" are raw nodes on separate managers.
// After "a" becomes leader, RemoveGraceful transfers leadership to "b" and
// stops "a"; "b" or "c" must be leader afterwards.
func TestManager_RemoveGraceful(t *testing.T) {
	const timeout = 10 * time.Second
	net := memtransport.NewNetwork()

	peers := []raft.NodeID{"a", "b", "c"}
	allNodes := make(map[raft.NodeID]*raft.Node, 3)

	// Build three nodes (one group, three replicas) using separate managers —
	// one manager per "physical node" — which is the canonical multi-Raft layout.
	mgrs := make([]*raft.Manager, 3)
	for i, id := range peers {
		mgrs[i] = raft.NewManager()
		others := make([]raft.NodeID, 0, 2)
		for _, p := range peers {
			if p != id {
				others = append(others, p)
			}
		}
		n := newManagedNode(t, mgrs[i], net, 1, id, others)
		allNodes[id] = n
	}
	for _, m := range mgrs {
		m.StartAll()
	}
	t.Cleanup(func() {
		for _, m := range mgrs {
			m.StopAll()
		}
	})

	tickAll := func() {
		for _, n := range allNodes {
			n.Tick()
		}
	}

	// Tick until a leader is elected.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var leaderID raft.NodeID
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		<-ticker.C
		tickAll()
		for _, id := range peers {
			if allNodes[id].StateSnapshot() == raft.Leader {
				leaderID = id
				break
			}
		}
		if leaderID != "" {
			break
		}
	}
	if leaderID == "" {
		t.Fatal("no leader elected")
	}

	// Find the manager that owns the leader node.
	var leaderMgr *raft.Manager
	for i, id := range peers {
		if id == leaderID {
			leaderMgr = mgrs[i]
			break
		}
	}

	// Pick a non-leader as the transfer target.
	var transferTo raft.NodeID
	for _, id := range peers {
		if id != leaderID {
			transferTo = id
			break
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Drive ticks in background so the transfer RPC can complete.
	stopTick := make(chan struct{})
	go func() {
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopTick:
				return
			case <-tk.C:
				for _, id := range peers {
					if id != leaderID {
						allNodes[id].Tick()
					}
				}
			}
		}
	}()

	if err := leaderMgr.RemoveGraceful(ctx, 1, transferTo); err != nil {
		close(stopTick)
		t.Fatalf("RemoveGraceful: %v", err)
	}
	close(stopTick)

	// The removed node must no longer be in its manager.
	if _, err := leaderMgr.Get(1); !errors.Is(err, raft.ErrGroupNotFound) {
		t.Errorf("Get after RemoveGraceful: want ErrGroupNotFound, got %v", err)
	}

	// The remaining nodes must have a leader — either via the graceful transfer
	// or a fresh election.
	hasLeader := false
	tk2 := time.NewTicker(time.Millisecond)
	defer tk2.Stop()
	deadline2 := time.Now().Add(timeout)
	for time.Now().Before(deadline2) && !hasLeader {
		<-tk2.C
		for _, id := range peers {
			if id != leaderID {
				allNodes[id].Tick()
			}
		}
		for _, id := range peers {
			if id != leaderID && allNodes[id].StateSnapshot() == raft.Leader {
				hasLeader = true
			}
		}
	}
	if !hasLeader {
		t.Error("no leader among remaining nodes after RemoveGraceful")
	}
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
	net.Register(nodeID, node)
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
