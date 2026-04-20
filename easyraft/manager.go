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

	// We don't call initRaft here because it needs the shared transport.
	// We'll initialize all stores in Manager.Start.
	s := &Store{
		collections: make(map[string]map[string]json.RawMessage),
		mutations:   make(map[string]map[string]MutationFunc),
		cfg:         mergedCfg,
		stopCtx:     m.stopCtx,
	}
	s.cfg.ID = m.cfg.ID // NodeID is shared across all groups; GroupID comes from the argument.

	m.stores[groupID] = s
	return s, nil
}

// Start initialises the shared gRPC transport, starts all registered stores,
// and (if WithHTTPAddr was set) starts the shared HTTP server.
// All stores must be added via [Manager.AddStore] before calling Start.
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

	// 2. Initialize and Start all Stores
	for groupID, s := range m.stores {
		if err := s.initRaftForManager(groupID, tr); err != nil {
			return fmt.Errorf("init store %d: %w", groupID, err)
		}
		if err := m.mgr.Add(groupID, s.node); err != nil {
			return fmt.Errorf("register store %d: %w", groupID, err)
		}
		s.node.Start()
	}

	// 3. Shared HTTP Server (Multi-Raft capable)
	if m.cfg.HTTPAddr != "" {
		if err := m.serveHTTP(); err != nil {
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
