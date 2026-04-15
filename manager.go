package raft

import (
	"context"
	"errors"
	"fmt"
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
// Typical usage:
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

// StopAll calls Stop on every registered node and blocks until all have
// stopped.
func (m *Manager) StopAll() {
	m.mu.RLock()
	nodes := make([]*Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		nodes = append(nodes, n)
	}
	m.mu.RUnlock()
	for _, n := range nodes {
		n.Stop()
	}
}

// StatusAll returns a point-in-time snapshot of every registered group's
// state. The slice order is not guaranteed.
func (m *Manager) StatusAll() []GroupStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]GroupStatus, 0, len(m.nodes))
	for gid, n := range m.nodes {
		out = append(out, GroupStatus{
			GroupID:     gid,
			NodeID:      n.cfg.ID,
			State:       n.StateSnapshot(),
			Term:        n.Term(),
			LastApplied: n.LastApplied(),
		})
	}
	return out
}

// RunTicker drives Tick for all registered nodes at the given interval until
// ctx is cancelled. It fans out ticks in parallel using a small goroutine pool
// so that a slow node cannot delay other groups' election timers.
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

			var wg sync.WaitGroup
			for _, n := range nodes {
				wg.Add(1)
				go func(nd *Node) {
					defer wg.Done()
					nd.Tick()
				}(n)
			}
			wg.Wait()
		}
	}
}
