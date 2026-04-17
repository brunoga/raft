// Package memtransport provides an in-memory implementation of raft.Transport
// for use in tests and simulations.
//
// Multiple nodes share a single Network, which routes RPCs directly to the
// target node's Handler. The Network supports simulating partitions and
// per-link message drops.
package memtransport

import (
	"context"
	"fmt"
	"sync"

	"github.com/brunoga/raft"
)

// Network is a shared message bus. All MemTransport instances created from
// the same Network can communicate with one another.
// Safe for concurrent use.
type Network struct {
	mu       sync.RWMutex
	handlers map[raft.NodeID]raft.Handler
	// dropped holds pairs [from, to] whose messages are silently discarded.
	dropped map[[2]raft.NodeID]bool
}

// NewNetwork returns an empty Network.
func NewNetwork() *Network {
	return &Network{
		handlers: make(map[raft.NodeID]raft.Handler),
		dropped:  make(map[[2]raft.NodeID]bool),
	}
}

// NewTransport creates a MemTransport bound to this Network with the given
// node identity. Call Register on the Network (or via the Transport's own
// Register method) to make the node reachable.
func (net *Network) NewTransport(id raft.NodeID) *MemTransport {
	return &MemTransport{id: id, network: net}
}

// Register makes handler reachable at id within the Network.
func (net *Network) Register(id raft.NodeID, handler raft.Handler) {
	net.mu.Lock()
	defer net.mu.Unlock()
	net.handlers[id] = handler
}

// Unregister removes id from the Network.
func (net *Network) Unregister(id raft.NodeID) {
	net.mu.Lock()
	defer net.mu.Unlock()
	delete(net.handlers, id)
}

// Drop causes all messages from → to to be silently discarded (one direction).
func (net *Network) Drop(from, to raft.NodeID) {
	net.mu.Lock()
	defer net.mu.Unlock()
	net.dropped[[2]raft.NodeID{from, to}] = true
}

// Restore re-enables the link from → to.
func (net *Network) Restore(from, to raft.NodeID) {
	net.mu.Lock()
	defer net.mu.Unlock()
	delete(net.dropped, [2]raft.NodeID{from, to})
}

// Partition isolates node id from all other nodes (drops in both directions).
func (net *Network) Partition(id raft.NodeID) {
	net.mu.Lock()
	ids := make([]raft.NodeID, 0, len(net.handlers))
	for k := range net.handlers {
		ids = append(ids, k)
	}
	net.mu.Unlock()
	for _, other := range ids {
		if other == id {
			continue
		}
		net.Drop(id, other)
		net.Drop(other, id)
	}
}

// Heal removes all drop rules involving id.
func (net *Network) Heal(id raft.NodeID) {
	net.mu.Lock()
	defer net.mu.Unlock()
	for pair := range net.dropped {
		if pair[0] == id || pair[1] == id {
			delete(net.dropped, pair)
		}
	}
}

// dispatch routes a single RPC from src to dst.
func (net *Network) dispatch(
	ctx context.Context,
	src, dst raft.NodeID,
	call func(raft.Handler) (any, error),
) (any, error) {
	net.mu.RLock()
	handler, ok := net.handlers[dst]
	dropped := net.dropped[[2]raft.NodeID{src, dst}]
	net.mu.RUnlock()

	if dropped {
		return nil, fmt.Errorf("memtransport: dropped")
	}
	if !ok {
		return nil, fmt.Errorf("memtransport: no handler registered for %s", dst)
	}
	return call(handler)
}

// MemTransport implements raft.Transport using in-process function calls.
type MemTransport struct {
	id      raft.NodeID
	network *Network
}

func (t *MemTransport) RequestVote(ctx context.Context, to raft.NodeID, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	v, err := t.network.dispatch(ctx, t.id, to, func(h raft.Handler) (any, error) {
		return h.HandleRequestVote(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return v.(*raft.RequestVoteResponse), nil
}

func (t *MemTransport) AppendEntries(ctx context.Context, to raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	v, err := t.network.dispatch(ctx, t.id, to, func(h raft.Handler) (any, error) {
		return h.HandleAppendEntries(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return v.(*raft.AppendEntriesResponse), nil
}

func (t *MemTransport) InstallSnapshot(ctx context.Context, to raft.NodeID, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	v, err := t.network.dispatch(ctx, t.id, to, func(h raft.Handler) (any, error) {
		return h.HandleInstallSnapshot(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return v.(*raft.InstallSnapshotResponse), nil
}

func (t *MemTransport) TimeoutNow(ctx context.Context, to raft.NodeID, req *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	v, err := t.network.dispatch(ctx, t.id, to, func(h raft.Handler) (any, error) {
		return h.HandleTimeoutNow(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return v.(*raft.TimeoutNowResponse), nil
}

func (t *MemTransport) ReadIndex(ctx context.Context, to raft.NodeID, req *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	v, err := t.network.dispatch(ctx, t.id, to, func(h raft.Handler) (any, error) {
		return h.HandleReadIndex(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return v.(*raft.ReadIndexResponse), nil
}

// Register makes id reachable in this transport's Network.
func (t *MemTransport) Register(id raft.NodeID, handler raft.Handler) {
	t.network.Register(id, handler)
}

// Unregister removes id from this transport's Network.
func (t *MemTransport) Unregister(id raft.NodeID) {
	t.network.Unregister(id)
}

func (t *MemTransport) Close() error { return nil }

// Compile-time interface check.
var _ raft.Transport = (*MemTransport)(nil)
