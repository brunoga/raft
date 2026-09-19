package easyraft

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/metrics/prommetrics"
	"github.com/brunoga/raft/storage/filestore"
	"github.com/brunoga/raft/transport/grpctransport"
)

// Manager coordinates multiple EasyRaft [Store] instances (Raft groups) on a
// single physical node. All groups share one gRPC transport listener, one HTTP
// server, and one lifecycle context. Each group has its own Raft log, leader,
// and storage directory.
//
// Use Manager when you want to shard your data across independent Raft groups
// (e.g. for horizontal scalability). For a single group, use [NewStore] directly.
type Manager struct {
	mu     sync.RWMutex
	stores map[uint64]*Store
	mgr    *raft.Manager
	cfg    Config

	transport  *grpctransport.GRPCTransport
	httpServer *http.Server
	cancel     context.CancelFunc
	stopCtx    context.Context
}

// managedGroup pairs a group ID with its store for iteration outside the lock.
type managedGroup struct {
	id    uint64
	store *Store
}

// NewManager creates a new EasyRaft manager.
func NewManager(opts ...Option) (*Manager, error) {
	var c Config
	for _, o := range opts {
		o(&c)
	}

	if c.ID == "" {
		return nil, fmt.Errorf("easyraft: WithID is required for Manager")
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		stores:  make(map[uint64]*Store),
		mgr:     raft.NewManager(),
		cfg:     c,
		stopCtx: ctx,
		cancel:  cancel,
	}, nil
}

// logger returns the configured logger, or slog.Default() so failures are
// reported rather than dropped.
func (m *Manager) logger() *slog.Logger {
	if m.cfg.Logger != nil {
		return m.cfg.Logger
	}
	return slog.Default()
}

// newStoreShell builds a Store with every internal structure initialised but
// no Raft node yet. Both [NewStore] and [Manager.AddStore] go through it so
// the two construction paths cannot drift apart.
func newStoreShell(stopCtx context.Context, cancel context.CancelFunc, cfg *Config) *Store {
	return &Store{
		collections:     make(map[string]map[string]json.RawMessage),
		mutations:       make(map[string]map[string]mutationFunc),
		raftPeers:       make(map[raft.NodeID]raftPeerInfo),
		onChangeFns:     make(map[string]func(rawChangeEvent)),
		notifyCh:        make(chan changeEvent, notifyQueueDepth),
		pendingGaps:     make(map[string]struct{}),
		discoveredAddrs: make(map[raft.NodeID]string),
		cfg:             *cfg,
		stopCtx:         stopCtx,
		cancel:          cancel,
	}
}

// AddStore creates a Store for the given Raft group ID and registers it with
// the Manager. The store inherits the Manager's NodeID, transport, and
// lifecycle context, but has its own storage directory and Raft consensus.
// [WithDataDir] is required on the store-level options.
// Store-level options override Manager-level options.
// AddStore must be called before [Manager.Start].
func (m *Manager) AddStore(groupID uint64, opts ...Option) (*Store, error) {
	// Merge manager-level options with store-specific options.
	// Store-specific options (like DataDir) take precedence.
	mergedCfg := m.cfg
	for _, o := range opts {
		o(&mergedCfg)
	}

	if mergedCfg.DataDir == "" {
		return nil, fmt.Errorf("easyraft: WithDataDir is required for Store %d", groupID)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.stores[groupID]; exists {
		return nil, fmt.Errorf("easyraft: store %d already exists", groupID)
	}

	// Give each store its own derived context so RemoveStore can cancel its
	// dispatchChanges goroutine without stopping the entire Manager.
	stopCtx, cancel := context.WithCancel(m.stopCtx)

	// We don't call initRaft here because it needs the shared transport.
	// We'll initialize all stores in Manager.Start.
	s := newStoreShell(stopCtx, cancel, &mergedCfg)
	s.cfg.ID = m.cfg.ID // NodeID is shared across all groups; GroupID comes from the argument.

	m.stores[groupID] = s
	return s, nil
}

// Start initialises the shared gRPC transport, starts all registered stores,
// and (if WithHTTPAddr was set) starts the shared HTTP server.
// All stores must be added via [Manager.AddStore] before calling Start.
//
// If Start returns an error, all partially-initialised resources (nodes,
// goroutines, transport) are cleaned up; the caller does not need to call Stop.
//
// The Manager lock is not held while groups join their clusters, so
// [Manager.GetStore] and the HTTP handlers stay responsive even when a join
// spends its full retry budget waiting for a seed to come up.
func (m *Manager) Start() error {
	m.mu.Lock()
	if m.cfg.RaftAddr == "" {
		m.mu.Unlock()
		return fmt.Errorf("easyraft: WithRaftAddr is required")
	}
	groups := make([]managedGroup, 0, len(m.stores))
	for groupID, s := range m.stores {
		groups = append(groups, managedGroup{id: groupID, store: s})
	}
	m.mu.Unlock()

	// Start groups in a stable order so a failure is reproducible.
	slices.SortFunc(groups, func(a, b managedGroup) int {
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		default:
			return 0
		}
	})

	// 1. Shared Transport
	var trOpts []grpctransport.Option
	if m.cfg.TLS != nil {
		trOpts = append(trOpts, grpctransport.WithTLSConfig(m.cfg.TLS))
	}
	tr, err := grpctransport.Listen(m.cfg.RaftAddr, trOpts...)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}
	m.mu.Lock()
	m.transport = tr
	m.mu.Unlock()
	tr.SetGroupLookup(m.mgr.Lookup)

	// cleanup tears down all resources initialised so far; called on any error.
	cleanup := func() {
		m.cancel()      // cancels all per-store derived contexts (dispatchChanges)
		m.mgr.StopAll() // stops any Raft nodes that were started
		for _, g := range groups {
			if g.store.storage != nil {
				if c, ok := g.store.storage.(interface{ Close() error }); ok {
					_ = c.Close()
				}
			}
		}
		_ = tr.Close()
		m.mu.Lock()
		m.transport = nil
		m.mu.Unlock()
	}

	// 2. Initialize and Start all Stores
	for _, g := range groups {
		if err := g.store.initRaftForManager(g.id, tr); err != nil {
			cleanup()
			return fmt.Errorf("init store %d: %w", g.id, err)
		}
		if err := m.mgr.Add(g.id, g.store.node); err != nil {
			cleanup()
			return fmt.Errorf("register store %d: %w", g.id, err)
		}
		// Join an existing cluster before starting the event loop, if configured.
		// This can retry for up to 30 seconds, which is exactly why the Manager
		// lock is not held here.
		if len(g.store.cfg.JoinAddrs) > 0 {
			if err := g.store.joinCluster(g.store.stopCtx); err != nil {
				g.store.logger().Warn("easyraft: cluster join failed", "group", g.id, "err", err)
			}
		}
		g.store.node.Start()
		go g.store.dispatchChanges()
		// Advertise this node's HTTP address so the cluster can redirect clients.
		if g.store.cfg.HTTPAddr != "" {
			go g.store.advertiseMetadata()
		}
	}

	// 3. Shared HTTP Server (Multi-Raft capable)
	if m.cfg.HTTPAddr != "" {
		if err := m.serveHTTP(); err != nil {
			cleanup()
			return err
		}
	}

	return nil
}

// Stop shuts down all stores, the shared HTTP server, and the shared transport.
func (m *Manager) Stop() {
	m.cancel()

	m.mu.Lock()
	httpServer := m.httpServer
	transport := m.transport
	m.transport = nil
	m.mu.Unlock()

	if httpServer != nil {
		_ = httpServer.Shutdown(context.Background())
	}
	m.mgr.StopAll()
	if transport != nil {
		_ = transport.Close()
	}
}

// GetStore returns the Store registered under groupID, or an error if no store
// is registered for that ID.
func (m *Manager) GetStore(groupID uint64) (*Store, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.stores[groupID]
	if !ok {
		return nil, fmt.Errorf("easyraft: group %d not found", groupID)
	}
	return s, nil
}

// GroupIDs returns a sorted snapshot of all group IDs currently registered
// with the Manager. Additions and removals after the call are not reflected.
func (m *Manager) GroupIDs() []uint64 {
	return m.mgr.GroupIDs()
}

// StatusAll returns a point-in-time snapshot of every registered group's
// Raft state, ordered by group ID. Groups that have been added but not yet
// started are omitted.
//
// The snapshot is taken from the Manager's own registry rather than the
// underlying raft.Manager, so the two cannot disagree about which groups this
// node owns.
// ctx is accepted so that this satisfies raft.NodeProvider, whose StatusAll
// may reach a node over a network; this implementation is local and returns
// immediately.
func (m *Manager) StatusAll(_ context.Context) []raft.GroupStatus {
	m.mu.RLock()
	groups := make([]managedGroup, 0, len(m.stores))
	for groupID, s := range m.stores {
		groups = append(groups, managedGroup{id: groupID, store: s})
	}
	m.mu.RUnlock()

	slices.SortFunc(groups, func(a, b managedGroup) int {
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		default:
			return 0
		}
	})

	out := make([]raft.GroupStatus, 0, len(groups))
	for _, g := range groups {
		if g.store.node == nil {
			continue
		}
		status := g.store.node.Status()
		status.GroupID = g.id
		out = append(out, status)
	}
	return out
}

// RemoveStore stops and unregisters the Store for groupID, closing its
// storage. Returns an error if no store is registered for that ID.
// Unlike [Manager.Stop], RemoveStore affects only one Raft group; the others
// continue running.
//
// For production use, prefer [Manager.RemoveStoreGraceful] which attempts a
// leadership transfer before stopping the node, preventing an election gap.
func (m *Manager) RemoveStore(groupID uint64) error {
	m.mu.Lock()
	s, ok := m.stores[groupID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("easyraft: group %d not found", groupID)
	}
	delete(m.stores, groupID)
	m.mu.Unlock()

	// Cancel the store's context to stop its dispatchChanges goroutine.
	if s.cancel != nil {
		s.cancel()
	}
	// Stop the Raft node via the underlying raft.Manager (also unregisters it).
	_ = m.mgr.Remove(groupID)
	// Close persistent storage.
	if s.storage != nil {
		if closer, ok := s.storage.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
	return nil
}

// RemoveStoreGraceful attempts a leadership transfer to transferTo before
// stopping the Store for groupID. If the group is not the leader or the
// transfer cannot complete before ctx expires, it falls through to an
// unconditional stop. Returns an error if no store is registered for that ID.
func (m *Manager) RemoveStoreGraceful(ctx context.Context, groupID uint64, transferTo raft.NodeID) error {
	m.mu.Lock()
	s, ok := m.stores[groupID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("easyraft: group %d not found", groupID)
	}
	delete(m.stores, groupID)
	m.mu.Unlock()

	// Cancel the store's context to stop its dispatchChanges goroutine.
	if s.cancel != nil {
		s.cancel()
	}
	// Delegate graceful removal (leadership transfer + node stop) to raft.Manager.
	removeErr := m.mgr.RemoveGraceful(ctx, groupID, transferTo)
	// Close persistent storage regardless of whether the graceful transfer succeeded.
	if s.storage != nil {
		if closer, ok := s.storage.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
	return removeErr
}

// TransferGroupLeadership asks the node managing groupID to transfer
// leadership to the peer identified by to. Returns an error if groupID is not
// registered. Use this to rebalance leaders across physical nodes without
// stopping any group.
func (m *Manager) TransferGroupLeadership(ctx context.Context, groupID uint64, to raft.NodeID) error {
	return m.mgr.TransferGroupLeadership(ctx, groupID, to)
}

// initRaftForManager is a modified initRaft that uses a shared transport.
func (s *Store) initRaftForManager(groupID uint64, tr raft.Transport) error {
	// Storage
	if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	st, err := filestore.Open(s.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	s.storage = st

	// Peer setup for this group. Recording each peer in raftPeers is what lets
	// GET /members and the join response report a Raft address for statically
	// configured peers — without it a joiner has no way to dial them.
	var peerConfigs []raft.PeerConfig
	adder, hasAdder := tr.(peerAdder)
	s.mu.Lock()
	for id, addr := range s.cfg.Peers {
		if id == s.cfg.ID {
			continue
		}
		if hasAdder {
			adder.AddPeer(id, addr)
		}
		peerConfigs = append(peerConfigs, raft.PeerConfig{ID: id, Voter: true})
		s.raftPeers[id] = raftPeerInfo{addr: addr, voter: true}
	}
	s.mu.Unlock()

	// Raft Config
	rCfg := raft.DefaultConfig()
	rCfg.ID = s.cfg.ID
	rCfg.GroupID = groupID
	rCfg.Peers = peerConfigs
	rCfg.Transport = tr
	rCfg.Storage = st
	rCfg.StateMachine = s
	rCfg.Logger = s.cfg.Logger
	rCfg.TickInterval = s.raftTickInterval() // Or 0 to use Manager.RunTicker
	rCfg.ElectionTimeoutMin = s.raftElectionTimeoutMin()
	rCfg.ElectionTimeoutMax = s.raftElectionTimeoutMax()
	rCfg.HeartbeatInterval = s.raftHeartbeatInterval()
	rCfg.SnapshotThreshold = s.cfg.SnapCount
	if rCfg.SnapshotThreshold == 0 {
		rCfg.SnapshotThreshold = 1000
	}

	// Every group shares the caller's registry, so the collectors are
	// registered once and each group's series carry its own "group" label.
	if s.cfg.PromRegisterer != nil {
		rCfg.Metrics = prommetrics.NewForGroup(s.cfg.PromRegisterer, groupID)
	}

	node, err := raft.New(&rCfg)
	if err != nil {
		return fmt.Errorf("new raft node: %w", err)
	}

	s.node = node
	s.reader = node
	s.transport = tr

	// Discovery: wire both transport connectivity and Raft membership, using
	// the Manager's shared stop context so teardown is coordinated.
	if s.cfg.Discovery != nil {
		if pa, ok := tr.(peerAdder); ok {
			s.startDiscovery(node, pa)
		}
	}

	return nil
}
