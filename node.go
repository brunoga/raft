package raft

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// ErrStopped is returned when an operation is attempted on a stopped node.
var ErrStopped = errors.New("raft: node is stopped")

// proposeMsg carries a client command into the event loop.
type proposeMsg struct {
	cmd    []byte
	respCh chan<- result[[]byte]
}

// rpcEnvelope wraps any inbound RPC together with a one-shot response channel.
// The Handler methods use this to hand work to the event-loop goroutine.
type rpcEnvelope struct {
	req    any
	respCh chan rpcResponse
}

type rpcResponse struct {
	resp any
	err  error
}

// applyResult is sent by the apply goroutine to the event loop after each
// state-machine application so that promise resolution stays in one goroutine.
// configCmd is non-nil when the applied entry was a cluster-membership change;
// in that case val is always nil and err is always nil (config entries are not
// forwarded to the user state machine).
// cmd is the raw log entry command; it is used by the event loop to update the
// client dedup table for ProposeOnce entries.
type applyResult struct {
	index     Index
	val       []byte
	err       error
	configCmd []byte // non-nil iff this was a config-change entry
	cmd       []byte // raw command (always set; used for dedup tracking)
}

// Node is a single participant in a Raft cluster.
// All exported methods are safe for concurrent use.
//
// Lifecycle: New → Start → (Tick drives it) → Stop.
type Node struct {
	cfg    Config
	logger *slog.Logger

	// --- Persistent state (cached; written via saveTerm) --------------------
	currentTerm Term
	votedFor    NodeID

	// --- Volatile state (all servers) ---------------------------------------
	// state and leaderID are written only by the event-loop goroutine.
	// They are also stored in the atomic mirrors below so that external
	// callers (StateSnapshot, Leader) can read them without a data race.
	state       State
	leaderID    NodeID
	commitIndex Index
	lastApplied Index

	// Atomic mirrors for safe external reads.
	atomicState        atomic.Uint32 // mirrors state
	atomicLeader       atomic.Value  // mirrors leaderID; stores string
	atomicLastApplied  atomic.Uint64 // mirrors lastApplied
	atomicCommitIndex  atomic.Uint64 // mirrors commitIndex
	atomicTerm         atomic.Uint64 // mirrors currentTerm
	// atomicPeers mirrors cfg.Peers as a []NodeID snapshot; updated by
	// applyConfigChange (event-loop only). Read by ReconfigureCluster outside
	// the event loop to avoid a data race on cfg.Peers.
	atomicPeers atomic.Value // stores []NodeID

	// --- Volatile state (leader only; nil when not leader) ------------------
	nextIndex  map[NodeID]Index
	matchIndex map[NodeID]Index
	// inflight tracks the number of outstanding AppendEntries RPCs per peer
	// so we can honour MaxInflightRPCs.
	inflight map[NodeID]int
	// snapshotInflight counts concurrent InstallSnapshot transfers in progress.
	// A simple counter is enough since we already have snapshotting bool to
	// prevent sending the same snapshot twice to the same peer.
	snapshotInflight map[NodeID]bool

	// --- Log ----------------------------------------------------------------
	log *raftLog

	// --- Timing (in ticks) --------------------------------------------------
	electionElapsed  int
	heartbeatElapsed int
	electionTimeout  int // randomised per election, in ticks
	heartbeatTimeout int // fixed, in ticks
	electionMinTicks int
	electionMaxTicks int

	// --- Membership change state (all nodes) --------------------------------
	// pendingConfigIndex is the log index of an in-flight config-change entry.
	// Zero means no change is pending. The leader uses this to prevent a second
	// change from being proposed before the first one commits; followers track it
	// so they can apply changes in order.
	pendingConfigIndex Index

	// jointOld and jointNew are non-nil while a joint-consensus reconfiguration
	// is in progress: jointOld holds the previous membership and jointNew holds
	// the target membership (peers only, self excluded). Both are nil outside of
	// joint consensus (i.e. in normal single-membership operation and after the
	// finalise entry is applied). Commits during joint consensus require a
	// majority from BOTH lists.
	// jointIncludeSelf is true when this node's own ID is present in the new
	// membership (encoded in the joint log entry). The finalise entry will
	// include self iff this flag is set. This allows a leader to initiate its
	// own removal by omitting itself from the new membership.
	jointOld         []NodeID
	jointNew         []NodeID
	jointIncludeSelf bool

	// --- Check-quorum state (leader only) -----------------------------------
	// leaderQuorumElapsed counts ticks since quorumAcks was last reset.
	// When CheckQuorum is enabled and this reaches electionTimeout without a
	// quorum of peers having acked, the leader steps down.
	leaderQuorumElapsed int
	// quorumAcks collects the set of peers that sent a successful AppendEntries
	// response during the current check-quorum window. Self is not included; it
	// is counted implicitly when evaluating quorum.
	quorumAcks map[NodeID]bool

	// --- Leadership transfer state (leader only) ----------------------------
	transferTarget  NodeID // non-empty while a transfer is in progress
	transferElapsed int    // ticks since transfer was initiated

	// transferCh carries TransferLeadership requests into the event loop.
	transferCh chan leadershipTransferMsg

	// --- Event-loop channels ------------------------------------------------
	tickCh    chan struct{}
	rpcCh     chan rpcEnvelope
	proposeCh chan proposeMsg
	stopCh    chan struct{}
	doneCh    chan struct{}

	// stopCtx is cancelled when Stop() is called. Passed to StateMachine
	// operations so they can abort on node shutdown.
	stopCtx    context.Context
	stopCancel context.CancelFunc
	// applyDoneCh is closed by applyLoop when it exits, allowing Stop() to
	// wait for graceful shutdown of the apply goroutine.
	applyDoneCh chan struct{}

	// applyBaseIndex is the snapshot index at node creation time. It seeds
	// the apply goroutine's localLastApplied without racing against later
	// event-loop writes to n.log.snapMeta. Set once in New(); never mutated.
	applyBaseIndex Index

	// --- Apply-loop channels ------------------------------------------------
	commitNotifyCh chan Index       // event loop → apply goroutine
	applyResultCh  chan applyResult // apply goroutine → event loop
	// applyAdvancedCh is signalled (non-blocking, size-1) whenever
	// atomicLastApplied advances. waitApplied listens on it instead of
	// polling with time.After, eliminating busy-wait latency overhead.
	applyAdvancedCh chan struct{}

	// --- Pending proposals (leader only) ------------------------------------
	pending map[Index]promise[[]byte]

	// --- Synchronisation ----------------------------------------------------
	startOnce sync.Once
	stopOnce  sync.Once

	// --- Vote counting (Candidate/PreCandidate only; reset on state change) -
	// receivedVoteSet tracks which peers granted their vote in the current
	// election round so that joint-consensus quorum can be evaluated per config
	// group rather than against the union alone.
	receivedVoteSet map[NodeID]bool

	// --- Snapshot state -----------------------------------------------------
	// snapshotTriggerCh carries snapshot-trigger metadata from the event loop
	// to applyLoop. applyLoop calls StateMachine.Snapshot() after completing
	// the current Apply, guaranteeing that Snapshot and Apply never overlap.
	// Size-1; the snapshotting bool prevents sending while one is in flight.
	snapshotTriggerCh chan snapshotTrigger
	// snapshotResultCh receives the completed snapshot from applyLoop. It is
	// separate from rpcCh so we can add it directly to the run() select
	// without going through the RPC envelope machinery.
	snapshotResultCh chan snapshotResult
	snapshotting     bool // true while a snapshot is in progress

	// pendingSnap holds chunks of an in-progress multi-chunk snapshot install
	// on this follower. It is populated by handleInstallSnapshot as chunks
	// arrive and consumed (then nil'd) when the final chunk (Done=true) lands.
	// Cleared on becomeFollower so a stale partial install cannot be completed
	// by a message from a previous leader after an election.
	pendingSnap *partialSnapshot

	// snapshotInstallWg tracks all live runSnapshotInstall goroutines.
	// Stop() waits on this WaitGroup so that those goroutines — which hold
	// references to the storage backend and rpcCh — have fully exited before
	// the node is considered stopped.
	snapshotInstallWg sync.WaitGroup

	// initialSnap, if non-nil, holds the snapshot loaded from storage in New().
	// applyLoop restores the state machine from it on its first iteration and
	// never touches it again. Set once before Start(); the event loop never
	// reads it, so there is no data race.
	initialSnap *snapshotInstall

	// restoreSnapshotCh lets the event loop signal the apply goroutine to
	// call StateMachine.Restore() and advance its localLastApplied.
	restoreSnapshotCh chan snapshotInstall

	// --- Linearizable read state (leader only; cleared on step-down) --------
	// leaderNopCommitted is true once the leader has committed at least one
	// entry in its own term (the leadership no-op). ReadIndex is unsafe before
	// this: the commitIndex may not yet reflect all entries committed by the
	// previous leader (Raft §8).
	leaderNopCommitted bool
	readBatchGen       uint64              // incremented each time a new barrier is broadcast
	readBatchAcks      map[NodeID]bool     // peers that ACKed the current barrier heartbeat
	readBatchIndex     Index               // commitIndex captured when the batch started
	pendingReads       []readIndexResolver // clients waiting for read-index confirmation
	// leaseExpiry is the wall-clock time until which the leader holds a valid
	// read lease. Zero means no lease. Set in confirmReadBatch; cleared in
	// becomeFollower. Used by ReadIndexLease to skip the heartbeat round-trip.
	leaseExpiry time.Time
	// leaseSendTime is the wall-clock time at which the most recent read-barrier
	// heartbeat was broadcast. The lease is valid for ElectionTimeoutMin from
	// this instant (not from when ACKs arrive), because followers reset their
	// election timers when they receive the heartbeat — not when the leader
	// receives their ACK.
	leaseSendTime time.Time

	// readIndexCh carries ReadIndex requests from callers into the event loop.
	readIndexCh chan readIndexMsg

	// --- Client dedup table (event-loop goroutine only) ---------------------
	// clientTable maps each client's NodeID to the latest (seqNum, result)
	// pair seen. It is persisted inside every snapshot so that leadership
	// hand-offs and restarts do not lose the dedup state. An LRU evicts the
	// least-recently-used client when the table exceeds MaxClientTableSize.
	clientTable *clientLRU

	// --- Heartbeat write-pumps (leader only; one per peer) ------------------
	// hbPumps maps each peer to a size-1 channel used by broadcastHeartbeat
	// to enqueue outbound heartbeat AppendEntries RPCs. A persistent pump
	// goroutine per peer drains the channel, sends the RPC, and posts the
	// appendResult back to rpcCh. This eliminates spawning a new goroutine
	// for every heartbeat tick (~2 per heartbeat interval per peer).
	//
	// The channel is size-1 and sends are non-blocking: if the pump is still
	// sending the previous heartbeat, the new one is dropped (the next tick
	// will retry). This is safe because a missed heartbeat only affects
	// follower election-timer resets — it does not affect safety.
	hbPumps   map[NodeID]chan *AppendEntriesRequest
	hbStopChs map[NodeID]chan struct{} // closed to stop the pump goroutine

	// --- RNG (used only in the event-loop goroutine) ------------------------
	rng *rand.Rand
}

// New creates a Node from cfg, loads persisted state, and caches the log
// boundaries. It does not start any goroutines; call Start for that.
func New(cfg *Config) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default().With("node", string(cfg.ID))
	}

	tickInterval := cfg.TickInterval
	if tickInterval <= 0 {
		tickInterval = 10 * time.Millisecond
	}

	heartbeatTicks := max(1, int(cfg.HeartbeatInterval/tickInterval))
	electionMinTicks := max(2, int(cfg.ElectionTimeoutMin/tickInterval))
	electionMaxTicks := max(electionMinTicks+1, int(cfg.ElectionTimeoutMax/tickInterval))

	rl, err := newRaftLog(cfg.Storage)
	if err != nil {
		return nil, fmt.Errorf("raft.New: %w", err)
	}

	hs, err := cfg.Storage.LoadHardState(context.Background())
	if err != nil {
		return nil, fmt.Errorf("raft.New: load hard state: %w", err)
	}

	n := &Node{
		cfg:         *cfg,
		logger:      logger,
		currentTerm: hs.CurrentTerm,
		votedFor:    hs.VotedFor,
		state:       Follower,
		log:         rl,
		// lastApplied is volatile; seed it from the snapshot boundary so the
		// apply loop does not re-apply already-snapshotted entries.
		lastApplied:       rl.snapMeta.LastIncludedIndex,
		applyBaseIndex:    rl.snapMeta.LastIncludedIndex,
		heartbeatTimeout:  heartbeatTicks,
		electionMinTicks:  electionMinTicks,
		electionMaxTicks:  electionMaxTicks,
		tickCh:            make(chan struct{}, 1),
		rpcCh:             make(chan rpcEnvelope, 1024),
		proposeCh:         make(chan proposeMsg, 1024),
		readIndexCh:       make(chan readIndexMsg, 64),
		transferCh:        make(chan leadershipTransferMsg, 4),
		stopCh:            make(chan struct{}),
		doneCh:            make(chan struct{}),
		applyDoneCh:       make(chan struct{}),
		commitNotifyCh:    make(chan Index, 1),
		applyResultCh:     make(chan applyResult, 256),
		applyAdvancedCh:   make(chan struct{}, 1),
		snapshotTriggerCh: make(chan snapshotTrigger, 1),
		snapshotResultCh:  make(chan snapshotResult, 1),
		restoreSnapshotCh: make(chan snapshotInstall, 1),
		pending:           make(map[Index]promise[[]byte]),
		clientTable:       newClientLRU(cfg.MaxClientTableSize),
		rng:               rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
	n.stopCtx, n.stopCancel = context.WithCancel(context.Background())

	// If a snapshot exists, seed initialSnap so applyLoop can restore the
	// state machine on its first iteration.
	if rl.snapMeta.LastIncludedIndex > 0 {
		n.clientTable.loadFrom(rl.snapClientTable)
		// We don't load the SM data here; applyLoop will call LoadSnapshot.
		n.initialSnap = &snapshotInstall{
			meta:        rl.snapMeta,
			clientTable: rl.snapClientTable,
		}
		rl.snapClientTable = nil // release reference
	}

	// Initialise atomic mirrors so external readers never see a nil value.
	n.atomicState.Store(uint32(Follower))
	n.atomicLeader.Store(string(NodeID("")))
	n.atomicTerm.Store(uint64(n.currentTerm))
	n.atomicLastApplied.Store(uint64(n.lastApplied))
	n.atomicCommitIndex.Store(uint64(n.commitIndex))
	initPeers := make([]NodeID, len(cfg.Peers))
	copy(initPeers, cfg.Peers)
	n.atomicPeers.Store(initPeers)
	n.resetElectionTimeout()

	// Register with the transport so we can receive inbound RPCs.
	n.cfg.Transport.Register(n.cfg.ID, n)

	return n, nil
}

// Start launches the event-loop and apply goroutines.
// If cfg.TickInterval > 0 an internal ticker goroutine is also started.
// For deterministic tests set TickInterval to 0 and call Tick() manually.
func (n *Node) Start() {
	n.startOnce.Do(func() {
		go n.run()
		go n.applyLoop()
		if n.cfg.TickInterval > 0 {
			go n.tickerLoop()
		}
		n.logger.Info("started", "term", n.currentTerm)
	})
}

// Stop signals the node to shut down and waits for all goroutines to exit,
// including the apply goroutine and any in-flight runSnapshotInstall goroutines.
func (n *Node) Stop() {
	n.stopOnce.Do(func() {
		n.stopCancel() // unblock any in-progress StateMachine operations
		close(n.stopCh)
		<-n.doneCh
		<-n.applyDoneCh
		// Wait for any snapshot-install goroutines to exit. stopCancel() already
		// cancelled their contexts, so they exit quickly; we just need to be sure
		// they have released all references before we return.
		n.snapshotInstallWg.Wait()
		n.cfg.Transport.Unregister(n.cfg.ID)
		n.logger.Info("stopped")
	})
}

// Tick advances the node's logical clock by one unit. Called automatically
// by the internal goroutine when TickInterval > 0, or manually in tests.
func (n *Node) Tick() {
	select {
	case n.tickCh <- struct{}{}:
	case <-n.stopCh:
	default:
		// Drop tick if the event loop is already busy. This prevents a single
		// slow node from stalling the shared Manager ticker (Raft §isolation).
	}
}

// Propose submits a command for replication and blocks until the entry is
// applied to the state machine, returning the state machine's result.
// Returns ErrNotLeader if this node is not the leader, or ErrStopped if the
// node has been stopped.
func (n *Node) Propose(ctx context.Context, cmd []byte) ([]byte, error) {
	// Non-blocking pre-check: if stopCh is already closed, return immediately
	// rather than racing with a buffered proposeCh.
	select {
	case <-n.stopCh:
		return nil, ErrStopped
	default:
	}

	respCh := make(chan result[[]byte], 1)
	msg := proposeMsg{cmd: cmd, respCh: respCh}
	select {
	case n.proposeCh <- msg:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.stopCh:
		return nil, ErrStopped
	}
	select {
	case r := <-respCh:
		return r.val, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.stopCh:
		return nil, ErrStopped
	}
}

// ID returns this node's own NodeID as specified in its Config.
// Safe for concurrent use.
func (n *Node) ID() NodeID {
	return n.cfg.ID
}

// StateSnapshot returns the current role of this node. Safe for concurrent
// use; reads from the atomic mirror that the event loop keeps in sync.
func (n *Node) StateSnapshot() State {
	return State(n.atomicState.Load())
}

// Leader returns the NodeID of the node this node believes to be the current
// leader, or empty string if unknown. Safe for concurrent use.
func (n *Node) Leader() NodeID {
	return NodeID(n.atomicLeader.Load().(string))
}

// ProposeOnce submits a command for replication with exactly-once semantics.
// clientID identifies the issuing client and seqNum is a per-client
// monotonically increasing sequence number. If the leader already has a result
// cached for (clientID, seqNum) it is returned immediately without
// re-appending. If seqNum is older than the latest recorded sequence for this
// client, ErrObsoleteSeqNum is returned.
//
// The caller is responsible for choosing seqNums correctly: a new, never-seen
// seqNum triggers a normal propose; the same seqNum retried after a timeout
// returns the cached result idempotently.
func (n *Node) ProposeOnce(ctx context.Context, clientID NodeID, seqNum uint64, cmd []byte) ([]byte, error) {
	return n.Propose(ctx, encodeDedupCmd(clientID, seqNum, cmd))
}

// ReadIndex requests a linearizable read-index from the leader and waits for
// the local state machine to apply up to that index before returning. It
// blocks until:
//   - (leader path) a heartbeat quorum confirms this node is still leader, and
//     the local state machine has applied up to the commit index, or
//   - (follower path) the leader has been queried for the current commitIndex,
//     and the local state machine has applied up to that index.
//
// Because ReadIndex waits for the state machine to catch up, callers can read
// from their local state machine immediately after ReadIndex returns and
// observe a linearizable snapshot.
//
// Returns ErrStopped if the node has been stopped.
func (n *Node) ReadIndex(ctx context.Context) (Index, error) {
	select {
	case <-n.stopCh:
		return 0, ErrStopped
	default:
	}

	// Fast path: if we are a follower and we know the leader, forward the RPC.
	// We do this outside the event loop for better concurrency.
	if n.StateSnapshot() != Leader {
		leaderID := n.Leader()
		if leaderID == "" {
			return 0, &NotLeaderError{}
		}
		req := &ReadIndexRequest{GroupID: n.cfg.GroupID, Term: n.Term()}
		resp, err := n.cfg.Transport.ReadIndex(ctx, leaderID, req)
		if err != nil {
			return 0, err
		}
		if _, err := n.waitApplied(ctx, resp.Index); err != nil {
			return 0, err
		}
		return resp.Index, nil
	}

	respCh := make(chan result[Index], 1)
	msg := readIndexMsg{
		resolver: promiseIndexResolver{p: promise[Index]{ch: respCh}},
		useLease: false,
	}
	select {
	case n.readIndexCh <- msg:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-n.stopCh:
		return 0, ErrStopped
	}
	select {
	case r := <-respCh:
		if r.err != nil {
			return 0, r.err
		}
		if _, err := n.waitApplied(ctx, r.val); err != nil {
			return 0, err
		}
		return r.val, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-n.stopCh:
		return 0, ErrStopped
	}
}

// ReadIndexLease is like ReadIndex but skips the heartbeat round-trip when the
// leader holds a valid clock-based read lease. Like ReadIndex, it waits for
// the local state machine to apply up to the returned index before returning,
// so callers can read from their local state machine immediately.
//
// If called on a follower, it behaves exactly like ReadIndex (forwarding to the
// leader), as followers do not hold read leases.
func (n *Node) ReadIndexLease(ctx context.Context) (Index, error) {
	select {
	case <-n.stopCh:
		return 0, ErrStopped
	default:
	}

	if n.StateSnapshot() != Leader {
		return n.ReadIndex(ctx)
	}

	respCh := make(chan result[Index], 1)
	msg := readIndexMsg{
		resolver: promiseIndexResolver{p: promise[Index]{ch: respCh}},
		useLease: true,
	}
	select {
	case n.readIndexCh <- msg:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-n.stopCh:
		return 0, ErrStopped
	}
	select {
	case r := <-respCh:
		if r.err != nil {
			return 0, r.err
		}
		if _, err := n.waitApplied(ctx, r.val); err != nil {
			return 0, err
		}
		return r.val, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-n.stopCh:
		return 0, ErrStopped
	}
}

// waitApplied blocks until the state machine has applied at least the given
// index. Returns the current LastApplied() once the condition is met.
func (n *Node) waitApplied(ctx context.Context, index Index) (Index, error) {
	for {
		last := n.LastApplied()
		if last >= index {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-n.stopCh:
			return last, ErrStopped
		case <-n.applyAdvancedCh:
			// Re-check LastApplied on next iteration.
		}
	}
}

// LastApplied returns the index of the last entry applied to the state machine.
// Safe for concurrent use; reads from the atomic mirror updated by the event
// loop.
func (n *Node) LastApplied() Index {
	return Index(n.atomicLastApplied.Load())
}

// CommitIndex returns the index of the highest log entry known to be committed.
// Safe for concurrent use.
func (n *Node) CommitIndex() Index {
	return Index(n.atomicCommitIndex.Load())
}

// ReadStale returns the index of the last entry applied to this node's local
// state machine. It is an atomic read with no lock and no RPC — the cheapest
// possible read operation.
//
// The returned index is a fence: any value read from the local state machine
// immediately after this call reflects at least this index. The data may be
// behind the leader by up to one replication round-trip; callers must
// explicitly accept this trade-off.
//
// Use ReadIndex or ReadIndexLease when linearizable consistency is required.
// ReadStale is appropriate for non-critical reads, cache warming, dashboard
// metrics, or any workload where slightly stale data is acceptable and low
// latency / no leader dependency matters more than strict consistency.
//
// Safe for concurrent use.
func (n *Node) ReadStale() Index {
	return n.LastApplied()
}

// TransferLeadership asks this (leader) node to hand off leadership to target.
// It returns ErrNotLeader if called on a non-leader, ErrLeadershipTransferInProgress
// if a transfer is already underway, and ErrStopped if the node is stopped.
// On success the transfer has been initiated: the leader will stop accepting
// proposals and send a TimeoutNow RPC to target once it is sufficiently
// caught-up. The caller may poll StateSnapshot() to observe the step-down.
func (n *Node) TransferLeadership(ctx context.Context, to NodeID) error {
	select {
	case <-n.stopCh:
		return ErrStopped
	default:
	}

	respCh := make(chan error, 1)
	msg := leadershipTransferMsg{target: to, respCh: respCh}
	select {
	case n.transferCh <- msg:
	case <-ctx.Done():
		return ctx.Err()
	case <-n.stopCh:
		return ErrStopped
	}
	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-n.stopCh:
		return ErrStopped
	}
}

// AddServer proposes adding a new peer to the cluster. It blocks until the
// change is committed and applied, or until ctx is cancelled.
// Returns ErrNotLeader if called on a non-leader, or ErrConfigChangeInProgress
// if another membership change is already in progress.
//
// Safety: AddServer uses the single-server change protocol (Raft §4.2), which
// is only safe when exactly one server is added or removed at a time. The
// ErrConfigChangeInProgress gate prevents concurrent changes, but callers must
// not attempt overlapping sequences (e.g. start an AddServer, then immediately
// call AddServer again before the first commits). For arbitrary topology
// changes — adding and removing multiple servers simultaneously — use
// ReconfigureCluster, which employs joint consensus.
func (n *Node) AddServer(ctx context.Context, id NodeID) error {
	_, err := n.Propose(ctx, encodeConfigEntry(configOpAdd, id))
	return err
}

// RemoveServer proposes removing peer id from the cluster. It blocks until
// the change is committed and applied, or until ctx is cancelled.
// Returns ErrNotLeader if called on a non-leader, or ErrConfigChangeInProgress
// if another membership change is already in progress.
//
// Safety: see AddServer. For removing the current leader, prefer
// ReconfigureCluster (which uses joint consensus and handles leader self-removal
// atomically), or transfer leadership first via TransferLeadership.
func (n *Node) RemoveServer(ctx context.Context, id NodeID) error {
	_, err := n.Propose(ctx, encodeConfigEntry(configOpRemove, id))
	return err
}

// ReconfigureCluster replaces the current cluster membership with newMembers
// using joint consensus (§4.3 of the Raft dissertation). It is the preferred
// API for arbitrary topology changes such as replacing the entire cluster or
// changing multiple nodes at once.
//
// The process is transparent to the caller:
//  1. A joint-config entry (C_old ∪ C_new) is committed, requiring a majority
//     from both the old and new membership simultaneously.
//  2. Once the joint entry is applied, the leader automatically appends and
//     commits a finalise entry containing only C_new.
//  3. ReconfigureCluster returns once the joint entry from step 1 is committed.
//
// newMembers is the complete new cluster membership. Include this node's own
// ID to retain it in the cluster; omit it to remove the current leader —
// the leader will step down automatically after the finalise entry commits.
// newMembers may be empty only if this node is also omitted (leaving the
// cluster with zero members is not useful; pass []NodeID{n.ID()} for a
// single-node cluster).
//
// Returns ErrNotLeader if this node is not the leader, ErrConfigChangeInProgress
// if another membership change is already in flight, or ErrStopped if the node
// has been stopped.
func (n *Node) ReconfigureCluster(ctx context.Context, newMembers []NodeID) error {
	// Reject duplicate entries in newMembers. Duplicates would propagate into
	// the finalise entry and cfg.Peers, causing quorum calculations to overcount
	// a node, which could permanently prevent the cluster from committing
	// (liveness violation).
	seen := make(map[NodeID]struct{}, len(newMembers))
	for _, id := range newMembers {
		if _, dup := seen[id]; dup {
			return fmt.Errorf("raft: ReconfigureCluster: duplicate member %q in newMembers", id)
		}
		seen[id] = struct{}{}
	}

	// Read the current peer list from the atomic mirror rather than cfg.Peers
	// directly. cfg.Peers is mutated by applyConfigChange inside the event
	// loop; reading it here (outside the loop) without synchronisation is a
	// data race.
	var oldPeers []NodeID
	if v := n.atomicPeers.Load(); v != nil {
		oldPeers = v.([]NodeID)
	}
	// newMembers is passed as-is: it may include self (self retained) or not
	// (self removed). applyConfigChange detects self's presence by inspecting
	// the joint entry's new-members list.
	_, err := n.Propose(ctx, encodeJointConfigEntry(oldPeers, newMembers))
	return err
}

// --- Atomic-safe internal setters (event-loop goroutine only) ---------------

// setState updates n.state and its atomic mirror atomically from the caller's
// perspective. Must only be called from the event-loop goroutine.
func (n *Node) setState(s State) {
	n.state = s
	n.atomicState.Store(uint32(s))
}

// setLeaderID updates n.leaderID and its atomic mirror. Event-loop only.
func (n *Node) setLeaderID(id NodeID) {
	n.leaderID = id
	n.atomicLeader.Store(string(id))
}

// setCommitIndex updates n.commitIndex and its atomic mirror. Event-loop only.
func (n *Node) setCommitIndex(idx Index) {
	n.commitIndex = idx
	n.atomicCommitIndex.Store(uint64(idx))
}

// --- Handler implementation -------------------------------------------------
// The Transport calls these from arbitrary goroutines. Each method serialises
// the request onto the event loop and blocks until a response is available.

// dispatchRPC is a generic helper that forwards an inbound RPC to the
// event-loop goroutine and waits for the response. The type parameter R is
// the expected concrete response type.
func dispatchRPC[R any](ctx context.Context, n *Node, req any) (R, error) {
	respCh := make(chan rpcResponse, 1)
	env := rpcEnvelope{req: req, respCh: respCh}

	select {
	case n.rpcCh <- env:
	case <-ctx.Done():
		var zero R
		return zero, ctx.Err()
	case <-n.stopCh:
		var zero R
		return zero, ErrStopped
	}

	select {
	case resp := <-respCh:
		if resp.err != nil {
			var zero R
			return zero, resp.err
		}
		r, ok := resp.resp.(R)
		if !ok {
			var zero R
			return zero, fmt.Errorf("raft: unexpected RPC response type %T", resp.resp)
		}
		return r, nil
	case <-ctx.Done():
		var zero R
		return zero, ctx.Err()
	case <-n.stopCh:
		var zero R
		return zero, ErrStopped
	}
}

func (n *Node) HandleRequestVote(ctx context.Context, req *RequestVoteRequest) (*RequestVoteResponse, error) {
	return dispatchRPC[*RequestVoteResponse](ctx, n, req)
}

func (n *Node) HandleAppendEntries(ctx context.Context, req *AppendEntriesRequest) (*AppendEntriesResponse, error) {
	return dispatchRPC[*AppendEntriesResponse](ctx, n, req)
}

func (n *Node) HandleInstallSnapshot(ctx context.Context, req *InstallSnapshotRequest) (*InstallSnapshotResponse, error) {
	return dispatchRPC[*InstallSnapshotResponse](ctx, n, req)
}

func (n *Node) HandleTimeoutNow(ctx context.Context, req *TimeoutNowRequest) (*TimeoutNowResponse, error) {
	return dispatchRPC[*TimeoutNowResponse](ctx, n, req)
}

func (n *Node) HandleReadIndex(ctx context.Context, req *ReadIndexRequest) (*ReadIndexResponse, error) {
	return dispatchRPC[*ReadIndexResponse](ctx, n, req)
}

// --- Internal helpers -------------------------------------------------------

// Term returns the current term of this node. Safe for concurrent use.
func (n *Node) Term() Term {
	return Term(n.atomicTerm.Load())
}

// saveTerm persists currentTerm+votedFor atomically then updates the cache.
// Must be called from the event-loop goroutine only.
func (n *Node) saveTerm(term Term, votedFor NodeID) error {
	if err := n.cfg.Storage.SaveHardState(n.stopCtx, HardState{
		CurrentTerm: term,
		VotedFor:    votedFor,
	}); err != nil {
		return fmt.Errorf("saveTerm: %w", err)
	}
	n.currentTerm = term
	n.votedFor = votedFor
	n.atomicTerm.Store(uint64(term))
	return nil
}

// traceRPC calls cfg.Tracer.StartRPC if a tracer is configured and returns the
// finish func. When no tracer is set it returns a no-op func so callers don't
// need a nil check. Safe to call from any goroutine.
func (n *Node) traceRPC(peer NodeID, rpcType string) func(error) {
	if n.cfg.Tracer == nil {
		return func(error) {}
	}
	return n.cfg.Tracer.StartRPC(n.cfg.ID, peer, rpcType)
}

// now returns the current wall-clock time, delegating to cfg.Clock when set.
func (n *Node) now() time.Time {
	if n.cfg.Clock != nil {
		return n.cfg.Clock.Now()
	}
	return time.Now()
}

// rpcTimeout returns the per-RPC deadline duration. Falls back to
// ElectionTimeoutMin when Config.RPCTimeout is not set.
func (n *Node) rpcTimeout() time.Duration {
	if n.cfg.RPCTimeout > 0 {
		return n.cfg.RPCTimeout
	}
	return n.cfg.ElectionTimeoutMin
}

// resetElectionTimeout picks a new randomised election timeout in [min, max).
func (n *Node) resetElectionTimeout() {
	span := n.electionMaxTicks - n.electionMinTicks
	if span <= 0 {
		span = 1
	}
	n.electionTimeout = n.electionMinTicks + n.rng.IntN(span)
	n.electionElapsed = 0
}

// snapshotChunkSize returns the maximum bytes per InstallSnapshot RPC chunk.
// Zero (disabled) means send the full snapshot in one RPC.
func (n *Node) snapshotChunkSize() int {
	return n.cfg.SnapshotChunkSize
}

// startHBPumps launches one persistent heartbeat-pump goroutine per peer.
// Called by becomeLeader; each pump reads from a size-1 channel and sends a
// single AppendEntries RPC before posting the result to rpcCh.
func (n *Node) startHBPumps() {
	n.hbPumps = make(map[NodeID]chan *AppendEntriesRequest, len(n.cfg.Peers))
	n.hbStopChs = make(map[NodeID]chan struct{}, len(n.cfg.Peers))
	for _, peer := range n.cfg.Peers {
		ch := make(chan *AppendEntriesRequest, 1)
		stop := make(chan struct{})
		n.hbPumps[peer] = ch
		n.hbStopChs[peer] = stop
		go n.runHBPump(peer, ch, stop)
	}
}

// stopHBPumps signals all pump goroutines to exit. Called by becomeFollower.
func (n *Node) stopHBPumps() {
	for _, stop := range n.hbStopChs {
		close(stop)
	}
	n.hbPumps = nil
	n.hbStopChs = nil
}

// startHBPumpFor starts a single heartbeat-pump goroutine for peer id.
// Called by applyConfigChange when a new peer is added while this node is leader.
// No-op if hbPumps is nil (i.e. we are not currently leader).
func (n *Node) startHBPumpFor(id NodeID) {
	if n.hbPumps == nil {
		return
	}
	if _, exists := n.hbPumps[id]; exists {
		return // already running
	}
	ch := make(chan *AppendEntriesRequest, 1)
	stop := make(chan struct{})
	n.hbPumps[id] = ch
	n.hbStopChs[id] = stop
	go n.runHBPump(id, ch, stop)
}

// stopHBPumpFor stops and removes the heartbeat-pump goroutine for peer id.
// Called by applyConfigChange when a peer is removed while this node is leader.
func (n *Node) stopHBPumpFor(id NodeID) {
	if stop, ok := n.hbStopChs[id]; ok {
		close(stop)
		delete(n.hbStopChs, id)
		delete(n.hbPumps, id)
	}
}

// runHBPump is the body of one per-peer heartbeat pump goroutine.
// It reads from ch, sends the AppendEntries RPC, and posts the result to rpcCh.
// The goroutine exits when stop is closed or n.stopCtx is cancelled.
func (n *Node) runHBPump(peer NodeID, ch <-chan *AppendEntriesRequest, stop <-chan struct{}) {
	for {
		var req *AppendEntriesRequest
		select {
		case req = <-ch:
		case <-stop:
			return
		case <-n.stopCtx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(n.stopCtx, n.rpcTimeout())
		finish := n.traceRPC(peer, "AppendEntries")
		resp, err := n.cfg.Transport.AppendEntries(ctx, peer, req)
		finish(err)
		cancel()
		if err != nil || resp == nil {
			continue // missed heartbeat; next tick will send another
		}
		select {
		case n.rpcCh <- rpcEnvelope{req: &appendResult{
			peer:    peer,
			term:    resp.Term,
			success: resp.Success,
			req:     req,
		}}:
		case <-stop:
			return
		case <-n.stopCtx.Done():
			return
		}
	}
}

// tickerLoop fires Tick() at the configured interval.
func (n *Node) tickerLoop() {
	t := time.NewTicker(n.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			n.Tick()
		case <-n.stopCh:
			return
		}
	}
}

// applyRestore installs a snapshot into the state machine and resets the
// apply goroutine's local tracking state. Called from both the priority-select
// drain and the main select in applyLoop to avoid code duplication.
func (n *Node) applyRestore(ctx context.Context, si snapshotInstall, localLastApplied *Index) map[NodeID]clientEntry {
	defer si.r.Close()
	if err := n.cfg.StateMachine.Restore(ctx, si.meta, si.r); err != nil {
		n.logger.Error("applyLoop: Restore", "err", err)
	}
	*localLastApplied = si.meta.LastIncludedIndex
	n.atomicLastApplied.Store(uint64(si.meta.LastIncludedIndex))
	select {
	case n.applyAdvancedCh <- struct{}{}:
	default:
	}
	newTable := make(map[NodeID]clientEntry, len(si.clientTable))
	maps.Copy(newTable, si.clientTable)
	return newTable
}

// applyLoop reads committed entries and feeds them to the state machine.
// It maintains its own lastApplied counter so it never touches Node fields
// directly; results flow back via applyResultCh.
//
// Snapshot restores are prioritised over commit notifications: when the event
// loop installs a snapshot it sends on restoreSnapshotCh, which the inner
// priority-select drains before processing any pending commit notification.
// This prevents the apply goroutine from trying to read log entries that the
// snapshot installation may have already compacted away.
func (n *Node) applyLoop() {
	defer close(n.applyDoneCh)

	// applyBaseIndex is set once in New() before any goroutine starts, so
	// reading it here is data-race free even though the event loop later
	// writes n.log.snapMeta.
	localLastApplied := n.applyBaseIndex
	ctx := n.stopCtx

	// localClientTable is the apply goroutine's own copy of the dedup table.
	// It is used to enforce exactly-once semantics for ProposeOnce entries: if
	// a (clientID, seqNum) pair has already been applied, SM.Apply is skipped
	// and the cached result is returned instead.
	//
	// Keeping a separate copy here (rather than reading n.clientTable) is
	// necessary because n.clientTable is owned by the event-loop goroutine and
	// must not be read from the apply goroutine without synchronisation.
	localClientTable := make(map[NodeID]clientEntry)

	// On restart from a snapshot: restore the state machine once before
	// processing any committed entries. initialSnap is set once in New()
	// and is only read here, so there is no data race.
	if n.initialSnap != nil {
		_, r, err := n.cfg.Storage.LoadSnapshot(ctx)
		if err == nil {
			// Skip the framing header (meta and table already handled in New).
			_, smReader, rerr := readWrappedSnapshot(r)
			if rerr == nil {
				if err := n.cfg.StateMachine.Restore(ctx, n.initialSnap.meta, smReader); err != nil {
					n.logger.Error("applyLoop: initial snapshot restore", "err", err)
				}
			} else {
				n.logger.Error("applyLoop: initial snapshot framing", "err", rerr)
			}
			_ = r.Close()
		} else if err != ErrNoSnapshot {
			n.logger.Error("applyLoop: initial snapshot load", "err", err)
		}
		// Seed localClientTable from the snapshot's table so that entries
		// already covered by the snapshot are not applied again on log replay.
		maps.Copy(localClientTable, n.initialSnap.clientTable)
		n.initialSnap = nil // release memory; event loop never reads this field
	}

	for {
		// Priority: drain any pending snapshot restore before applying entries.
		// NOTE (false positive — no data race on restoreSnapshotCh): the event
		// loop and the apply goroutine are the only two goroutines that touch
		// restoreSnapshotCh. The event loop only writes to it (handleInstallSnapshot)
		// and the apply goroutine only reads from it. The channel is size-1 with a
		// non-blocking replace so the event loop never blocks, and localLastApplied
		// is owned exclusively by the apply goroutine. All node state that the apply
		// goroutine reads (log entries via Storage) is immutable after being written.
		select {
		case si := <-n.restoreSnapshotCh:
			localClientTable = n.applyRestore(ctx, si, &localLastApplied)
			continue
		default:
		}

		select {
		case si := <-n.restoreSnapshotCh:
			localClientTable = n.applyRestore(ctx, si, &localLastApplied)

		case trig := <-n.snapshotTriggerCh:
			// Take the snapshot here in applyLoop so Snapshot() and Apply()
			// are never concurrent on the same state machine.
			pr, pw := io.Pipe()
			errCh := make(chan error, 1)
			go func() {
				errCh <- n.cfg.Storage.SaveSnapshot(n.stopCtx, trig.meta, pr)
			}()

			serr := writeWrappedSnapshot(pw, trig.clientTable, func(w io.Writer) error {
				return n.cfg.StateMachine.Snapshot(n.stopCtx, w)
			})
			_ = pw.Close() // signals EOF to SaveSnapshot

			saveErr := <-errCh
			if serr == nil {
				serr = saveErr
			}

			select {
			case n.snapshotResultCh <- snapshotResult{
				meta: trig.meta,
				err:  serr,
			}:
			case <-n.stopCh:
				return
			}

		case commitIdx := <-n.commitNotifyCh:
			lo := localLastApplied + 1
			if lo > commitIdx {
				continue
			}
			// Fetch the entire range in one storage call to avoid N sequential
			// disk seeks when a node is catching up with many committed entries.
			entries, err := n.cfg.Storage.GetLogEntries(ctx, lo, commitIdx+1)
			if err != nil {
				// If the range is partly compacted (snapshot installed concurrently),
				// fall back to individual reads so we can advance past missing entries.
				n.logger.Warn("apply: GetLogEntries failed, retrying one-by-one",
					"lo", lo, "hi", commitIdx, "err", err)
				entries = entries[:0]
				for i := lo; i <= commitIdx; i++ {
					e, rerr := n.cfg.Storage.GetLogEntry(ctx, i)
					if rerr != nil {
						n.logger.Warn("apply: entry not found", "index", i, "err", rerr)
						ar := applyResult{index: i, err: rerr}
						select {
						case n.applyResultCh <- ar:
							localLastApplied = i
						case <-n.stopCh:
							return
						}
						continue
					}
					entries = append(entries, e)
				}
			}
			for _, entry := range entries {
				i := entry.Index
				// Config entries are handled by the Raft layer; do not forward
				// to the user state machine.
				var ar applyResult
				switch {
				case isConfigEntry(entry.Command):
					ar = applyResult{index: i, configCmd: entry.Command, cmd: entry.Command}
				case isDedupCmd(entry.Command):
					// ProposeOnce command: enforce exactly-once by checking the
					// local dedup table BEFORE calling SM.Apply. This closes the
					// window where a retry is sent to a new leader while the
					// original committed entry has not yet been applied, resulting
					// in two log entries with the same (clientID, seqNum).
					clientID, seqNum, payload, decErr := decodeDedupCmd(entry.Command)
					if decErr != nil {
						// Malformed dedup header; apply as-is.
						val, applyErr := n.cfg.StateMachine.Apply(ctx, entry)
						ar = applyResult{index: i, val: val, err: applyErr, cmd: entry.Command}
					} else if cached, ok := localClientTable[clientID]; ok && seqNum == cached.seqNum {
						// Exact duplicate: return the cached result without re-applying.
						ar = applyResult{index: i, val: cached.result, cmd: entry.Command}
					} else {
						// New (seqNum > cached) or first-seen: strip header, apply.
						smEntry := entry
						smEntry.Command = payload
						val, applyErr := n.cfg.StateMachine.Apply(ctx, smEntry)
						ar = applyResult{index: i, val: val, err: applyErr, cmd: entry.Command}
						// Update the local table immediately so subsequent entries
						// in this batch see the up-to-date dedup state.
						if applyErr == nil {
							localClientTable[clientID] = clientEntry{seqNum: seqNum, result: val}
						}
					}
				default:
					val, applyErr := n.cfg.StateMachine.Apply(ctx, entry)
					ar = applyResult{index: i, val: val, err: applyErr, cmd: entry.Command}
				}
				select {
				case n.applyResultCh <- ar:
					localLastApplied = i
				case <-n.stopCh:
					return
				}
			}

		case <-n.stopCh:
			return
		}
	}
}
