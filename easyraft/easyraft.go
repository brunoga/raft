package easyraft

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/filestore"
	"github.com/brunoga/raft/transport/grpctransport"
)

// EasyRaft provides a high-level, type-safe API for a Raft-backed collection.
type EasyRaft[T any] struct {
	store *Store
	cfg   Config
}

// New creates a new single-collection EasyRaft instance.
func New[T any](opts ...Option) (*EasyRaft[T], error) {
	var c Config
	for _, o := range opts {
		o(&c)
	}

	if c.ID == "" {
		return nil, fmt.Errorf("easyraft: WithID is required")
	}
	if c.DataDir == "" {
		return nil, fmt.Errorf("easyraft: WithDataDir is required")
	}

	er := &EasyRaft[T]{
		store: newStore(),
		cfg:   c,
	}

	if err := er.initRaft(); err != nil {
		return nil, err
	}

	return er, nil
}

func (e *EasyRaft[T]) initRaft() error {
	// 1. Storage
	if err := os.MkdirAll(e.cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	store, err := filestore.Open(e.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	// 2. Transport
	if e.cfg.RaftAddr == "" {
		return fmt.Errorf("easyraft: WithRaftAddr is required")
	}
	tr, err := grpctransport.Listen(e.cfg.RaftAddr)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}

	var peerConfigs []raft.PeerConfig
	for id, addr := range e.cfg.Peers {
		if id != e.cfg.ID {
			tr.AddPeer(id, addr)
			peerConfigs = append(peerConfigs, raft.PeerConfig{ID: id, Voter: true})
		}
	}

	// 3. Raft Config
	rCfg := raft.DefaultConfig()
	rCfg.ID = e.cfg.ID
	rCfg.Peers = peerConfigs
	rCfg.Transport = tr
	rCfg.Storage = store
	rCfg.StateMachine = e.store
	rCfg.Logger = e.cfg.Logger
	rCfg.TickInterval = 100 * time.Millisecond
	rCfg.ElectionTimeoutMin = 1 * time.Second
	rCfg.ElectionTimeoutMax = 2 * time.Second
	rCfg.HeartbeatInterval = 100 * time.Millisecond
	rCfg.SnapshotThreshold = e.cfg.SnapCount
	if rCfg.SnapshotThreshold == 0 {
		rCfg.SnapshotThreshold = 1000
	}

	node, err := raft.New(&rCfg)
	if err != nil {
		return fmt.Errorf("new raft node: %w", err)
	}

	tr.Register(e.cfg.ID, node)

	e.store.setNode(node)
	return nil
}

// Start launches the Raft event loop.
func (e *EasyRaft[T]) Start() {
	e.store.node.Start()
}

// Stop shuts down the node.
func (e *EasyRaft[T]) Stop() {
	e.store.node.Stop()
}

// RegisterMutation defines a named mutation that can be safely replicated
// and executed atomically across the cluster.
// 'fn' receives the current value and user-provided arguments, and returns
// the new value and optional response data.
func (e *EasyRaft[T]) RegisterMutation(name string, fn func(current *T, args []byte) (*T, []byte, error)) {
	e.store.registerMutation("default", name, func(currentRaw json.RawMessage, args json.RawMessage) (json.RawMessage, json.RawMessage, error) {
		var current T
		if len(currentRaw) > 0 {
			if err := json.Unmarshal(currentRaw, &current); err != nil {
				return nil, nil, fmt.Errorf("easyraft: unmarshal current: %w", err)
			}
		}

		next, resp, err := fn(&current, args)
		if err != nil {
			return nil, nil, err
		}

		nextRaw, err := json.Marshal(next)
		if err != nil {
			return nil, nil, fmt.Errorf("easyraft: marshal next: %w", err)
		}

		return nextRaw, resp, nil
	})
}

// Create inserts a new item into the collection.
// Returns ErrKeyExists if the key is already present.
func (e *EasyRaft[T]) Create(ctx context.Context, key string, value T) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("easyraft: encode value: %w", err)
	}
	_, err = e.store.propose(ctx, &command{
		Op:         opCreate,
		Collection: "default",
		Key:        key,
		Value:      b,
	})
	return err
}

// Update replaces an existing item.
// Returns ErrKeyNotFound if the key is not present.
func (e *EasyRaft[T]) Update(ctx context.Context, key string, value T) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("easyraft: encode value: %w", err)
	}
	_, err = e.store.propose(ctx, &command{
		Op:         opUpdate,
		Collection: "default",
		Key:        key,
		Value:      b,
	})
	return err
}

// Delete removes an existing item.
func (e *EasyRaft[T]) Delete(ctx context.Context, key string) error {
	_, err := e.store.propose(ctx, &command{
		Op:         opDelete,
		Collection: "default",
		Key:        key,
	})
	return err
}

// Mutate executes a registered mutation atomically.
func (e *EasyRaft[T]) Mutate(ctx context.Context, key, name string, args []byte) ([]byte, error) {
	return e.store.propose(ctx, &command{
		Op:         opMutate,
		Collection: "default",
		Key:        key,
		MutateName: name,
		MutateArgs: args,
	})
}

// Read returns an item by key with linearizable consistency.
func (e *EasyRaft[T]) Read(ctx context.Context, key string) (T, error) {
	var empty T

	if e.store.node == nil {
		return empty, fmt.Errorf("easyraft: node not started")
	}

	if _, err := e.store.node.ReadIndexLease(ctx); err != nil {
		return empty, fmt.Errorf("easyraft: read index: %w", err)
	}

	return e.ReadStale(key)
}

// ReadStale returns an item by key from local state.
// It is fast but may return stale data if the node is lagging behind the leader.
func (e *EasyRaft[T]) ReadStale(key string) (T, error) {
	var val T
	e.store.mu.RLock()
	defer e.store.mu.RUnlock()

	coll := e.store.collections["default"]
	if coll == nil {
		return val, ErrKeyNotFound
	}
	raw, ok := coll[key]
	if !ok {
		return val, ErrKeyNotFound
	}

	if err := json.Unmarshal(raw, &val); err != nil {
		return val, fmt.Errorf("easyraft: decode value: %w", err)
	}
	return val, nil
}

// List returns all items in the collection with linearizable consistency.
func (e *EasyRaft[T]) List(ctx context.Context) (map[string]T, error) {
	if e.store.node == nil {
		return nil, fmt.Errorf("easyraft: node not started")
	}

	if _, err := e.store.node.ReadIndexLease(ctx); err != nil {
		return nil, fmt.Errorf("easyraft: read index: %w", err)
	}

	e.store.mu.RLock()
	defer e.store.mu.RUnlock()

	out := make(map[string]T)
	coll := e.store.collections["default"]
	if coll == nil {
		return out, nil
	}

	for k, raw := range coll {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("easyraft: decode key %q: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}
