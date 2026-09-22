package easyraft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/metrics/prommetrics"
	"github.com/brunoga/raft/v2/storage/filestore"
	"github.com/brunoga/raft/v2/storage/sharedwal"
	"github.com/brunoga/raft/v2/transport/grpctransport"
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
	cfg    config

	transport  *grpctransport.GRPCTransport
	wal        *sharedwal.WAL
	httpServer *http.Server
	cancel     context.CancelFunc
	stopCtx    context.Context
}

// SharedWAL returns the write-ahead log every group on this Manager appends
// to, or nil when [WithSharedWAL] was not given.
//
// It is the handle for the things only the log knows: Groups lists what it
// holds, which is how a host that runs a changing set of groups finds them
// again after a restart; Remove forgets a group that has been decommissioned
// so its space can be reclaimed; Reclaim runs that reclamation on demand.
// Removing a store from the Manager deliberately does not remove its log --
// a group taken off a host is usually coming back.
func (m *Manager) SharedWAL() *sharedwal.WAL {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.wal
}

// groupStorage returns the storage a group should use, or nil to let it open
// its own directory.
func (m *Manager) groupStorage(groupID uint64) raft.Storage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.wal == nil {
		return nil
	}
	return m.wal.Storage(groupID)
}

// managedGroup pairs a group ID with its store for iteration outside the lock.
type managedGroup struct {
	id    uint64
	store *Store
}

// NewManager creates a new EasyRaft manager.
func NewManager(opts ...Option) (*Manager, error) {
	var c config
	for _, o := range opts {
		o(&c)
	}

	if c.ID == "" {
		return nil, fmt.Errorf("easyraft: WithID is required for Manager")
	}
	if err := validateSecurity(&c, true); err != nil {
		return nil, err
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
func newStoreShell(stopCtx context.Context, cancel context.CancelFunc, cfg *config) *Store {
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

	// With a shared log there is nothing for a per-group directory to hold:
	// the log lives at the Manager's own DataDir and every group is a
	// partition of it.
	if mergedCfg.DataDir == "" && !m.cfg.SharedWAL {
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

	// 1. Shared write-ahead log, when every group is to write to one.
	if m.cfg.SharedWAL {
		if m.cfg.DataDir == "" {
			return fmt.Errorf("easyraft: WithSharedWAL needs WithDataDir on the Manager: " +
				"the log every group shares has to live somewhere")
		}
		wal, walErr := sharedwal.Open(m.cfg.DataDir)
		if walErr != nil {
			return fmt.Errorf("open shared wal: %w", walErr)
		}
		m.mu.Lock()
		m.wal = wal
		m.mu.Unlock()
	}

	// 2. Shared Transport
	warnIfPeersUnauthorized(&m.cfg, m.logger())
	tr, err := grpctransport.Listen(m.cfg.RaftAddr, transportOptions(&m.cfg)...)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}
	m.mu.Lock()
	m.transport = tr
	m.mu.Unlock()
	tr.SetGroupLookup(m.mgr.Lookup)

	// cleanup tears down all resources initialised so far; called on any error.
	//
	// Errors from the teardown itself are discarded: Start already has the
	// error that made it fail, and that is the one the caller needs. Each
	// store's storage handle is cleared as it is closed so a caller that calls
	// Stop anyway does not close it a second time.
	cleanup := func() {
		m.cancel()      // cancels all per-store derived contexts (dispatchChanges)
		m.mgr.StopAll() // stops any Raft nodes that were started
		for _, g := range groups {
			if g.store.storage != nil {
				if c, ok := g.store.storage.(io.Closer); ok {
					_ = c.Close()
				}
				g.store.storage = nil
			}
			// The transport is shared and closed once, just below; drop the
			// store's reference so a later Stop cannot close it again.
			g.store.transport = nil
		}
		_ = tr.Close()
		m.mu.Lock()
		if m.wal != nil {
			_ = m.wal.Close()
			m.wal = nil
		}
		m.transport = nil
		m.mu.Unlock()
	}

	// 2. Initialize and Start all Stores
	for _, g := range groups {
		if err := g.store.initRaftForManager(g.id, tr, m.groupStorage(g.id)); err != nil {
			cleanup()
			return fmt.Errorf("init store %d: %w", g.id, err)
		}
		// Join an existing cluster before starting the event loop, if configured.
		// This can retry for up to 30 seconds, which is exactly why the Manager
		// lock is not held here.
		//
		// A group that was told to join and did not is not a replica of that
		// group, so it fails Start for the same reason [Store.Start] does — and
		// it fails the whole Manager, because a node silently missing one of
		// its shards is the kind of partial start an operator has no way to
		// notice.
		//
		// The join runs before the node is registered with the underlying
		// raft.Manager, so a group that fails here leaves no registered but
		// unstarted node behind: cleanup stops every node it finds registered,
		// and Node.Stop waits for goroutines that only Node.Start creates.
		if len(g.store.cfg.JoinAddrs) > 0 {
			if err := g.store.joinCluster(g.store.stopCtx); err != nil {
				cleanup()
				return fmt.Errorf("easyraft: group %d did not join the cluster: %w", g.id, err)
			}
		}
		if err := m.mgr.Add(g.id, g.store.node); err != nil {
			cleanup()
			return fmt.Errorf("register store %d: %w", g.id, err)
		}
		g.store.node.Start()
		// Record that this group's event loop is running, so a later
		// Store-level Stop stops the node instead of mistaking the store for
		// one that was never started and closing the shared transport.
		g.store.started.Store(true)
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
//
// Like [Store.Stop], every step runs whatever the earlier ones reported, and
// the returned error joins the failures so the caller can act on them instead
// of finding them in a log. The one that matters most is a group whose storage
// did not close cleanly: that group's on-disk log may not be intact, which
// decides whether this node can be restarted or has to be rebuilt from a peer.
//
// Stopping the Raft nodes and closing each group's storage go through the
// stores themselves, so a group that was already shut down individually is not
// torn down twice.
func (m *Manager) Stop() error {
	m.cancel()

	m.mu.Lock()
	httpServer := m.httpServer
	transport := m.transport
	wal := m.wal
	m.transport = nil
	m.wal = nil
	stores := make([]*Store, 0, len(m.stores))
	for _, s := range m.stores {
		stores = append(stores, s)
	}
	m.mu.Unlock()

	var errs []error

	if httpServer != nil {
		if err := httpServer.Shutdown(context.Background()); err != nil {
			errs = append(errs, fmt.Errorf("easyraft: shut down manager http server: %w", err))
		}
	}

	// Shut the groups down through their own stores, so each one stops its
	// node and closes its storage exactly once, and so a group configured with
	// [WithLeaveOnStop] departs while its node is still able to propose the
	// membership change.
	for _, s := range stores {
		if err := s.Stop(); err != nil {
			errs = append(errs, err)
		}
	}

	// Catch any node registered with the underlying raft.Manager that no store
	// owns. Node.Stop is idempotent, so the groups just handled are unaffected.
	m.mgr.StopAll()

	if transport != nil {
		if err := transport.Close(); err != nil {
			errs = append(errs, fmt.Errorf("easyraft: close manager transport: %w", err))
		}
	}

	// Last, and only here: the log is shared, so a group closing its own view
	// of it closes nothing. Every node has stopped by now, so there is no
	// write left that could arrive after it.
	if wal != nil {
		if err := wal.Close(); err != nil {
			errs = append(errs, fmt.Errorf("easyraft: close shared wal: %w", err))
		}
	}

	return errors.Join(errs...)
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
	// A failure here means the group was never registered — it was added but
	// the Manager was never started — which is not something the caller asked
	// about: the group is gone from this Manager either way.
	_ = m.mgr.Remove(groupID)
	// Close persistent storage. A failure is worth reporting: this group's
	// on-disk log may not be intact.
	if s.storage != nil {
		if closer, ok := s.storage.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				return fmt.Errorf("easyraft: close storage for group %d: %w", groupID, err)
			}
		}
		s.storage = nil
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
	// Close persistent storage regardless of whether the graceful transfer
	// succeeded, and report a close failure alongside it: either one on its own
	// is something the caller may need to act on.
	var closeErr error
	if s.storage != nil {
		if closer, ok := s.storage.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				closeErr = fmt.Errorf("easyraft: close storage for group %d: %w", groupID, err)
			}
		}
		s.storage = nil
	}
	return errors.Join(removeErr, closeErr)
}

// TransferGroupLeadership asks the node managing groupID to transfer
// leadership to the peer identified by to. Returns an error if groupID is not
// registered. Use this to rebalance leaders across physical nodes without
// stopping any group.
func (m *Manager) TransferGroupLeadership(ctx context.Context, groupID uint64, to raft.NodeID) error {
	return m.mgr.TransferGroupLeadership(ctx, groupID, to)
}

// initRaftForManager is a modified initRaft that uses a shared transport.
func (s *Store) initRaftForManager(groupID uint64, tr raft.Transport, st raft.Storage) error {
	// Storage. A shared log hands one in; without it each group opens its own
	// directory.
	//
	// filestore.Open creates the directory itself, and makes the creation
	// durable by fsyncing the parents it had to create. Creating it here first
	// would leave the store nothing to create, so that durability would be
	// skipped and the directory's own name would never be fsynced.
	if st == nil {
		fs, err := filestore.Open(s.cfg.DataDir)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		st = fs
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
		peerConfigs = append(peerConfigs, raft.PeerConfig{
			ID: id, Voter: true, Witness: s.cfg.Witnesses[id],
		})
		s.raftPeers[id] = raftPeerInfo{addr: addr, voter: true}
	}
	s.mu.Unlock()

	// Raft config
	rCfg := s.raftConfig(peerConfigs, tr, st)
	rCfg.GroupID = groupID

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
