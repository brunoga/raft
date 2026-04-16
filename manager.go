package raft

import (
	"context"
	"errors"
	"fmt"
	"runtime"
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
//	mgr.Add(1, nodeA)
//	mgr.Add(2, nodeB)
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
func (m *Manager) AddAndStart(groupID uint64, node *Node) error {
	if err := m.Add(groupID, node); err != nil {
		return err
	}
	node.Start()
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
		// Best-effort: ignore transfer errors (e.g. no quorum) and fall through
		// to the unconditional stop below.
		_ = node.TransferLeadership(ctx, transferTo)
	}
	return m.Remove(groupID)
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

// RunTicker drives Tick for all registered nodes at the given interval until
// ctx is cancelled. It fans out ticks in parallel using a bounded worker pool
// (min(G, GOMAXPROCS) workers) so that a slow node cannot delay other groups'
// election timers, and goroutine allocation is O(GOMAXPROCS) per tick rather
// than O(G).
//
// RunTicker blocks until ctx is done.
func (m *Manager) RunTicker(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
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

			// Bounded worker pool: cap concurrency at GOMAXPROCS so we don't
			// spawn one goroutine per group per tick at large G.
			workers := min(len(nodes), runtime.GOMAXPROCS(0))
			work := make(chan *Node, len(nodes))
			for _, n := range nodes {
				work <- n
			}
			close(work)

			var wg sync.WaitGroup
			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for nd := range work {
						nd.Tick()
					}
				}()
			}
			wg.Wait()
		}
	}
}
