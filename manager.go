package raft

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"sync"
	"time"
)

// ErrGroupNotFound is returned by Manager.Get when no Node is registered for
// the requested GroupID.
var ErrGroupNotFound = errors.New("raft: group not found")

// ErrGroupExists is returned by Manager.Add when a Node is already registered
// for the given GroupID.
var ErrGroupExists = errors.New("raft: group already exists")

// GroupStatus is a point-in-time snapshot of a single Raft group's state on
// this physical node. It is used by Manager.StatusAll and by the leader
// balancing controller (§22).
type GroupStatus struct {
	GroupID     uint64
	NodeID      NodeID
	State       State
	Term        Term
	LastApplied Index
}

// Manager multiplexes multiple independent Raft groups on a single physical
// node. Each group is a normal *Node identified by a uint64 GroupID. The
// Manager is the authoritative registry; the shared GRPCTransport delegates
// inbound RPC routing to the Manager via a group-lookup function.
//
// # Storage partitioning convention
//
// Every group must have its own isolated storage to avoid log and snapshot
// collisions. The recommended layout when using filestore is:
//
//	<data-dir>/groups/<groupID>/
//
// Pass that path to filestore.Open when constructing each Node's Config.
// The Manager itself does not enforce this convention; it is the caller's
// responsibility to provide correctly partitioned storage.
//
// # Typical usage
//
//	mgr := raft.NewManager()
//	cfgA := raft.DefaultConfig()
//	cfgA.TickInterval = 0 // use Manager's central ticker
//	nodeA, _ := raft.New(&cfgA)
//	mgr.Add(1, nodeA)
//
//	mgr.StartAll()
//	defer mgr.StopAll()
//	mgr.RunTicker(ctx, 10*time.Millisecond)
type Manager struct {
	mu    sync.RWMutex
	nodes map[uint64]*Node // GroupID → Node
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{nodes: make(map[uint64]*Node)}
}

// Add registers node under groupID. The node must have cfg.GroupID == groupID;
// Add returns an error if the IDs do not match or if a node is already
// registered for that group.
func (m *Manager) Add(groupID uint64, node *Node) error {
	if node.cfg.GroupID != groupID {
		return fmt.Errorf("raft: node GroupID %d does not match registered groupID %d",
			node.cfg.GroupID, groupID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.nodes[groupID]; exists {
		return ErrGroupExists
	}
	m.nodes[groupID] = node
	return nil
}

// AddAndStart registers node under groupID and immediately starts it.
// It is the correct way to add a new group to a Manager that has already
// called StartAll: a plain Add followed by a manual node.Start() is
// equivalent but error-prone.
//
// The node is started before it is made visible in the registry so that a
// concurrent StopAll cannot call Stop on a node that has never been started
// (which would block indefinitely).
func (m *Manager) AddAndStart(groupID uint64, node *Node) error {
	// Start before Add: the node is visible to StopAll only after Add returns,
	// at which point Start has already been called.
	node.Start()
	if err := m.Add(groupID, node); err != nil {
		node.Stop() // roll back: undo the Start if Add fails.
		// Note: transport.Register was already called by node.Start(), and
		// there is no transport.Deregister. In practice Add fails only for
		// duplicate GroupIDs (a programming error), so a stale handlers entry
		// in the transport is harmless — the Manager's groupLookup path takes
		// precedence for inbound multi-raft RPCs.
		return err
	}
	return nil
}

// Remove stops the node registered under groupID and unregisters it.
// Returns ErrGroupNotFound if no node is registered for that ID.
func (m *Manager) Remove(groupID uint64) error {
	m.mu.Lock()
	node, exists := m.nodes[groupID]
	if !exists {
		m.mu.Unlock()
		return ErrGroupNotFound
	}
	delete(m.nodes, groupID)
	m.mu.Unlock()
	node.Stop()
	return nil
}

// RemoveGraceful attempts a leadership transfer before stopping the node.
// If the node for groupID is the current leader it calls TransferLeadership
// to hand off leadership to transferTo, waiting up to the deadline in ctx.
// Whether the transfer succeeds or not, the node is stopped and unregistered.
// This is the preferred removal path in production: an abrupt Remove on a
// leader forces an election and a momentary quorum gap; RemoveGraceful avoids
// that gap when there is time to transfer first.
//
// Returns ErrGroupNotFound if no node is registered for groupID.
func (m *Manager) RemoveGraceful(ctx context.Context, groupID uint64, transferTo NodeID) error {
	m.mu.RLock()
	node, exists := m.nodes[groupID]
	m.mu.RUnlock()
	if !exists {
		return ErrGroupNotFound
	}
	if node.StateSnapshot() == Leader {
		// Best-effort: ignore transfer errors (e.g. no quorum, ErrStopped from a
		// concurrent StopAll that stopped the node between our RUnlock and here)
		// and fall through to the unconditional stop below.
		if err := node.TransferLeadership(ctx, transferTo); err == nil {
			// Transfer accepted: wait for the node to actually step down before
			// removing it. If we Remove too early, the background TimeoutNow RPC
			// is cancelled and the transfer fails, causing a quorum gap.
			// If ctx expires we break out and fall through to the unconditional
			// Remove — the node must always be stopped regardless of transfer
			// outcome.
		stepDown:
			for node.StateSnapshot() == Leader {
				select {
				case <-ctx.Done():
					break stepDown
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
	}
	err := m.Remove(groupID)
	if errors.Is(err, ErrGroupNotFound) {
		// A concurrent Remove or StopAll won the race between our initial lookup
		// and this call. The group is already gone — that is the desired outcome.
		return nil
	}
	return err
}

// Get returns the Node registered under groupID, or ErrGroupNotFound.
func (m *Manager) Get(groupID uint64) (*Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.nodes[groupID]
	if !ok {
		return nil, ErrGroupNotFound
	}
	return n, nil
}

// GroupIDs returns the sorted list of group IDs currently registered with the
// Manager. The slice is a snapshot; additions and removals after the call are
// not reflected. Useful for introspection, healthchecks, and leader-balancing
// controllers that need to enumerate groups without holding the lock.
func (m *Manager) GroupIDs() []uint64 {
	m.mu.RLock()
	ids := make([]uint64, 0, len(m.nodes))
	for id := range m.nodes {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	slices.Sort(ids)
	return ids
}

// Lookup returns the Handler for groupID and true, or nil and false if no node
// is registered for that ID. It satisfies the group-lookup signature consumed
// by GRPCTransport.SetGroupLookup.
func (m *Manager) Lookup(groupID uint64) (Handler, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.nodes[groupID]
	return n, ok
}

// StartAll calls Start on every registered node. Nodes that have already been
// started are unaffected (Start is idempotent).
func (m *Manager) StartAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, n := range m.nodes {
		n.Start()
	}
}

// StopAll stops every registered node concurrently and blocks until all have
// stopped. It also removes all nodes from the Manager so that Add can be
// called again for the same GroupIDs after a restart.
func (m *Manager) StopAll() {
	m.mu.Lock()
	nodes := make([]*Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		nodes = append(nodes, n)
	}
	m.nodes = make(map[uint64]*Node) // clear so the Manager can be reused
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(nd *Node) {
			defer wg.Done()
			nd.Stop()
		}(n)
	}
	wg.Wait()
}

// Close stops all registered nodes and unregisters them. It implements
// io.Closer so that Manager can be used with defer and shutdown managers that
// accept io.Closer. Close is equivalent to StopAll and always returns nil.
func (m *Manager) Close() error {
	m.StopAll()
	return nil
}

// StatusAll returns a point-in-time snapshot of every registered group's
// state. The slice order is not guaranteed.
//
// The manager lock is held only long enough to snapshot the node map; Node
// methods are called after releasing the lock so that concurrent Add, Remove,
// and StopAll calls are not blocked for the full iteration.
func (m *Manager) StatusAll() []GroupStatus {
	m.mu.RLock()
	type entry struct {
		gid uint64
		n   *Node
	}
	snap := make([]entry, 0, len(m.nodes))
	for gid, n := range m.nodes {
		snap = append(snap, entry{gid, n})
	}
	m.mu.RUnlock()

	out := make([]GroupStatus, 0, len(snap))
	for _, e := range snap {
		out = append(out, GroupStatus{
			GroupID:     e.gid,
			NodeID:      e.n.cfg.ID,
			State:       e.n.StateSnapshot(),
			Term:        e.n.Term(),
			LastApplied: e.n.LastApplied(),
		})
	}
	return out
}

// tickWgPool reuses WaitGroup objects across ticks to avoid per-tick heap
// allocation. A WaitGroup is safe to reuse after Wait() has returned.
var tickWgPool = sync.Pool{New: func() any { return new(sync.WaitGroup) }}

// RunTicker drives Tick for all registered nodes at the given interval until
// ctx is cancelled.
//
// To avoid double-ticking, RunTicker only sends ticks to nodes whose
// TickInterval is set to 0 (manual ticking). Nodes with a positive
// TickInterval already drive their own internal ticker and are skipped by
// RunTicker.
//
// RunTicker fans out ticks in parallel using a persistent worker pool of
// GOMAXPROCS goroutines so that a slow node cannot delay other groups'
// election timers. Workers are started once when RunTicker is called and
// exit cleanly when ctx is cancelled.
//
// RunTicker blocks until ctx is done and all workers have exited.
func (m *Manager) RunTicker(ctx context.Context, interval time.Duration) {
	type tickItem struct {
		node *Node
		done *sync.WaitGroup
	}

	// Warn once if any registered node has TickInterval > 0; those nodes drive
	// their own internal ticker and will be skipped by RunTicker. Operators who
	// expect RunTicker to drive all groups should set TickInterval = 0.
	var warnSkippedOnce sync.Once

	nWorkers := runtime.GOMAXPROCS(0)
	// Buffer allows the main goroutine to keep sending while workers process;
	// 2× nWorkers keeps the pipeline full without large memory cost.
	work := make(chan tickItem, nWorkers*2)

	var poolWg sync.WaitGroup
	poolWg.Add(nWorkers)
	for range nWorkers {
		go func() {
			defer poolWg.Done()
			for item := range work {
				item.node.Tick()
				item.done.Done()
			}
		}()
	}

	ticker := time.NewTicker(interval)
	defer func() {
		ticker.Stop()
		close(work)   // signals workers to exit
		poolWg.Wait() // wait for all workers to drain and exit
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			nodes := make([]*Node, 0, len(m.nodes))
			for _, n := range m.nodes {
				nodes = append(nodes, n)
			}
			m.mu.RUnlock()

			if len(nodes) == 0 {
				continue
			}

			// Reuse WaitGroup from pool to avoid per-tick heap allocation caused
			// by &tickWg escaping into the tickItem sent to workers.
			tickWg := tickWgPool.Get().(*sync.WaitGroup)
			tickWg.Add(len(nodes))
			for _, n := range nodes {
				if n.cfg.TickInterval == 0 {
					work <- tickItem{node: n, done: tickWg}
				} else {
					warnSkippedOnce.Do(func() {
						slog.Warn("RunTicker: node has TickInterval>0 and will not be ticked by the Manager; set TickInterval=0 for Manager-driven ticking",
							"node", n.cfg.ID, "tickInterval", n.cfg.TickInterval)
					})
					tickWg.Done()
				}
			}
			tickWg.Wait()
			tickWgPool.Put(tickWg)
		}
	}
}
