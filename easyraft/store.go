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
	"github.com/brunoga/raft/discovery"
	"github.com/brunoga/raft/metrics/prommetrics"
	"github.com/brunoga/raft/storage/filestore"
	"github.com/brunoga/raft/transport/grpctransport"
)

var (
	ErrKeyNotFound = errors.New("easyraft: key not found")
	ErrKeyExists   = errors.New("easyraft: key already exists")
	ErrNotLeader   = errors.New("easyraft: not leader")
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

type Store struct {
	mu           sync.RWMutex
	collections  map[string]map[string]json.RawMessage
	mutations    map[string]map[string]MutationFunc
	node         *raft.Node
	waitForApply time.Duration

	cfg        Config
	cancel     context.CancelFunc
	stopCtx    context.Context
	httpServer *http.Server
	transport  raft.Transport
	storage    raft.Storage
}

// NewStore creates a new EasyRaft store that can manage multiple collections.
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
		collections:  make(map[string]map[string]json.RawMessage),
		mutations:    make(map[string]map[string]MutationFunc),
		waitForApply: 5 * time.Second,
		cfg:          c,
		cancel:       cancel,
		stopCtx:      ctx,
	}

	if err := s.initRaft(); err != nil {
		return nil, err
	}

	return s, nil
}

// Txn provides an atomic, cross-collection transaction.
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

// Txn executes a function within an atomic transaction.
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
	rCfg.TickInterval = 100 * time.Millisecond
	rCfg.ElectionTimeoutMin = 1 * time.Second
	rCfg.ElectionTimeoutMax = 2 * time.Second
	rCfg.HeartbeatInterval = 100 * time.Millisecond
	rCfg.SnapshotThreshold = s.cfg.SnapCount
	if rCfg.SnapshotThreshold == 0 {
		rCfg.SnapshotThreshold = 1000
	}

	node, err := raft.New(&rCfg)
	if err != nil {
		return fmt.Errorf("new raft node: %w", err)
	}

	tr.Register(s.cfg.ID, node)
	s.transport = tr
	s.storage = st

	// 4. Metrics
	if s.cfg.PromRegisterer != nil {
		rCfg.Metrics = prommetrics.New(s.cfg.PromRegisterer)
	}

	// 5. Discovery
	if s.cfg.Discovery != nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		agent := discovery.NewAgent(s.cfg.Discovery, tr, s.cfg.DiscoveryInterval)
		go func() {
			if err := agent.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				if s.cfg.Logger != nil {
					s.cfg.Logger.Error("discovery agent failed", "err", err)
				}
			}
		}()

		if runner, ok := s.cfg.Discovery.(interface {
			Run(context.Context) error
		}); ok {
			go func() {
				if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					if s.cfg.Logger != nil {
						s.cfg.Logger.Error("discovery runner failed", "err", err)
					}
				}
			}()
		}
	}

	s.node = node
	return nil
}

// Start launches the Raft event loop and HTTP server.
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
	// This ensures that after a few rotations, every node's URL is known.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		if s.node == nil {
			return
		}

		b, _ := json.Marshal(s.cfg.HTTPAddr)
		_, err := s.propose(s.stopCtx, &command{
			Op:         opUpdate,
			Collection: "__easyraft_metadata__",
			Key:        string(s.cfg.ID),
			Value:      b,
		})

		// If key doesn't exist, try create
		if errors.Is(err, ErrKeyNotFound) {
			_, err = s.propose(s.stopCtx, &command{
				Op:         opCreate,
				Collection: "__easyraft_metadata__",
				Key:        string(s.cfg.ID),
				Value:      b,
			})
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

// Ready blocks until the node has a known leader and has applied at least
// one entry from the current term, indicating it is ready for operations.
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

// Stop shuts down the store and releases all resources.
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

// Collection provides a type-safe view into a named collection within a Store.
type Collection[T any] struct {
	store *Store
	name  string
}

// AddCollection adds a new typed collection to the store.
// Collection names starting with "__" are reserved for internal use.
func AddCollection[T any](s *Store, name string) *Collection[T] {
	if strings.HasPrefix(name, "__") {
		panic("easyraft: collection names starting with '__' are reserved")
	}
	return &Collection[T]{
		store: s,
		name:  name,
	}
}

// RegisterMutation defines a named mutation for a specific collection.
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
func (c *Collection[T]) Delete(ctx context.Context, key string) error {
	_, err := c.store.propose(ctx, &command{
		Op:         opDelete,
		Collection: c.name,
		Key:        key,
	})
	return err
}

// CreateOnce inserts a new item into the collection with exactly-once semantics.
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
func (c *Collection[T]) DeleteOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key string) error {
	_, err := c.store.ProposeOnce(ctx, clientID, seqNum, &command{
		Op:         opDelete,
		Collection: c.name,
		Key:        key,
	})
	return err
}

// Mutate executes a registered mutation atomically.
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
func (c *Collection[T]) MutateOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key, name string, args []byte) ([]byte, error) {
	return c.store.ProposeOnce(ctx, clientID, seqNum, &command{
		Op:         opMutate,
		Collection: c.name,
		Key:        key,
		MutateName: name,
		MutateArgs: args,
	})
}

// Read returns an item by key with linearizable consistency.
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

// ReadStale returns an item by key from local state.
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

// ListStale returns all items in the collection from local state.
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

// AddServer adds a new peer to the cluster.
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

// RemoveServer removes a peer from the cluster.
func (s *Store) RemoveServer(ctx context.Context, id raft.NodeID) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	return s.node.RemoveServer(ctx, id)
}

// TransferLeadership initiates a leadership transfer to the target node.
func (s *Store) TransferLeadership(ctx context.Context, to raft.NodeID) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	return s.node.TransferLeadership(ctx, to)
}

// MutationFunc receives the raw current value and arbitrary raw arguments,
// and returns the raw new value, an optional raw response, and an error.
// The new value will be saved back into the state machine.
// If an error is returned, the state machine is NOT updated.
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
