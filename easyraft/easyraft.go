package easyraft

import (
	"context"

	"github.com/brunoga/raft"
)

// EasyRaft is a convenience wrapper that binds a [Store] to a single [Collection]
// named "default". It exposes the full Collection API directly without requiring
// the caller to manage Store and Collection separately.
//
// Use [New] when your service manages a single entity type. For multiple entity
// types in one Raft group, use [NewStore] + [AddCollection] instead.
type EasyRaft[T any] struct {
	store      *Store
	collection *Collection[T]
}

// New creates a single-collection EasyRaft instance. The underlying collection
// is named "default". [WithID], [WithRaftAddr], and [WithDataDir] are required.
// Call Start after creating the instance and registering any mutations.
func New[T any](opts ...Option) (*EasyRaft[T], error) {
	s, err := NewStore(opts...)
	if err != nil {
		return nil, err
	}

	return &EasyRaft[T]{
		store:      s,
		collection: AddCollection[T](s, "default"),
	}, nil
}

// Start launches the Raft event loop. Register all mutations before calling Start.
func (e *EasyRaft[T]) Start() {
	e.store.Start()
}

// Stop shuts down the node.
func (e *EasyRaft[T]) Stop() {
	e.store.Stop()
}

// RegisterMutation defines a named atomic read-modify-write operation.
// fn must be purely deterministic — see [Collection.RegisterMutation] for the
// full determinism contract. Call before Start.
func (e *EasyRaft[T]) RegisterMutation(name string, fn func(current *T, args []byte) (*T, []byte, error)) {
	e.collection.RegisterMutation(name, fn)
}

// Create inserts a new item into the collection.
func (e *EasyRaft[T]) Create(ctx context.Context, key string, value T) error {
	return e.collection.Create(ctx, key, value)
}

// Update replaces an existing item.
func (e *EasyRaft[T]) Update(ctx context.Context, key string, value T) error {
	return e.collection.Update(ctx, key, value)
}

// Delete removes an existing item.
func (e *EasyRaft[T]) Delete(ctx context.Context, key string) error {
	return e.collection.Delete(ctx, key)
}

// Upsert inserts key if it does not exist, or replaces it if it does.
// Unlike Create + Update, Upsert is a single atomic operation that never
// returns [ErrKeyExists] or [ErrKeyNotFound].
func (e *EasyRaft[T]) Upsert(ctx context.Context, key string, value T) error {
	return e.collection.Upsert(ctx, key, value)
}

// Mutate executes a registered mutation atomically.
func (e *EasyRaft[T]) Mutate(ctx context.Context, key, name string, args []byte) ([]byte, error) {
	return e.collection.Mutate(ctx, key, name, args)
}

// CreateOnce inserts a new item with exactly-once semantics.
// See [Collection.CreateOnce] for the (clientID, seqNum) contract.
func (e *EasyRaft[T]) CreateOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key string, value T) error {
	return e.collection.CreateOnce(ctx, clientID, seqNum, key, value)
}

// UpdateOnce replaces an existing item with exactly-once semantics.
// See [Collection.CreateOnce] for the (clientID, seqNum) contract.
func (e *EasyRaft[T]) UpdateOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key string, value T) error {
	return e.collection.UpdateOnce(ctx, clientID, seqNum, key, value)
}

// DeleteOnce removes an existing item with exactly-once semantics.
// See [Collection.CreateOnce] for the (clientID, seqNum) contract.
func (e *EasyRaft[T]) DeleteOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key string) error {
	return e.collection.DeleteOnce(ctx, clientID, seqNum, key)
}

// MutateOnce executes a registered mutation with exactly-once semantics.
// See [Collection.CreateOnce] for the (clientID, seqNum) contract.
func (e *EasyRaft[T]) MutateOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key, name string, args []byte) ([]byte, error) {
	return e.collection.MutateOnce(ctx, clientID, seqNum, key, name, args)
}

// Read returns an item by key with linearizable consistency.
func (e *EasyRaft[T]) Read(ctx context.Context, key string) (T, error) {
	return e.collection.Read(ctx, key)
}

// ReadStale returns an item by key from local state.
func (e *EasyRaft[T]) ReadStale(key string) (T, error) {
	return e.collection.ReadStale(key)
}

// ListStale returns all items in the collection from local state.
func (e *EasyRaft[T]) ListStale() (map[string]T, error) {
	return e.collection.ListStale()
}

// List returns all items in the collection with linearizable consistency.
func (e *EasyRaft[T]) List(ctx context.Context) (map[string]T, error) {
	return e.collection.List(ctx)
}

// OnChange registers fn to be called whenever a committed write modifies the
// default collection. fn receives the key, the new typed value (nil on delete),
// and a deleted flag. Call before [EasyRaft.Start].
func (e *EasyRaft[T]) OnChange(fn func(key string, value *T, deleted bool)) {
	e.collection.OnChange(fn)
}

// Ready blocks until this node has a known leader and has applied at least one
// entry in the current term. Call after Start to avoid spurious [ErrNotLeader]
// errors during leader election. Returns ctx.Err() if the context expires.
func (e *EasyRaft[T]) Ready(ctx context.Context) error {
	return e.store.Ready(ctx)
}

// Status returns a point-in-time snapshot of this node's Raft state.
func (e *EasyRaft[T]) Status() raft.GroupStatus {
	return e.store.Status()
}

// Members returns the current cluster membership as seen by this node.
func (e *EasyRaft[T]) Members() []raft.PeerConfig {
	return e.store.Members()
}

// Leader returns the NodeID of the node this node believes to be the current
// Raft leader, or empty string if unknown.
func (e *EasyRaft[T]) Leader() raft.NodeID {
	return e.store.Leader()
}

// AddServer registers addr with the transport and adds id as a voting member
// of the Raft cluster. Must be called on the leader; returns [ErrNotLeader] otherwise.
func (e *EasyRaft[T]) AddServer(ctx context.Context, id raft.NodeID, addr string) error {
	return e.store.AddServer(ctx, id, addr)
}

// RemoveServer removes id from the Raft cluster membership. Must be called on
// the leader; returns [ErrNotLeader] otherwise.
func (e *EasyRaft[T]) RemoveServer(ctx context.Context, id raft.NodeID) error {
	return e.store.RemoveServer(ctx, id)
}

// TransferLeadership gracefully transfers leadership to to. Blocks until this
// node steps down or the context expires.
func (e *EasyRaft[T]) TransferLeadership(ctx context.Context, to raft.NodeID) error {
	return e.store.TransferLeadership(ctx, to)
}
