// Package easyraft provides a high-level abstraction over the brunoga/raft
// package that lets you build strongly-consistent, replicated services without
// managing Raft internals.
//
// # Core types
//
// [Store] is the primary type. It owns one Raft node, one gRPC transport, and
// one file-backed storage directory. It implements [raft.StateMachine]
// internally — callers never touch Apply, Snapshot, or Restore directly.
//
// [Collection] is a type-safe namespace within a Store. Multiple collections
// can share a single Store (and thus a single Raft group). Each collection
// provides Create, Read, Update, Delete, List, and Mutate operations.
//
// [EasyRaft] is a convenience wrapper that binds one Store to one Collection
// named "default". Use it when your service manages a single entity type.
//
// [Manager] runs multiple independent Raft groups (shards) on one physical
// node, sharing a single gRPC transport and HTTP server.
//
// # Determinism requirement
//
// Mutation functions registered with [Collection.RegisterMutation] execute
// inside [raft.StateMachine].Apply on every replica during both normal
// operation and log replay after a restart. They must be purely deterministic:
// do not call time.Now, rand, or any external I/O inside a mutation. If a
// mutation needs the current time, encode the timestamp in the args before
// proposing — all nodes will then apply the same value.
//
// # Getting started
//
//	er, err := easyraft.New[MyType](
//	    easyraft.WithID("n1"),
//	    easyraft.WithRaftAddr(":7001"),
//	    easyraft.WithDataDir("/data/n1"),
//	    easyraft.WithPeers(map[raft.NodeID]string{"n2": "host2:7001"}),
//	)
//	er.Start()
//	defer er.Stop()
//
// See the package README and [examples/ratelimiter] for complete examples.
package easyraft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/metrics/prommetrics"
	"github.com/brunoga/raft/storage/filestore"
	"github.com/brunoga/raft/transport/grpctransport"
)

var (
	// ErrKeyNotFound is returned by Read, Update, Delete, and Mutate when the
	// requested key does not exist in the collection.
	ErrKeyNotFound = errors.New("easyraft: key not found")

	// ErrKeyExists is returned by Create when a key already exists in the
	// collection.
	ErrKeyExists = errors.New("easyraft: key already exists")

	// ErrNotLeader is returned by any write or linearizable-read operation when
	// this node is not the current Raft leader. The HTTP layer translates this
	// into a 307 redirect (if the leader's address is known) or a 503 response.
	ErrNotLeader = errors.New("easyraft: not leader")
)

type opType string

const (
	opCreate opType = "create"
	opUpdate opType = "update"
	opDelete opType = "delete"
	opMutate opType = "mutate"
	opBatch  opType = "batch"
)

type command struct {
	Op         opType          `json:"op"`
	Collection string          `json:"collection"`
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value,omitempty"`
	MutateName string          `json:"mutate_name,omitempty"`
	MutateArgs json.RawMessage `json:"mutate_args,omitempty"`
	Batch      []command       `json:"batch,omitempty"`
}

// Store is a Raft-replicated key-value store that can hold multiple typed
// collections. Each Store owns exactly one Raft node, one gRPC transport, and
// one persistent storage directory.
//
// Create a Store with [NewStore], add typed views with [AddCollection], then
// call [Store.Start]. Writes are forwarded to the leader automatically via
// ErrNotLeader; the HTTP layer (enabled by [WithHTTPAddr]) additionally issues
// 307 redirects to the leader's HTTP address.
//
// A Store is safe to use from multiple goroutines after Start returns.
type Store struct {
	mu          sync.RWMutex
	collections map[string]map[string]json.RawMessage
	mutations   map[string]map[string]MutationFunc
	node        *raft.Node

	cfg        Config
	cancel     context.CancelFunc
	stopCtx    context.Context
	httpServer *http.Server
	transport  raft.Transport
	storage    raft.Storage
}

// NewStore creates a Store that can manage multiple typed collections.
// [WithID], [WithRaftAddr], and [WithDataDir] are required; all other options
// are optional. Call [Store.Start] after registering collections.
func NewStore(opts ...Option) (*Store, error) {
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

	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{
		collections: make(map[string]map[string]json.RawMessage),
		mutations:   make(map[string]map[string]MutationFunc),
		cfg:         c,
		cancel:      cancel,
		stopCtx:     ctx,
	}

	if err := s.initRaft(); err != nil {
		return nil, err
	}

	return s, nil
}

// Txn accumulates operations across multiple collections to be committed as a
// single atomic log entry. Build one via [Store.Txn]; do not construct directly.
type Txn struct {
	store *Store
	cmds  []command
}

// Create adds a create operation to the transaction.
func (t *Txn) Create(collection, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	t.cmds = append(t.cmds, command{
		Op:         opCreate,
		Collection: collection,
		Key:        key,
		Value:      b,
	})
	return nil
}

// Update adds an update operation to the transaction.
func (t *Txn) Update(collection, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	t.cmds = append(t.cmds, command{
		Op:         opUpdate,
		Collection: collection,
		Key:        key,
		Value:      b,
	})
	return nil
}

// Delete adds a delete operation to the transaction.
func (t *Txn) Delete(collection, key string) {
	t.cmds = append(t.cmds, command{
		Op:         opDelete,
		Collection: collection,
		Key:        key,
	})
}

// Mutate adds a mutation operation to the transaction.
func (t *Txn) Mutate(collection, key, name string, args any) error {
	var b []byte
	if args != nil {
		var err error
		b, err = json.Marshal(args)
		if err != nil {
			return err
		}
	}
	t.cmds = append(t.cmds, command{
		Op:         opMutate,
		Collection: collection,
		Key:        key,
		MutateName: name,
		MutateArgs: b,
	})
	return nil
}

// Txn calls fn to build a batch of operations and then proposes the entire
// batch as a single Raft log entry. Either all operations succeed or the whole
// entry fails — there is no partial application.
//
// The returned slice has one json.RawMessage per operation in the order they
// were added; mutation results are included, CRUD results are null.
// An empty transaction (no operations added) returns nil, nil.
func (s *Store) Txn(ctx context.Context, fn func(tx *Txn) error) ([]json.RawMessage, error) {
	tx := &Txn{store: s}
	if err := fn(tx); err != nil {
		return nil, err
	}

	if len(tx.cmds) == 0 {
		return nil, nil
	}

	res, err := s.propose(ctx, &command{
		Op:    opBatch,
		Batch: tx.cmds,
	})
	if err != nil {
		return nil, err
	}

	var results []json.RawMessage
	if err := json.Unmarshal(res, &results); err != nil {
		return nil, fmt.Errorf("easyraft: decode txn results: %w", err)
	}
	return results, nil
}

func (s *Store) initRaft() error {
	// 1. Storage
	if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	st, err := filestore.Open(s.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	// 2. Transport
	if s.cfg.RaftAddr == "" {
		return fmt.Errorf("easyraft: WithRaftAddr is required")
	}

	var trOpts []grpctransport.Option
	if s.cfg.TLS != nil {
		trOpts = append(trOpts, grpctransport.WithTLSConfig(s.cfg.TLS))
	}

	tr, err := grpctransport.Listen(s.cfg.RaftAddr, trOpts...)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}

	var peerConfigs []raft.PeerConfig
	for id, addr := range s.cfg.Peers {
		if id != s.cfg.ID {
			tr.AddPeer(id, addr)
			peerConfigs = append(peerConfigs, raft.PeerConfig{ID: id, Voter: true})
		}
	}

	// 3. Raft Config
	rCfg := raft.DefaultConfig()
	rCfg.ID = s.cfg.ID
	rCfg.Peers = peerConfigs
	rCfg.Transport = tr
	rCfg.Storage = st
	rCfg.StateMachine = s
	rCfg.Logger = s.cfg.Logger
	rCfg.TickInterval = s.raftTickInterval()
	rCfg.ElectionTimeoutMin = s.raftElectionTimeoutMin()
	rCfg.ElectionTimeoutMax = s.raftElectionTimeoutMax()
	rCfg.HeartbeatInterval = s.raftHeartbeatInterval()
	rCfg.SnapshotThreshold = s.cfg.SnapCount
	if rCfg.SnapshotThreshold == 0 {
		rCfg.SnapshotThreshold = 1000
	}

	// 4. Metrics — must be set before raft.New so the node is constructed with metrics wired in.
	if s.cfg.PromRegisterer != nil {
		rCfg.Metrics = prommetrics.New(s.cfg.PromRegisterer)
	}

	node, err := raft.New(&rCfg)
	if err != nil {
		return fmt.Errorf("new raft node: %w", err)
	}

	tr.Register(s.cfg.ID, node)
	s.transport = tr
	s.storage = st

	// 5. Discovery: wire both transport-level connectivity and Raft membership.
	// We run our own polling loop rather than using DiscoveryAgent so we can
	// call node.AddServer for each newly seen peer (not just tr.AddPeer).
	if s.cfg.Discovery != nil {
		s.startDiscovery(node, tr)
	}

	s.node = node
	return nil
}

// peerAdder is the subset of Transport that can register peer addresses.
// grpctransport.GRPCTransport satisfies this interface.
type peerAdder interface {
	AddPeer(raft.NodeID, string)
}

// startDiscovery launches the background goroutines that keep the transport
// peer table and Raft membership in sync with the discovery output.
// It uses s.stopCtx so everything is torn down cleanly on Store.Stop.
func (s *Store) startDiscovery(node *raft.Node, tr peerAdder) {
	// Some Discovery implementations (e.g. udpbroadcast) need their own Run loop
	// to receive incoming peer announcements.
	if runner, ok := s.cfg.Discovery.(interface{ Run(context.Context) error }); ok {
		go func() {
			if err := runner.Run(s.stopCtx); err != nil && !errors.Is(err, context.Canceled) {
				if s.cfg.Logger != nil {
					s.cfg.Logger.Error("discovery runner failed", "err", err)
				}
			}
		}()
	}

	interval := s.cfg.DiscoveryInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// knownMembers tracks peers that have been successfully added to the Raft
		// cluster via AddServer.  Pre-populate with statically configured peers so
		// we never issue a redundant membership-change RPC for them.
		knownMembers := make(map[raft.NodeID]struct{})
		for id := range s.cfg.Peers {
			knownMembers[id] = struct{}{}
		}

		for {
			peers, err := s.cfg.Discovery.Discover(s.stopCtx)
			if err != nil && !errors.Is(err, context.Canceled) && s.cfg.Logger != nil {
				s.cfg.Logger.Warn("discovery: Discover failed", "err", err)
			}
			for _, p := range peers {
				if p.ID == s.cfg.ID {
					continue
				}
				tr.AddPeer(p.ID, p.Addr)
				if _, seen := knownMembers[p.ID]; seen {
					continue
				}
				addCtx, cancel := context.WithTimeout(s.stopCtx, 5*time.Second)
				if err := node.AddServer(addCtx, raft.PeerConfig{ID: p.ID, Voter: true}); err != nil {
					if s.cfg.Logger != nil {
						s.cfg.Logger.Warn("discovery: AddServer failed (will retry)", "peer", p.ID, "err", err)
					}
				} else {
					knownMembers[p.ID] = struct{}{}
				}
				cancel()
			}

			select {
			case <-s.stopCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Start launches the Raft event loop, begins advertising this node's HTTP
// address to the cluster (if WithHTTPAddr was set), and starts the HTTP server.
// Register all collections and mutations before calling Start.
func (s *Store) Start() {
	if s.node != nil {
		s.node.Start()
	}

	// Register our own HTTP address in the metadata collection so others can redirect to us.
	if s.cfg.HTTPAddr != "" {
		go s.advertiseMetadata()
	}

	if s.cfg.HTTPAddr != "" {
		_ = s.serveHTTP()
	}
}

func (s *Store) advertiseMetadata() {
	// Retry until we successfully register our HTTP address with the leader.
	// This ensures that after a leadership rotation, every node's URL is known.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		if s.node == nil {
			return
		}

		b, _ := json.Marshal(s.cfg.HTTPAddr)
		cmd := &command{
			Collection: "__easyraft_metadata__",
			Key:        string(s.cfg.ID),
			Value:      b,
		}

		// Try create first: succeeds on fresh start (one round-trip).
		cmd.Op = opCreate
		_, err := s.propose(s.stopCtx, cmd)
		if errors.Is(err, ErrKeyExists) {
			// Key already exists — update our address in-place.
			cmd.Op = opUpdate
			_, err = s.propose(s.stopCtx, cmd)
		}

		if err == nil {
			if s.cfg.Logger != nil {
				s.cfg.Logger.Info("easyraft: advertised HTTP address", "addr", s.cfg.HTTPAddr)
			}
			return
		}

		select {
		case <-s.stopCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Ready blocks until this node has a known leader and has applied at least one
// entry in the current term, confirming the local state machine is caught up.
// Call it after Start before issuing your first writes to avoid spurious
// ErrNotLeader errors during leader election.
// Returns ctx.Err() if the context expires, or an error if the store is stopped.
func (s *Store) Ready(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if s.node != nil {
			if s.node.Leader() != "" {
				// Once we have a leader, do a ReadIndex to ensure we are caught up.
				_, err := s.node.ReadIndex(ctx)
				if err == nil {
					return nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCtx.Done():
			return errors.New("easyraft: store stopped")
		case <-ticker.C:
		}
	}
}

// Status returns the current status of the underlying Raft node.
func (s *Store) Status() raft.GroupStatus {
	return s.node.Status()
}

// Stop cancels the store's context (terminating discovery goroutines), shuts
// down the HTTP server, stops the Raft node, and closes persistent storage.
// It is safe to call Stop more than once.
func (s *Store) Stop() {

	if s.cancel != nil {
		s.cancel()
	}
	if s.httpServer != nil {
		_ = s.httpServer.Shutdown(context.Background())
	}
	if s.node != nil {
		s.node.Stop()
	}
	if s.storage != nil {
		if closer, ok := s.storage.(io.Closer); ok {
			_ = closer.Close()
		}
	}
}

// Collection is a type-safe view into a named namespace within a [Store].
// All keys within a collection are independent of keys in other collections.
// T must be JSON-serialisable.
//
// A Collection is a lightweight handle — it is safe to create multiple
// Collection values pointing at the same name and store.
type Collection[T any] struct {
	store *Store
	name  string
}

// AddCollection returns a typed Collection handle for the given name within s.
// It can be called at any time — before or after Start — and is idempotent for
// the same name and type.
// Collection names starting with "__" are reserved for internal use and will
// panic if used.
func AddCollection[T any](s *Store, name string) *Collection[T] {
	if strings.HasPrefix(name, "__") {
		panic("easyraft: collection names starting with '__' are reserved")
	}
	return &Collection[T]{
		store: s,
		name:  name,
	}
}

// RegisterMutation registers a named read-modify-write function for this
// collection. Mutations are the correct way to update a value based on its
// current state — a Read followed by Update is not atomic.
//
// fn is called inside [raft.StateMachine].Apply, which runs on every replica
// during both normal operation and log replay after a restart or snapshot
// restore. fn MUST be purely deterministic: do not call time.Now, rand, or
// any external I/O inside fn. If different replicas produce different results
// for the same log entry, the cluster state will silently diverge.
//
// If fn needs the current time or any external value, encode it in the args
// before calling Mutate — all replicas will then receive and apply the same bytes.
//
// If fn returns an error, the state machine is not updated and the error is
// returned to the proposing caller.
//
// Call RegisterMutation before Start.
func (c *Collection[T]) RegisterMutation(name string, fn func(current *T, args []byte) (*T, []byte, error)) {
	c.store.registerMutation(c.name, name, func(currentRaw json.RawMessage, args json.RawMessage) (json.RawMessage, json.RawMessage, error) {
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
// Returns [ErrKeyExists] if an item with that key already exists.
// Returns [ErrNotLeader] if this node is not the current Raft leader.
func (c *Collection[T]) Create(ctx context.Context, key string, value T) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("easyraft: encode value: %w", err)
	}
	_, err = c.store.propose(ctx, &command{
		Op:         opCreate,
		Collection: c.name,
		Key:        key,
		Value:      b,
	})
	return err
}

// Update replaces an existing item.
// Returns [ErrKeyNotFound] if the key does not exist.
// Returns [ErrNotLeader] if this node is not the current Raft leader.
func (c *Collection[T]) Update(ctx context.Context, key string, value T) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("easyraft: encode value: %w", err)
	}
	_, err = c.store.propose(ctx, &command{
		Op:         opUpdate,
		Collection: c.name,
		Key:        key,
		Value:      b,
	})
	return err
}

// Delete removes an existing item.
// Returns [ErrKeyNotFound] if the key does not exist.
// Returns [ErrNotLeader] if this node is not the current Raft leader.
func (c *Collection[T]) Delete(ctx context.Context, key string) error {
	_, err := c.store.propose(ctx, &command{
		Op:         opDelete,
		Collection: c.name,
		Key:        key,
	})
	return err
}

// CreateOnce inserts a new item with exactly-once semantics. If the network
// drops the response and the caller retries with the same (clientID, seqNum),
// the cached result is returned without applying the command a second time.
// seqNum must increase monotonically per clientID.
func (c *Collection[T]) CreateOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key string, value T) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("easyraft: encode value: %w", err)
	}
	_, err = c.store.ProposeOnce(ctx, clientID, seqNum, &command{
		Op:         opCreate,
		Collection: c.name,
		Key:        key,
		Value:      b,
	})
	return err
}

// UpdateOnce replaces an existing item with exactly-once semantics.
// See [Collection.CreateOnce] for the seqNum contract.
func (c *Collection[T]) UpdateOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key string, value T) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("easyraft: encode value: %w", err)
	}
	_, err = c.store.ProposeOnce(ctx, clientID, seqNum, &command{
		Op:         opUpdate,
		Collection: c.name,
		Key:        key,
		Value:      b,
	})
	return err
}

// DeleteOnce removes an existing item with exactly-once semantics.
// See [Collection.CreateOnce] for the seqNum contract.
func (c *Collection[T]) DeleteOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key string) error {
	_, err := c.store.ProposeOnce(ctx, clientID, seqNum, &command{
		Op:         opDelete,
		Collection: c.name,
		Key:        key,
	})
	return err
}

// Mutate executes a named mutation registered with [Collection.RegisterMutation].
// The mutation runs as a single Raft log entry — it is an atomic read-modify-write.
// args is passed verbatim to the mutation function and may be nil.
// Returns the response bytes returned by the mutation function, or an error.
// Returns [ErrKeyNotFound] if the key does not exist.
// Returns [ErrNotLeader] if this node is not the current Raft leader.
func (c *Collection[T]) Mutate(ctx context.Context, key, name string, args []byte) ([]byte, error) {
	return c.store.propose(ctx, &command{
		Op:         opMutate,
		Collection: c.name,
		Key:        key,
		MutateName: name,
		MutateArgs: args,
	})
}

// MutateOnce executes a registered mutation with exactly-once semantics.
// See [Collection.CreateOnce] for the seqNum contract.
func (c *Collection[T]) MutateOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key, name string, args []byte) ([]byte, error) {
	return c.store.ProposeOnce(ctx, clientID, seqNum, &command{
		Op:         opMutate,
		Collection: c.name,
		Key:        key,
		MutateName: name,
		MutateArgs: args,
	})
}

// Read returns an item by key with linearizable consistency. It performs a
// ReadIndexLease round-trip to confirm the local state machine is up to date
// before serving from the local map.
// Returns [ErrKeyNotFound] if the key does not exist.
// Returns [ErrNotLeader] if this node cannot establish a read lease.
func (c *Collection[T]) Read(ctx context.Context, key string) (T, error) {
	var empty T
	if c.store.node == nil {
		return empty, fmt.Errorf("easyraft: node not started")
	}
	if _, err := c.store.node.ReadIndexLease(ctx); err != nil {
		return empty, fmt.Errorf("easyraft: read index: %w", err)
	}
	return c.ReadStale(key)
}

// ReadStale returns an item by key directly from the local state machine,
// without a leader round-trip. The result may lag behind the cluster by up to
// one heartbeat interval. Use this for read-heavy workloads where bounded
// staleness is acceptable.
// Returns [ErrKeyNotFound] if the key does not exist locally.
func (c *Collection[T]) ReadStale(key string) (T, error) {
	var val T
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	coll := c.store.collections[c.name]
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

// ListStale returns all items in the collection from the local state machine
// without a leader round-trip. The result may be stale by up to one heartbeat.
// Returns an empty map (not nil) if the collection has no items.
func (c *Collection[T]) ListStale() (map[string]T, error) {
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	out := make(map[string]T)
	coll := c.store.collections[c.name]
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

// List returns all items in the collection with linearizable consistency.
// It performs a ReadIndexLease round-trip before reading from local state.
// Returns an empty map (not nil) if the collection has no items.
func (c *Collection[T]) List(ctx context.Context) (map[string]T, error) {
	if c.store.node == nil {
		return nil, fmt.Errorf("easyraft: node not started")
	}
	if _, err := c.store.node.ReadIndexLease(ctx); err != nil {
		return nil, fmt.Errorf("easyraft: read index: %w", err)
	}

	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	out := make(map[string]T)
	coll := c.store.collections[c.name]
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

// AddServer registers addr with the transport and adds id as a voting member
// of the Raft cluster. This must be called on the leader; it returns
// [ErrNotLeader] otherwise. Blocks until the membership change is committed.
func (s *Store) AddServer(ctx context.Context, id raft.NodeID, addr string) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	if adder, ok := s.transport.(interface {
		AddPeer(raft.NodeID, string)
	}); ok {
		adder.AddPeer(id, addr)
	}
	return s.node.AddServer(ctx, raft.PeerConfig{ID: id, Voter: true})
}

// RemoveServer removes id from the Raft cluster membership. Must be called on
// the leader. If id is the current leader, it commits the removal and then
// steps down; the caller is responsible for stopping the removed node.
func (s *Store) RemoveServer(ctx context.Context, id raft.NodeID) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	return s.node.RemoveServer(ctx, id)
}

// TransferLeadership gracefully hands off leadership to to. The leader waits
// for to to catch up on the log, then sends a TimeoutNow RPC that causes it
// to start an election immediately. Blocks until this node steps down or the
// context expires.
func (s *Store) TransferLeadership(ctx context.Context, to raft.NodeID) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	return s.node.TransferLeadership(ctx, to)
}

// MutationFunc is the low-level, untyped mutation callback stored in the
// registry. It receives the current value and args as raw JSON and must return
// the new value and an optional response, both as raw JSON.
//
// Use [Collection.RegisterMutation] to register a typed wrapper instead of
// implementing MutationFunc directly.
//
// If an error is returned, the state machine is NOT updated.
// MutationFunc MUST be purely deterministic — see [Collection.RegisterMutation].
type MutationFunc func(currentValue json.RawMessage, args json.RawMessage) (newValue json.RawMessage, response json.RawMessage, err error)

func (s *Store) registerMutation(collection, name string, fn MutationFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutations[collection] == nil {
		s.mutations[collection] = make(map[string]MutationFunc)
	}
	s.mutations[collection][name] = fn
}

func (s *Store) Apply(_ context.Context, entry raft.LogEntry) ([]byte, error) {
	var cmd command
	if err := json.Unmarshal(entry.Command, &cmd); err != nil {
		return nil, fmt.Errorf("easyraft: decode cmd: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.applyCommand(&cmd)
}

func (s *Store) applyCommand(cmd *command) ([]byte, error) {
	if cmd.Op == opBatch {
		results := make([]json.RawMessage, 0, len(cmd.Batch))
		for i := range cmd.Batch {
			res, err := s.applyCommand(&cmd.Batch[i])
			if err != nil {
				return nil, err
			}
			results = append(results, res)
		}
		return json.Marshal(results)
	}

	coll, exists := s.collections[cmd.Collection]
	if !exists {
		coll = make(map[string]json.RawMessage)
		s.collections[cmd.Collection] = coll
	}

	switch cmd.Op {
	case opCreate:
		if _, ok := coll[cmd.Key]; ok {
			return nil, ErrKeyExists
		}
		coll[cmd.Key] = cmd.Value
		return nil, nil

	case opUpdate:
		if _, ok := coll[cmd.Key]; !ok {
			return nil, ErrKeyNotFound
		}
		coll[cmd.Key] = cmd.Value
		return nil, nil

	case opDelete:
		if _, ok := coll[cmd.Key]; !ok {
			return nil, ErrKeyNotFound
		}
		delete(coll, cmd.Key)
		return nil, nil

	case opMutate:
		currentVal, ok := coll[cmd.Key]
		if !ok {
			return nil, ErrKeyNotFound
		}

		collMutations := s.mutations[cmd.Collection]
		if collMutations == nil {
			return nil, fmt.Errorf("easyraft: unknown mutation collection %q", cmd.Collection)
		}
		fn, ok := collMutations[cmd.MutateName]
		if !ok {
			return nil, fmt.Errorf("easyraft: unknown mutation %q in collection %q", cmd.MutateName, cmd.Collection)
		}

		newVal, resp, err := fn(currentVal, cmd.MutateArgs)
		if err != nil {
			return nil, err
		}
		coll[cmd.Key] = newVal
		return resp, nil

	default:
		return nil, fmt.Errorf("easyraft: unknown op %q", cmd.Op)
	}
}

func (s *Store) Snapshot(_ context.Context, w io.Writer) error {
	s.mu.RLock()
	// Create a point-in-time copy of the collections map structure.
	// Since values are json.RawMessage ([]byte), we only copy the slice headers.
	// This is safe because we never mutate the bytes of a RawMessage in-place.
	snap := make(map[string]map[string]json.RawMessage, len(s.collections))
	for collName, items := range s.collections {
		collCopy := make(map[string]json.RawMessage, len(items))
		for k, v := range items {
			collCopy[k] = v
		}
		snap[collName] = collCopy
	}
	s.mu.RUnlock()

	// Perform the expensive JSON encoding and I/O outside the lock.
	return json.NewEncoder(w).Encode(snap)
}

func (s *Store) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	var collections map[string]map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&collections); err != nil {
		return fmt.Errorf("easyraft: restore decode: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if collections == nil {
		collections = make(map[string]map[string]json.RawMessage)
	}
	s.collections = collections
	return nil
}

// raftTickInterval returns the configured tick interval, falling back to the
// easyraft default of 100 ms (more conservative than the raft package default).
func (s *Store) raftTickInterval() time.Duration {
	if s.cfg.TickInterval > 0 {
		return s.cfg.TickInterval
	}
	return 100 * time.Millisecond
}

func (s *Store) raftHeartbeatInterval() time.Duration {
	if s.cfg.HeartbeatInterval > 0 {
		return s.cfg.HeartbeatInterval
	}
	return 100 * time.Millisecond
}

func (s *Store) raftElectionTimeoutMin() time.Duration {
	if s.cfg.ElectionTimeoutMin > 0 {
		return s.cfg.ElectionTimeoutMin
	}
	return 1 * time.Second
}

func (s *Store) raftElectionTimeoutMax() time.Duration {
	if s.cfg.ElectionTimeoutMax > 0 {
		return s.cfg.ElectionTimeoutMax
	}
	return 2 * time.Second
}

// propose encodes a command and proposes it to the Raft node.
func (s *Store) propose(ctx context.Context, cmd *command) ([]byte, error) {
	if s.node == nil {
		return nil, errors.New("easyraft: node not started")
	}

	b, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("easyraft: encode cmd: %w", err)
	}

	res, err := s.node.Propose(ctx, b)
	if err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return nil, ErrNotLeader
		}
		return nil, err
	}
	return res, nil
}

// ProposeOnce submits a command for replication with exactly-once semantics.
func (s *Store) ProposeOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, cmd *command) ([]byte, error) {
	if s.node == nil {
		return nil, errors.New("easyraft: node not started")
	}

	b, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("easyraft: encode cmd: %w", err)
	}

	res, err := s.node.ProposeOnce(ctx, clientID, seqNum, b)
	if err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return nil, ErrNotLeader
		}
		return nil, err
	}
	return res, nil
}
