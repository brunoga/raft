package easyraft

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
	s := &Store{
		collections: make(map[string]map[string]json.RawMessage),
		mutations:   make(map[string]map[string]mutationFunc),
		raftPeers:   make(map[raft.NodeID]raftPeerInfo),
		onChangeFns: make(map[string]func(string, json.RawMessage, bool)),
		notifyCh:    make(chan changeEvent, 1024),
		cfg:         mergedCfg,
		stopCtx:     stopCtx,
		cancel:      cancel,
	}
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
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Shared Transport
	if m.cfg.RaftAddr == "" {
		return fmt.Errorf("easyraft: WithRaftAddr is required")
	}
	var trOpts []grpctransport.Option
	if m.cfg.TLS != nil {
		trOpts = append(trOpts, grpctransport.WithTLSConfig(m.cfg.TLS))
	}
	tr, err := grpctransport.Listen(m.cfg.RaftAddr, trOpts...)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}
	m.transport = tr
	tr.SetGroupLookup(m.mgr.Lookup)

	// cleanup tears down all resources initialised so far; called on any error.
	cleanup := func() {
		m.cancel()      // cancels all per-store derived contexts (dispatchChanges)
		m.mgr.StopAll() // stops any Raft nodes that were started
		for _, s := range m.stores {
			if s.storage != nil {
				if c, ok := s.storage.(interface{ Close() error }); ok {
					_ = c.Close()
				}
			}
		}
		_ = tr.Close()
		m.transport = nil
	}

	// 2. Initialize and Start all Stores
	for groupID, s := range m.stores {
		if err := s.initRaftForManager(groupID, tr); err != nil {
			cleanup()
			return fmt.Errorf("init store %d: %w", groupID, err)
		}
		if err := m.mgr.Add(groupID, s.node); err != nil {
			cleanup()
			return fmt.Errorf("register store %d: %w", groupID, err)
		}
		// Join an existing cluster before starting the event loop, if configured.
		if len(s.cfg.JoinAddrs) > 0 {
			if err := s.joinCluster(s.stopCtx); err != nil && s.cfg.Logger != nil {
				s.cfg.Logger.Warn("easyraft: cluster join failed", "err", err)
			}
		}
		s.node.Start()
		go s.dispatchChanges()
		// Advertise this node's HTTP address so the cluster can redirect clients.
		if s.cfg.HTTPAddr != "" {
			go s.advertiseMetadata()
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
	if m.httpServer != nil {
		_ = m.httpServer.Shutdown(context.Background())
	}
	m.mgr.StopAll()
	if m.transport != nil {
		_ = m.transport.Close()
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
// Raft state. The slice order is not guaranteed.
func (m *Manager) StatusAll() []raft.GroupStatus {
	return m.mgr.StatusAll()
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

	// Peer setup for this group
	var peerConfigs []raft.PeerConfig
	for id, addr := range s.cfg.Peers {
		if id != s.cfg.ID {
			if adder, ok := tr.(interface{ AddPeer(raft.NodeID, string) }); ok {
				adder.AddPeer(id, addr)
			}
			peerConfigs = append(peerConfigs, raft.PeerConfig{ID: id, Voter: true})
		}
	}

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

	if s.cfg.PromRegisterer != nil {
		rCfg.Metrics = prommetrics.New(s.cfg.PromRegisterer)
	}

	node, err := raft.New(&rCfg)
	if err != nil {
		return fmt.Errorf("new raft node: %w", err)
	}

	s.node = node
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
