package raft

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	// submitted is when the caller handed the command over, so that reported
	// latency includes the time spent queued for the event loop -- which is
	// time the caller waited, and is exactly what gets long under load.
	submitted time.Time
}

// pendingProposal is a proposal waiting for its entry to be applied.
type pendingProposal struct {
	promise   promise[[]byte]
	submitted time.Time
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

// snapshotAck is an InstallSnapshot response held back until the snapshot it
// completes has been written. answer is safe to call more than once; only the
// first call is delivered.
type snapshotAck struct {
	index  Index
	answer func(error)
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
	// atomicDurableTerm mirrors the highest term whose hard state is known to
	// be on disk. It is what goes out on a message built outside the event
	// loop, where there is no write to defer against; see ReadIndex.
	atomicDurableTerm   atomic.Uint64
	atomicState         atomic.Uint32 // mirrors state
	atomicLeader        atomic.Value  // mirrors leaderID; stores string
	atomicLastApplied   atomic.Uint64 // mirrors lastApplied
	atomicCommitIndex   atomic.Uint64 // mirrors commitIndex
	atomicTerm          atomic.Uint64 // mirrors currentTerm
	atomicSnapshotIndex atomic.Uint64 // mirrors log.snapMeta.LastIncludedIndex; updated by handleSnapshotResult
	// atomicPeers mirrors cfg.Peers as a []PeerConfig snapshot and atomicVoter
	// mirrors cfg.Voter, this node's own role. Both are written by
	// storeMembership from the event loop, and read by ReconfigureCluster,
	// Members and Status from whatever goroutine calls them.
	//
	// cfg.Peers and cfg.Voter change together on every configuration change,
	// so they are mirrored together: a reader that took the peer list from
	// here and this node's role from cfg would be racing the event loop for
	// one of the two values.
	atomicPeers atomic.Value // stores []PeerConfig
	atomicVoter atomic.Bool  // mirrors cfg.Voter

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

	// departing holds the peers this leader has removed from the membership
	// but is still replicating to, so that each of them learns it was
	// removed. Leader only; nil otherwise. See beginDeparture.
	departing map[NodeID]departure

	// --- Log ----------------------------------------------------------------
	log *raftLog

	// writer owns every mutating call into Storage. The event loop queues work
	// on it and never waits for it; see storage_writer.go.
	writer *storageWriter
	// deferredWrites holds work that must not happen until a queued storage
	// write has completed, in the order the writes were queued. Anything that
	// tells another node, or this node's own commit accounting, that something
	// is on disk belongs here.
	deferredWrites []deferredWrite
	// unsafeTermSeq is the sequence number of the most recent hard-state write
	// that has not completed, or 0 when the term and vote on disk are the ones
	// this node is using. See sendGate.
	unsafeTermSeq uint64
	// completedWriteSeq is the highest sequence number whose write has
	// completed and been processed. Work handed to afterWrite for a write at
	// or below it has nothing left to wait for.
	completedWriteSeq uint64
	// recordedCommit is the commit index most recently handed to a
	// CommitRecorder. It exists to throttle those writes; nothing in normal
	// operation reads what they record. See maybeRecordCommit.
	recordedCommit Index

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
	jointOld         []PeerConfig
	jointNew         []PeerConfig
	jointIncludeSelf bool
	// jointSelfVoter is true if this node is a voter in the new configuration
	// during joint consensus.
	jointSelfVoter bool
	// jointSelfVoterOld is this node's voting role in C_old, captured when the
	// joint configuration was adopted. Quorum checks against C_old need it
	// because cfg.Voter may already describe the new role.
	jointSelfVoterOld bool

	// --- Membership provenance ----------------------------------------------
	// membershipWitness is what the membership in effect says about this
	// node: whether it is a witness. It is compared with Config.Witness,
	// which is what the node actually is, at startup and whenever a change
	// is applied. Event-loop only.
	membershipWitness bool

	// baseMembership is the membership recorded in the snapshot this node
	// started from, or the bootstrap membership from Config when there is no
	// snapshot. It is the base that config entries in the log are replayed on
	// top of by rebuildMembership.
	baseMembership membershipState
	// configIndex is the log index of the config entry that established the
	// membership currently in effect, or the snapshot index when it came from
	// the snapshot base. A truncation at or below it invalidates the
	// membership and forces a rebuild.
	configIndex Index

	// --- Check-quorum state (leader only) -----------------------------------
	// leaderQuorumElapsed counts ticks since quorumAcks was last reset.
	// When CheckQuorum is enabled and this reaches electionTimeout without a
	// quorum of peers having acked, the leader steps down.
	leaderQuorumElapsed int
	// quorumAcks collects the set of peers that sent a successful AppendEntries
	// response during the current check-quorum window. Self is not included; it
	// is counted implicitly when evaluating quorum.
	quorumAcks map[NodeID]bool

	// termStartIndex is the index of the no-op this node appended when it
	// became leader, and so the first index in its own term. Zero when not
	// leading.
	//
	// Everything at or above it was appended by this node in the current term:
	// a leader only ever appends its own entries, and one that accepts an
	// AppendEntries has already stepped down. Everything below it is from an
	// earlier term and can never be committed by replica count (Raft 5.4.2).
	// That makes it the exact lower bound for the commit scan, and removes the
	// need to read each entry's term back from storage.
	termStartIndex Index

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
	pending map[Index]pendingProposal

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

	// snapInstallAck answers the InstallSnapshot RPC that delivered the final
	// chunk of a snapshot, and is held until that snapshot is on disk.
	//
	// The leader records the acknowledgement as this node's match index, which
	// is its statement of what this node durably holds. Answering when the
	// last chunk merely arrived would make that statement false for as long as
	// the write took, and permanently false if this node died during it: the
	// match index is never lowered. Nothing unsafe follows, because a snapshot
	// never covers anything past the leader's commit index and so cannot
	// advance one -- but the same number decides whether a learner has caught
	// up enough to become a voter, and promoting one that has not costs the
	// cluster a voter that cannot vote.
	snapInstallAck *snapshotAck
	// installingSnap is the last-included index of the snapshot currently
	// being written, or 0 when none is. A leader whose per-chunk deadline
	// expires while that write is in progress restarts the transfer from the
	// beginning; starting a second write of the same snapshot would queue
	// behind the first inside the storage backend and make the next deadline
	// harder to meet than the last. The retry waits on the write already
	// running instead.
	installingSnap Index

	// pendingSnap holds chunks of an in-progress multi-chunk snapshot install
	// on this follower. It is populated by handleInstallSnapshot as chunks
	// arrive and consumed (then nil'd) when the final chunk (Done=true) lands.
	// Cleared on becomeFollower so a stale partial install cannot be completed
	// by a message from a previous leader after an election.
	pendingSnap *partialSnapshot

	// snapshotWriteWg tracks background snapshot writers started for a
	// captured state machine. Stop waits on it so that a writer holding a
	// capture and the storage backend has finished before the node is
	// considered stopped.
	snapshotWriteWg sync.WaitGroup

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
	pendingReads       []readIndexResolver // clients waiting for the round in flight
	// waitingReads holds requests that arrived while a confirmation round was
	// already in flight. They cannot be answered by that round -- it proves
	// leadership as of a moment before they arrived -- so they wait for the
	// next one, started as soon as the current round completes.
	waitingReads []readIndexResolver
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
	// leaseBase is the instant the current lease was granted from, kept with
	// its monotonic reading so that a suspend -- which stops the monotonic
	// clock while the wall clock keeps going -- can be detected as the two
	// diverging. See leaseClockJumped.
	leaseBase time.Time

	// readIndexCh carries ReadIndex requests from callers into the event loop.
	readIndexCh chan readIndexMsg

	// --- Client dedup table (event-loop goroutine only) ---------------------
	// clientTable maps each client's NodeID to the latest (seqNum, result)
	// pair seen. It is persisted inside every snapshot so that leadership
	// hand-offs and restarts do not lose the dedup state. An LRU evicts the
	// least-recently-used client when the table exceeds MaxClientTableSize.
	clientTable *clientLRU

	// clientTableSize mirrors clientTable.len() for ClientTableSize, which is
	// called from outside the event loop.
	clientTableSize atomic.Int64
	// clientTableCap is the bound the table is kept under. Like membership,
	// the bound is state every replica must share: a replica keeping a
	// different one evicts different clients and diverges. It is established
	// by a config entry the leader appends before the first ProposeOnce
	// entry, carried in every snapshot, and changed with
	// SetMaxClientTableSize.
	//
	// It follows the apply order, not the log: a cap entry changes the table
	// when it is applied, in sequence with the entries around it, because a
	// bound that shrank and then grew again leaves a different table from
	// one that was only ever the final value. clientTableCapAgreed says
	// whether the bound in effect is one the group agreed (applied from the
	// log or loaded from a snapshot) rather than this node's own
	// Config.MaxClientTableSize. Event-loop only; atomicClientTableCap
	// mirrors clientTableCap for readers outside.
	clientTableCap       int
	clientTableCapAgreed bool
	atomicClientTableCap atomic.Int64
	// clientTableCapReplicated says whether the group has agreed a bound as
	// far as this node's log knows: a cap entry in the log, applied or not,
	// or one in the snapshot base. A leader whose log knows of none appends
	// one before the first ProposeOnce entry. Rebuilt with membership.
	clientTableCapReplicated bool
	// baseClientTableCap is the bound recorded in the snapshot this node
	// started from, when hasBaseClientTableCap.
	baseClientTableCap    int
	hasBaseClientTableCap bool
	// applyStartCap is the bound the apply goroutine's own table starts
	// under: the snapshot's when there was one, else Config. Set in New.
	applyStartCap int
	// commitQuorumApplied is the commit quorum policy in effect, from the
	// latest applied quorum entry or the snapshot; commitQuorumLatest the
	// policy from the latest quorum entry in the log, applied or not. Every
	// decision uses the stricter of the two; see quorum.go. 0 is a majority.
	// commitQuorumAgreed says whether the group has agreed one at all, as
	// distinct from 0 meaning majority, so that a snapshot can carry it.
	// Event-loop only; atomicCommitQuorum mirrors commitQuorumApplied.
	commitQuorumApplied int
	commitQuorumLatest  int
	commitQuorumAgreed  bool
	atomicCommitQuorum  atomic.Int64
	// quorumEntryPending is the index of a commit quorum entry this leader
	// has appended from its Config and not yet seen commit, or 0.
	quorumEntryPending Index
	// capEntryPending is the index of a client table cap entry this leader
	// has appended and not yet seen commit, or 0. It stops a burst of
	// ProposeOnce batches from each appending one.
	capEntryPending Index

	// clientsForgotten counts evictions since the node started, and
	// lastForgetLog is when one was last written to the log. Both are
	// event-loop-owned. The eviction is worth a warning because it is the
	// point at which exactly-once stops holding, and worth rate-limiting
	// because a table that is one entry too small evicts on every proposal.
	clientsForgotten uint64
	lastForgetLog    time.Time

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

	// handler is the Handler wrapper registered with the Transport. Created
	// once in New() so Handler() always returns the same value.
	handler *nodeHandler

	// --- Leadership observers ------------------------------------------------
	// watchers holds the subscriptions created by LeadershipChanges. The event
	// loop announces through them; the mutex is held only for the length of a
	// non-blocking send, so it never delays consensus work.
	watchersMu  sync.Mutex
	watchers    map[uint64]chan LeadershipChange
	nextWatcher uint64
	// leadership is the last announced status, published as one value so that
	// a reader can never see a half-applied transition -- a node claiming
	// leadership with no leader recorded yet, say. Written by the event loop
	// only, read by subscribers.
	leadership atomic.Value // stores LeadershipChange
	// leadershipDirty is set when a transition changes state or leader, and
	// cleared when the event loop announces the result at the end of its turn.
	leadershipDirty bool
	// observers holds the subscriptions created by Events. They share
	// watchersMu with the watchers above because they are published the same
	// way, by a non-blocking send from the event loop, so one lock covers both
	// and is never held for longer than that send takes. See observer.go.
	observers    map[uint64]*observer
	nextObserver uint64
	// peerHealth remembers, per peer, whether this leader's AppendEntries RPCs
	// are reaching it, so that an observer is told once when a follower stops
	// answering and once when it starts again. Event-loop only.
	peerHealth map[NodeID]peerHealth
	// removedNotified is set once Config.OnRemoved has been invoked, so that
	// a removal reported by more than one entry -- a finalise entry after a
	// direct remove, say -- reports once. Event-loop only.
	removedNotified bool
	// fatalErr holds the first durable-write failure this node hit, if any.
	// Setting it stops the node; it is read by FatalError and reported in
	// place of ErrStopped by every operation afterwards.
	fatalErr atomic.Value // stores error
}

// fail stops the node because a write Raft's safety argument depends on did not
// reach stable storage. Only the first failure is recorded. Event-loop only.
//
// Continuing is not an option: a node that acts on a term, vote or log entry
// that may not survive a restart can vote twice in one term, or acknowledge
// entries a leader then counts towards a commit quorum although they are not
// durable. Stopping keeps the failure local to this node, where the rest of the
// cluster treats it as an ordinary unreachable peer.
func (n *Node) fail(err error, op string) {
	wrapped := fmt.Errorf("%w: %s: %w", ErrNodeFailed, op, err)
	if !n.fatalErr.CompareAndSwap(nil, wrapped) {
		return // already failing; keep the first error
	}
	n.logger.Error("fatal: durable write failed; stopping this node",
		"op", op, "err", err)
	// Told before Stop begins, so that the event reaches the subscriptions
	// while they are still open. It is the last thing this node reports.
	n.emit(&Event{Type: EventNodeFailed, Err: wrapped})
	if n.cfg.OnFatal != nil {
		go n.cfg.OnFatal(wrapped)
	}
	// Stop from a separate goroutine: Stop waits for the event loop to exit,
	// and fail is called from inside it.
	go n.Stop()
}

// FatalError returns the durable-write failure that stopped this node, or nil
// if it is running or was stopped normally. Safe for concurrent use.
func (n *Node) FatalError() error {
	if v := n.fatalErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// checkRunning reports why an operation cannot proceed, or nil if the node is
// running. A node that stopped because of a durable-write failure reports that
// failure rather than a plain shutdown, so a caller can tell "this node is
// broken" from "this node was asked to stop".
func (n *Node) checkRunning() error {
	if err := n.FatalError(); err != nil {
		return err
	}
	select {
	case <-n.stopCh:
		return ErrStopped
	default:
	}
	return nil
}

// stoppedErr is what operations report once the node is no longer running:
// the failure that stopped it if there was one, ErrStopped otherwise.
func (n *Node) stoppedErr() error {
	if err := n.FatalError(); err != nil {
		return err
	}
	return ErrStopped
}

// LeadershipChange reports this node's leadership status at a moment in time.
type LeadershipChange struct {
	// IsLeader is true when this node is the leader.
	IsLeader bool
	// Leader is the node this node believes is the leader, empty when it does
	// not know. When IsLeader is true it is this node's own ID.
	Leader NodeID
	// Term is the term the node was in when the change happened.
	Term Term
}

// LeadershipChanges returns a channel reporting every change in this node's
// leadership, and a function that ends the subscription.
//
// Applications need this to start and stop work that only the leader should do:
// driving a scheduler, running a compaction, accepting writes. Polling State in
// a loop answers the question late and cannot tell a brief leadership change
// from no change at all.
//
// The channel is coalescing rather than lossless: a consumer that falls behind
// sees the most recent status, never a stale one. That is the right trade for
// this signal, since acting on an out-of-date leadership status is worse than
// missing an intermediate step, but it does mean a consumer cannot count
// transitions. The channel is closed when the node stops.
//
// The returned stop function may be called more than once and must be called to
// release the subscription. It never blocks.
//
//	changes, stop := node.LeadershipChanges()
//	defer stop()
//	for change := range changes {
//		if change.IsLeader {
//			go startLeaderWork()
//		} else {
//			stopLeaderWork()
//		}
//	}
func (n *Node) LeadershipChanges() (changes <-chan LeadershipChange, stop func()) {
	ch := make(chan LeadershipChange, 1)

	n.watchersMu.Lock()
	select {
	case <-n.stopCh:
		// Already stopped: hand back a closed channel so a range over it ends
		// immediately rather than blocking for ever.
		n.watchersMu.Unlock()
		close(ch)
		return ch, func() {}
	default:
	}
	if n.watchers == nil {
		n.watchers = make(map[uint64]chan LeadershipChange)
	}
	n.nextWatcher++
	id := n.nextWatcher
	n.watchers[id] = ch
	// Deliver the current status immediately, so a subscriber does not have to
	// wait for the next change to learn where it stands. It comes from the
	// single published value rather than from the individual mirrors, which
	// could be read either side of a transition.
	ch <- n.currentLeadership()
	n.watchersMu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			n.watchersMu.Lock()
			defer n.watchersMu.Unlock()
			if existing, ok := n.watchers[id]; ok {
				delete(n.watchers, id)
				close(existing)
			}
		})
	}
}

// currentLeadership returns the last published status. Safe for concurrent use.
func (n *Node) currentLeadership() LeadershipChange {
	if v := n.leadership.Load(); v != nil {
		return v.(LeadershipChange)
	}
	return LeadershipChange{}
}

// announceLeadership publishes the node's leadership and tells subscribers,
// unless nothing they care about has changed. Event-loop only.
//
// It runs at the end of an event-loop turn rather than from each field write,
// because a single transition writes several fields: becoming leader sets the
// role first and the leader ID second, and announcing in between would hand
// subscribers a node that claims leadership with no leader recorded. Waiting
// until the turn ends means every announcement describes a state the node was
// actually in.
func (n *Node) announceLeadership() {
	if !n.leadershipDirty {
		return
	}
	n.leadershipDirty = false

	change := LeadershipChange{
		IsLeader: n.state == Leader,
		Leader:   n.leaderID,
		Term:     n.currentTerm,
	}

	n.watchersMu.Lock()
	defer n.watchersMu.Unlock()
	if n.currentLeadership() == change {
		return
	}
	n.leadership.Store(change)
	n.emitLocked(&Event{
		Type:     EventLeadershipChanged,
		IsLeader: change.IsLeader,
		Leader:   change.Leader,
	})

	for _, ch := range n.watchers {
		// Coalescing send: replace an undelivered status rather than block the
		// event loop or leave the subscriber holding a stale one.
		select {
		case ch <- change:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- change:
			default:
			}
		}
	}
}

// closeWatchers ends every subscription. Called once during shutdown.
func (n *Node) closeWatchers() {
	n.watchersMu.Lock()
	defer n.watchersMu.Unlock()
	for id, ch := range n.watchers {
		delete(n.watchers, id)
		close(ch)
	}
}

// New creates a Node from cfg, loads persisted state, and caches the log
// boundaries. It does not start any goroutines; call Start for that.
func New(cfg *Config) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Witness && cfg.StateMachine == nil {
		// A witness applies nothing; give it something that agrees.
		c := *cfg
		c.StateMachine = witnessStateMachine{}
		cfg = &c
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

	if cfg.SnapshotThreshold > 0 && cfg.TrailingLogs >= cfg.SnapshotThreshold {
		logger.Warn("TrailingLogs is at or above SnapshotThreshold; capping it so that "+
			"compaction still reclaims log space",
			"trailingLogs", cfg.TrailingLogs, "snapshotThreshold", cfg.SnapshotThreshold)
	}

	stopCtx, stopCancel := context.WithCancel(context.Background())
	writer := newStorageWriter(cfg.Storage)

	rl, err := newRaftLog(cfg.Storage, writer)
	if err != nil {
		stopCancel()
		return nil, fmt.Errorf("raft.New: %w", err)
	}

	hs, err := cfg.Storage.LoadHardState(context.Background())
	if err != nil {
		stopCancel()
		return nil, fmt.Errorf("raft.New: load hard state: %w", err)
	}

	// Copy the peer list. Node holds Config by value, so without this the node
	// and the caller would share one backing array, and the node rewrites its
	// own list in place from the event-loop goroutine as configuration changes
	// commit.
	//
	// Nothing observable depends on this today: rebuildMembership below
	// replaces the slice with a freshly built one before the node ever runs,
	// so the caller's array is already let go of. That is an accident of how
	// membership recovery happens to work rather than a property anyone
	// stated, and it is one refactor away from not being true. Copying says it
	// outright and costs one allocation per node.
	peers := append([]PeerConfig(nil), cfg.Peers...)

	n := &Node{
		cfg:         *cfg,
		logger:      logger,
		currentTerm: hs.CurrentTerm,
		votedFor:    hs.VotedFor,
		state:       Follower,
		log:         rl,
		writer:      writer,
		// lastApplied is volatile; seed it from the snapshot boundary so the
		// apply loop does not re-apply already-snapshotted entries.
		lastApplied:       rl.snapMeta.LastIncludedIndex,
		applyBaseIndex:    rl.snapMeta.LastIncludedIndex,
		heartbeatTimeout:  heartbeatTicks,
		electionMinTicks:  electionMinTicks,
		electionMaxTicks:  electionMaxTicks,
		tickCh:            make(chan struct{}, 1),
		rpcCh:             make(chan rpcEnvelope, 1024),
		proposeCh:         make(chan proposeMsg, proposalQueueSize(cfg)),
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
		pending:           make(map[Index]pendingProposal),
		clientTable:       newClientLRU(cfg.MaxClientTableSize),
		clientTableCap:    cfg.MaxClientTableSize,
		applyStartCap:     cfg.MaxClientTableSize,
		rng:               rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
	n.cfg.Peers = peers
	n.stopCtx, n.stopCancel = stopCtx, stopCancel
	n.handler = &nodeHandler{n: n}

	// Pick up where the last run left off, so that a restart does not record a
	// lower index than the one already on disk and throw away proof it still
	// has. Nothing else reads this value, so a store that cannot supply it, or
	// fails to, costs only a wider band if this node is ever recovered.
	if cr, ok := cfg.Storage.(CommitRecorder); ok {
		if recorded, cerr := cr.LoadCommitIndex(stopCtx); cerr == nil {
			n.recordedCommit = recorded
		} else {
			logger.Warn("could not read the recorded commit index; "+
				"disaster recovery will have less to go on", "err", cerr)
		}
	}

	// A state machine that keeps its own durable state is asked what it
	// already has. Anything at or below that index has been applied and made
	// durable, so replaying it would be work at best and, for a state machine
	// whose operations are not idempotent, wrong.
	durableApplied, err := n.durableAppliedIndex()
	if err != nil {
		stopCancel()
		return nil, err
	}
	if durableApplied > n.lastApplied {
		n.lastApplied = durableApplied
		n.applyBaseIndex = durableApplied
	}

	// The client table bound the snapshot was taken under comes first, so
	// that the table is loaded under it. A snapshot that recorded none leaves
	// this node on its Config value until the log or a leader says otherwise.
	if rl.hasSnapClientTableCap {
		n.adoptClientTableCap(rl.snapClientTableCap, "snapshot")
		n.baseClientTableCap, n.hasBaseClientTableCap = rl.snapClientTableCap, true
		n.applyStartCap = rl.snapClientTableCap
	}

	// If a snapshot exists, seed initialSnap so applyLoop can restore the
	// state machine on its first iteration. A state machine already past the
	// snapshot point does not want it: restoring would put it back.
	if rl.snapMeta.LastIncludedIndex > 0 && durableApplied < rl.snapMeta.LastIncludedIndex {
		n.clientTable.loadFrom(rl.snapClientTable)
		n.syncClientTableSize()
		// We don't load the SM data here; applyLoop will call LoadSnapshot.
		n.initialSnap = &snapshotInstall{
			meta:              rl.snapMeta,
			clientTable:       rl.snapClientTable,
			clientTableCap:    rl.snapClientTableCap,
			hasClientTableCap: rl.hasSnapClientTableCap,
		}
		rl.snapClientTable = nil // release reference
	}

	// Recover the membership. Config.Peers is only a bootstrap value: it
	// applies when this node has no snapshot membership and no config entry in
	// its log. Anything the cluster has since agreed takes precedence, because
	// membership is replicated state and a node that forgets it can vote under
	// the wrong quorum rules.
	if rl.hasSnapMembership {
		n.baseMembership = rl.snapMembership
		n.adoptCommitQuorum(rl.snapMembership.commitQuorum, "snapshot")
	} else {
		n.baseMembership = membershipState{
			members: withSelf(cfg.Peers, cfg.ID, true, cfg.Voter, cfg.Witness),
		}
	}
	rl.snapMembership = membershipState{}
	if err := n.rebuildMembership(context.Background()); err != nil {
		stopCancel()
		return nil, fmt.Errorf("raft.New: recover membership: %w", err)
	}
	// What the cluster believes this node is has to match what it is; see
	// Config.Witness.
	if n.membershipWitness != n.cfg.Witness {
		stopCancel()
		return nil, fmt.Errorf("raft.New: %w: the recovered membership says %s is witness=%v "+
			"but Config.Witness is %v", ErrWitnessMismatch, cfg.ID, n.membershipWitness, cfg.Witness)
	}

	// Initialise atomic mirrors so external readers never see a nil value.
	if durableApplied > 0 && rl.snapMeta.LastIncludedIndex > 0 &&
		durableApplied >= rl.snapMeta.LastIncludedIndex {
		// The client dedup table lives in the snapshot rather than in the
		// state machine, so it is still needed even when the state itself is
		// not: without it a client whose command was applied before the
		// restart gets it applied a second time on retry.
		n.clientTable.loadFrom(rl.snapClientTable)
		n.syncClientTableSize()
		rl.snapClientTable = nil
	}

	n.atomicClientTableCap.Store(int64(n.clientTableCap))
	n.atomicCommitQuorum.Store(int64(n.commitQuorumApplied))
	n.atomicState.Store(uint32(Follower))
	n.atomicLeader.Store(string(NodeID("")))
	n.leadership.Store(LeadershipChange{Term: n.currentTerm})
	n.atomicTerm.Store(uint64(n.currentTerm))
	// Whatever survived a restart is on disk by definition.
	n.atomicDurableTerm.Store(uint64(n.currentTerm))
	n.atomicLastApplied.Store(uint64(n.lastApplied))
	n.atomicCommitIndex.Store(uint64(n.commitIndex))
	n.storeMembership()
	n.resetElectionTimeout()

	// Register with the transport so we can receive inbound RPCs.
	n.cfg.Transport.Register(n.cfg.ID, n.handler)

	return n, nil
}

// Start launches the event-loop and apply goroutines.
// If cfg.TickInterval > 0 an internal ticker goroutine is also started.
// For deterministic tests set TickInterval to 0 and call Tick() manually.
func (n *Node) Start() {
	n.startOnce.Do(func() {
		// Read the term before the goroutines start. Once the event loop is
		// running it owns currentTerm, and it can change it within microseconds
		// of starting -- an election timeout is all it takes.
		term := n.currentTerm

		go n.run()
		go n.applyLoop()
		if n.cfg.TickInterval > 0 {
			go n.tickerLoop()
		}
		n.logger.Info("started", "term", term)
	})
}

// Stop signals the node to shut down and waits for all goroutines to exit,
// including the apply goroutine and any in-flight runSnapshotInstall
// goroutines. It is safe to call more than once and from any goroutine.
//
// Use Shutdown instead where the wait has to be bounded.
//
// Storage writes that have been accepted but not yet carried out are finished
// first, rather than abandoned. Dropping them would be safe -- nothing was
// acknowledged on their behalf -- but it would mean an orderly restart
// routinely threw away the tail of the log and fetched it back from the
// leader. The consequence is that Stop waits for the storage backend: one that
// has hung rather than failed will hold it there, as it would have held the
// event loop before shutdown was asked for.
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
		n.snapshotWriteWg.Wait()
		n.closeWatchers()
		n.closeObservers()
		n.cfg.Transport.Unregister(n.cfg.ID)
		n.logger.Info("stopped")
	})
}

// Shutdown stops the node like Stop, but gives up waiting when ctx is done and
// returns ctx.Err().
//
// It exists because Stop finishes the storage writes the node accepted before
// returning, which is what makes an orderly restart keep the tail of its log
// rather than fetching it back from the leader -- and which means a storage
// backend that has hung rather than failed holds Stop there indefinitely. A
// process that has to come down on a deadline needs a way to say so.
//
// Giving up does not cancel the shutdown. It carries on in the background, so
// a node Shutdown returned an error for is neither running nor finished, and
// its storage should not be reopened by another process. Prefer Stop where
// there is no deadline to meet.
func (n *Node) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.Stop()
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		n.logger.Warn("shutdown deadline passed; the node is still stopping")
		return ctx.Err()
	}
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
//
// cmd is retained, not copied: it is written to the log, sent to followers, and
// handed to StateMachine.Apply. The caller must not modify it after this call,
// including after it returns, since a snapshot may still read it.
//
// ctx controls the caller-side wait: cancelling it unblocks Propose and
// returns ctx.Err(). It does not set the deadline on outbound Raft RPCs —
// use [Config.RPCTimeout] for that.
//
// The command first waits for room in the proposal queue, whose size is
// [Config.ProposalQueueSize]. When the queue is full, [Config.ProposalOverflow]
// decides between waiting for space and returning ErrProposalQueueFull.
func (n *Node) Propose(ctx context.Context, cmd []byte) ([]byte, error) {
	// Non-blocking pre-check: if the node is stopped or broken, return
	// immediately rather than racing with a buffered proposeCh.
	if err := n.checkRunning(); err != nil {
		return nil, err
	}
	if err := n.checkProposalSize(len(cmd)); err != nil {
		return nil, err
	}

	respCh := make(chan result[[]byte], 1)
	msg := proposeMsg{cmd: cmd, respCh: respCh, submitted: n.now()}
	if n.cfg.ProposalOverflow == ProposalOverflowReject {
		select {
		case n.proposeCh <- msg:
		default:
			return nil, ErrProposalQueueFull
		}
	} else {
		select {
		case n.proposeCh <- msg:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-n.stopCh:
			return nil, n.stoppedErr()
		}
	}
	select {
	case r := <-respCh:
		return r.val, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.stopCh:
		return nil, n.stoppedErr()
	}
}

// ID returns this node's own NodeID as specified in its Config.
// Safe for concurrent use.
func (n *Node) ID() NodeID {
	return n.cfg.ID
}

// proposalQueueSize resolves Config.ProposalQueueSize, with zero meaning the
// default.
func proposalQueueSize(cfg *Config) int {
	if cfg.ProposalQueueSize > 0 {
		return cfg.ProposalQueueSize
	}
	return DefaultProposalQueueSize
}

// ProposalQueueDepth returns how many proposals are waiting for the event loop,
// out of the [Config.ProposalQueueSize] the queue holds. Safe for concurrent
// use.
//
// It is the measurement to watch for a node whose event loop is falling
// behind. A depth that sits near the capacity means proposals are waiting on
// something other than the disk -- a slow state machine, a long snapshot,
// heavy replication -- and with ProposalOverflowReject it is the point at which
// callers start seeing ErrProposalQueueFull.
func (n *Node) ProposalQueueDepth() int {
	return len(n.proposeCh)
}

// isSingleVoter returns true if this node is the only voting member of the
// cluster.
func (n *Node) isSingleVoter() bool {
	if !n.cfg.Voter {
		return false
	}
	for _, p := range n.cfg.Peers {
		if p.Voter {
			return false
		}
	}
	return true
}

// Status returns a point-in-time snapshot of this node's state as a
// GroupStatus. It is a convenience wrapper that performs the same atomic reads
// as Manager.StatusAll and is useful when you hold a *Node directly.
func (n *Node) Status() GroupStatus {
	voter := false
	for _, m := range n.Members() {
		if m.ID == n.cfg.ID {
			voter = m.Voter
			break
		}
	}
	return GroupStatus{
		GroupID:     n.cfg.GroupID,
		NodeID:      n.cfg.ID,
		State:       n.State(),
		Term:        n.Term(),
		LastApplied: n.LastApplied(),
		Voter:       voter,
		Witness:     n.cfg.Witness,
	}
}

// Members returns the current cluster membership as seen by this node: the
// full peer list from the last applied configuration entry, plus this node
// itself. The returned slice is a snapshot; it will not reflect future
// membership changes.
//
// Safe for concurrent use: both the peer list and this node's own role are
// read from the atomic mirrors the event loop keeps in sync, not from the
// Config it rewrites in place.
func (n *Node) Members() []PeerConfig {
	var peers []PeerConfig
	if v := n.atomicPeers.Load(); v != nil {
		peers = v.([]PeerConfig)
	}
	out := make([]PeerConfig, 0, len(peers)+1)
	out = append(out, PeerConfig{ID: n.cfg.ID, Voter: n.atomicVoter.Load(), Witness: n.cfg.Witness})
	out = append(out, peers...)
	return out
}

// State returns the current role of this node (Follower, Candidate, Leader, or
// PreCandidate). Safe for concurrent use; reads from the atomic mirror that
// the event loop keeps in sync.
func (n *Node) State() State {
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
//
// As with Propose, cmd is retained rather than copied and must not be modified
// after this call.
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
// ctx controls the caller-side wait: cancelling it unblocks ReadIndex and
// returns ctx.Err(). It does not set the deadline on the outbound ReadIndex
// RPC to the leader — use [Config.RPCTimeout] for that.
//
// Returns ErrStopped if the node has been stopped.
func (n *Node) ReadIndex(ctx context.Context) (Index, error) {
	if err := n.checkRunning(); err != nil {
		return 0, err
	}

	// Fast path: if we are a follower and we know the leader, forward the RPC.
	// We do this outside the event loop for better concurrency.
	if n.State() != Leader {
		leaderID := n.Leader()
		if leaderID == "" {
			return 0, &NotLeaderError{}
		}
		// The durable term, not the current one. This request is built off the
		// event loop, so there is no write for it to be deferred against, and
		// the receiver steps down when it sees a term above its own. Sending a
		// term this node might forget in a crash would make a healthy leader
		// abandon its term for one that never existed.
		req := &ReadIndexRequest{
			GroupID: n.cfg.GroupID,
			Term:    Term(n.atomicDurableTerm.Load()),
		}
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
		return 0, n.stoppedErr()
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
		return 0, n.stoppedErr()
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
	if err := n.checkRunning(); err != nil {
		return 0, err
	}

	// A lease read is only as good as the promise that a leader which has lost
	// contact with its cluster stops being one. That promise is check-quorum,
	// and without it a leader partitioned away from everybody keeps its lease,
	// keeps believing it leads, and answers reads from a state machine the
	// rest of the cluster has moved on from -- silently, for as long as the
	// partition lasts. Refusing here rather than serving the read makes the
	// unsafe combination impossible to hold by accident.
	if !n.cfg.CheckQuorum {
		return 0, ErrLeaseReadUnavailable
	}

	if n.State() != Leader {
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
		return 0, n.stoppedErr()
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
		return 0, n.stoppedErr()
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
			return last, n.stoppedErr()
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

// SnapshotIndex returns the index of the last log entry included in the most
// recent snapshot taken by this node (i.e. the compaction boundary). Zero
// means no snapshot has been taken yet. Safe for concurrent use; reads from
// the atomic mirror updated by handleSnapshotResult.
func (n *Node) SnapshotIndex() Index {
	return Index(n.atomicSnapshotIndex.Load())
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
// caught-up. The caller may poll State() to observe the step-down.
func (n *Node) TransferLeadership(ctx context.Context, to NodeID) error {
	if err := n.checkRunning(); err != nil {
		return err
	}

	respCh := make(chan error, 1)
	msg := leadershipTransferMsg{target: to, respCh: respCh}
	select {
	case n.transferCh <- msg:
	case <-ctx.Done():
		return ctx.Err()
	case <-n.stopCh:
		return n.stoppedErr()
	}
	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-n.stopCh:
		return n.stoppedErr()
	}
}

// AddServer proposes adding a new peer to the cluster. It blocks until the
// change is committed and applied, or until ctx is cancelled.
// Returns ErrNotLeader if called on a non-leader, or ErrConfigChangeInProgress
// if another membership change is already in progress.
//
// Adding a peer with Voter true makes it count towards every quorum from the
// moment the change commits, while its log may still be empty. A three-node
// cluster becomes a four-node cluster needing three votes, one of which cannot
// be given until the new node has caught up, so it tolerates no failures at
// all until then. Use AddVoter unless that is what you meant: it stages the
// same node in as a learner and promotes it once it is caught up.
//
// Safety: AddServer uses the single-server change protocol (Raft §4.2), which
// is only safe when exactly one server is added or removed at a time. The
// ErrConfigChangeInProgress gate prevents concurrent changes, but callers must
// not attempt overlapping sequences (e.g. start an AddServer, then immediately
// call AddServer again before the first commits). For arbitrary topology
// changes — adding and removing multiple servers simultaneously — use
// ReconfigureCluster, which employs joint consensus.
func (n *Node) AddServer(ctx context.Context, peer PeerConfig) error {
	_, err := n.Propose(ctx, encodeConfigEntry(configOpAdd, peer))
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
//
// The removed node is told. It stops counting towards any quorum as soon as
// the leader appends the change, but the leader goes on replicating to it
// until it has acknowledged the entry and a commit index covering it, so that
// it steps down and its Config.OnRemoved fires rather than being left to
// campaign against a cluster that ignores it. That courtesy is bounded: a
// removed node that cannot be reached, or that is so far behind it would need
// a snapshot, is given up on.
func (n *Node) RemoveServer(ctx context.Context, id NodeID) error {
	_, err := n.Propose(ctx, encodeConfigEntry(configOpRemove, PeerConfig{ID: id}))
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
// ID and Voter status to retain it in the cluster; omit it to remove the
// current leader — the leader will step down automatically after the finalise
// entry commits.
// newMembers may be empty only if this node is also omitted (leaving the
// cluster with zero members is not useful; pass []PeerConfig{{ID: n.ID(), Voter: true}}
// for a single-node cluster).
//
// Returns ErrNotLeader if this node is not the leader, ErrConfigChangeInProgress
// if another membership change is already in flight, or ErrStopped if the node
// has been stopped.
func (n *Node) ReconfigureCluster(ctx context.Context, newMembers []PeerConfig) error {
	// Reject duplicate entries in newMembers. Duplicates would propagate into
	// the finalise entry and cfg.Peers, causing quorum calculations to overcount
	// a node, which could permanently prevent the cluster from committing
	// (liveness violation).
	seen := make(map[NodeID]struct{}, len(newMembers))
	for _, p := range newMembers {
		if _, dup := seen[p.ID]; dup {
			return fmt.Errorf("raft: ReconfigureCluster: duplicate member %q in newMembers", p.ID)
		}
		seen[p.ID] = struct{}{}
	}

	// Read the current peer list from the atomic mirror rather than cfg.Peers
	// directly. cfg.Peers is mutated by applyConfigChange inside the event
	// loop; reading it here (outside the loop) without synchronisation is a
	// data race.
	var oldPeers []PeerConfig
	if v := n.atomicPeers.Load(); v != nil {
		oldPeers = v.([]PeerConfig)
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
	if n.state != s {
		n.leadershipDirty = true
	}
	n.state = s
	n.atomicState.Store(uint32(s))
}

// setLeaderID updates n.leaderID and its atomic mirror. Event-loop only.
func (n *Node) setLeaderID(id NodeID) {
	if n.leaderID != id {
		n.leadershipDirty = true
	}
	n.leaderID = id
	n.atomicLeader.Store(string(id))
}

// setCommitIndex updates n.commitIndex and its atomic mirror. Event-loop only.
//
// commitIndex is monotonic: an entry, once committed, stays committed. A
// request that would move it backwards (a reordered or delayed RPC carrying an
// older LeaderCommit) is ignored rather than trusted.
func (n *Node) setCommitIndex(idx Index) {
	if idx <= n.commitIndex {
		return
	}
	n.commitIndex = idx
	n.atomicCommitIndex.Store(uint64(idx))
	n.maybeRecordCommit()
}

// commitRecordInterval is how far the commit index must move before the node
// writes it down again.
//
// It is the width of the band a recovery has to guess about, so smaller is
// better, bounded by what the write costs. The write is one fixed-size record
// rewritten in place with no fsync, which is a memcpy into the page cache; the
// queue entry carrying it to the writer costs more than the write does. Set
// against an append per proposal batch, one of these per 256 commits is noise,
// and writing on every commit would be measurable for a value almost no node
// ever reads.
const commitRecordInterval = 256

// maybeRecordCommit asks storage to remember how far the log has committed, if
// it can and if enough has changed to be worth a write.
//
// The value is the lower of the commit index and what is actually on this
// node's disk. Committed is not enough on its own: with asynchronous
// persistence a follower's commit index runs ahead of its own writes, and
// recording an index whose entries are still in memory would claim, to a
// recovery that happens after a crash, that entries the disk never received
// were committed.
//
// The queue gives the same guarantee by a longer route -- entries are handed
// to the writer before the commit index that covers them moves, so this
// operation is already ordered behind the writes it vouches for -- but that
// depends on every caller of setCommitIndex keeping to it. The clamp is local
// and does not.
//
// Event-loop only.
func (n *Node) maybeRecordCommit() {
	if n.writer == nil || n.writer.commits == nil {
		return
	}
	idx := min(n.commitIndex, n.log.stableIndex())
	if idx < n.recordedCommit+commitRecordInterval {
		return
	}
	n.recordedCommit = idx
	n.writer.enqueue(&writeOp{
		seq:          n.log.nextWriteSeq(),
		kind:         writeCommitIndex,
		index:        idx,
		durableAfter: n.log.queuedDurable,
	})
}

// --- Handler implementation -------------------------------------------------
// The Transport calls these from arbitrary goroutines. Each method serialises
// the request onto the event loop and blocks until a response is available.

// dispatchRPC is a generic helper that forwards an inbound RPC to the
// event-loop goroutine and waits for the response. The type parameter R is
// the expected concrete response type.
func dispatchRPC[R any](ctx context.Context, n *Node, req any) (R, error) {
	if err := n.checkRunning(); err != nil {
		var zero R
		return zero, err
	}

	respCh := make(chan rpcResponse, 1)
	env := rpcEnvelope{req: req, respCh: respCh}

	select {
	case n.rpcCh <- env:
	case <-ctx.Done():
		var zero R
		return zero, ctx.Err()
	case <-n.stopCh:
		var zero R
		return zero, n.stoppedErr()
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
		return zero, n.stoppedErr()
	}
}

// nodeHandler wraps a *Node and implements the Handler interface. It is the
// value registered with the Transport so that the Handle* dispatch methods do
// not appear on the public Node API.
type nodeHandler struct{ n *Node }

func (h *nodeHandler) HandleRequestVote(ctx context.Context, req *RequestVoteRequest) (*RequestVoteResponse, error) {
	return dispatchRPC[*RequestVoteResponse](ctx, h.n, req)
}

func (h *nodeHandler) HandleAppendEntries(ctx context.Context, req *AppendEntriesRequest) (*AppendEntriesResponse, error) {
	return dispatchRPC[*AppendEntriesResponse](ctx, h.n, req)
}

func (h *nodeHandler) HandleInstallSnapshot(ctx context.Context, req *InstallSnapshotRequest) (*InstallSnapshotResponse, error) {
	return dispatchRPC[*InstallSnapshotResponse](ctx, h.n, req)
}

func (h *nodeHandler) HandleTimeoutNow(ctx context.Context, req *TimeoutNowRequest) (*TimeoutNowResponse, error) {
	return dispatchRPC[*TimeoutNowResponse](ctx, h.n, req)
}

func (h *nodeHandler) HandleReadIndex(ctx context.Context, req *ReadIndexRequest) (*ReadIndexResponse, error) {
	return dispatchRPC[*ReadIndexResponse](ctx, h.n, req)
}

// Handler returns the Handler that this node registers with its Transport.
// Use this when you need to pass the node's RPC handler to a transport or
// router (e.g. Manager.Lookup returns Handler values for a shared transport).
func (n *Node) Handler() Handler {
	return n.handler
}

// --- Internal helpers -------------------------------------------------------

// Term returns the current term of this node. Safe for concurrent use.
func (n *Node) Term() Term {
	return Term(n.atomicTerm.Load())
}

// durableAppliedIndex asks a state machine that keeps its own durable state
// how far it has already got, and checks the answer is one this node can act
// on.
func (n *Node) durableAppliedIndex() (Index, error) {
	durable, ok := n.cfg.StateMachine.(DurableStateMachine)
	if !ok {
		return 0, nil
	}
	idx, err := durable.AppliedIndex(context.Background())
	if err != nil {
		return 0, fmt.Errorf("raft.New: state machine applied index: %w", err)
	}
	if last := n.log.lastLogIndex(); idx > last {
		// The state machine claims entries this node does not have. Replaying
		// is impossible and ignoring it would apply those indices a second
		// time when the log catches up, so there is nothing safe to do.
		return 0, fmt.Errorf(
			"raft.New: state machine reports applied index %d but the log ends at %d; "+
				"its storage is ahead of this node's log", idx, last)
	}
	return idx, nil
}

// saveTerm records currentTerm and votedFor and queues the write that makes
// them durable, returning the sequence number of that write.
//
// The term and the vote take effect here and now. What the caller must not do
// is act on them where another node can see it: the whole of Raft's
// one-vote-per-term rule is that a node which votes, crashes, and comes back
// having forgotten the vote can vote again in the same term, and two leaders
// in one term follows. So nothing this node sends may carry the new term until
// the returned write has completed -- see sendGate, which every handler that
// answers an RPC passes through.
//
// Must be called from the event-loop goroutine only.
func (n *Node) saveTerm(term Term, votedFor NodeID) uint64 {
	seq := n.log.nextWriteSeq()
	n.writer.enqueue(&writeOp{
		seq:          seq,
		kind:         writeHardState,
		hs:           HardState{CurrentTerm: term, VotedFor: votedFor},
		durableAfter: n.log.queuedDurable,
	})
	n.unsafeTermSeq = seq

	if n.currentTerm != term {
		n.leadershipDirty = true
	}
	n.currentTerm = term
	n.votedFor = votedFor
	n.atomicTerm.Store(uint64(term))
	return seq
}

// sendGate returns the write a message produced in this turn has to wait for.
//
// A reply carries this node's term, and sending it is a statement that the
// node is at that term: a leader counts an acknowledgement, a candidate counts
// a vote. If the term were not yet durable, a crash would take the node back
// to an earlier one, free to make a second and different decision in a term it
// had already decided. So a reply waits for whichever is later of the log
// write it depends on and the hard-state write, which in practice is the log
// write when there is one, because the hard state is always queued first.
func (n *Node) sendGate(writeSeq uint64) uint64 {
	if n.unsafeTermSeq > writeSeq {
		return n.unsafeTermSeq
	}
	return writeSeq
}

// traceRPC calls cfg.Tracer.StartRPC if a tracer is configured, returning the
// context to make the RPC with and the finish func. When no tracer is set it
// returns the context unchanged and a no-op func, so callers need no nil
// check. Safe to call from any goroutine.
func (n *Node) traceRPC(ctx context.Context, peer NodeID, rpcType RPCType) (rpcCtx context.Context, finish func(error)) {
	if n.cfg.Tracer == nil {
		return ctx, func(error) {}
	}
	return n.cfg.Tracer.StartRPC(ctx, n.cfg.ID, peer, rpcType)
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

// writeSnapshot streams a snapshot into storage and reports what it cost.
// write does the state-machine half; everything around it is the same whether
// the state was captured first or is being serialised in place.
func (n *Node) writeSnapshot(trig *snapshotTrigger, write func(context.Context, io.Writer) error) snapshotResult {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		errCh <- n.cfg.Storage.SaveSnapshot(n.stopCtx, trig.meta, pr)
	}()

	// Count the bytes on the way past. The size of a snapshot is the main
	// thing that decides how long a lagging follower takes to catch up, so it
	// is worth reporting, and this is the only place that sees it.
	counter := &countingWriter{w: pw}
	started := n.now()
	serr := writeSnapshotFrame(counter, &snapshotFrame{
		table:             trig.clientTable,
		membership:        trig.membership,
		clientTableCap:    trig.clientTableCap,
		hasClientTableCap: trig.hasClientTableCap,
	}, func(w io.Writer) error {
		return write(n.stopCtx, w)
	})
	_ = pw.Close() // signals EOF to SaveSnapshot

	saveErr := <-errCh
	if serr == nil {
		serr = saveErr
	}

	return snapshotResult{
		meta:              trig.meta,
		membership:        trig.membership,
		clientTableCap:    trig.clientTableCap,
		hasClientTableCap: trig.hasClientTableCap,
		sizeBytes:         counter.n,
		duration:          n.now().Sub(started),
		err:               serr,
	}
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	written, err := c.w.Write(p)
	c.n += int64(written)
	return written, err
}

// reportStorageWrite tells a StorageMetrics implementation how long a durable
// write took. A no-op unless Config.Metrics also implements it.
func (n *Node) reportStorageWrite(op string, d time.Duration, err error) {
	if n.cfg.Metrics == nil {
		return
	}
	sm, isStorageMetrics := n.cfg.Metrics.(StorageMetrics)
	if !isStorageMetrics {
		return
	}
	sm.StorageWrite(n.cfg.ID, op, d, err)
}

// deferredWrite is work that must not happen until a queued storage write has
// completed: an acknowledgement to a leader, a promise resolved for a client,
// anything whose meaning is "this is on disk".
//
// Deferring is the whole of what makes an asynchronous write safe. The entry
// enters the log immediately and is replicated and counted towards the vote
// gate straight away, because the in-memory log is what this node will act on
// for as long as it is running. What waits is every statement made to somebody
// else, because a statement survives a crash and the entry might not.
type deferredWrite struct {
	seq  uint64
	run  func()
	fail func(error)
}

// afterWrite registers work to run once the write with this sequence number
// has completed, or to be failed if it does not. Either may be nil.
//
// A seq of 0 means there was no write to wait for -- an append of no entries --
// and the work runs immediately. So does a write that has already completed:
// waiting is driven by completions, so work queued behind one that has been
// and gone would sit there until some later write happened to arrive, and
// would sit there for ever if none did.
func (n *Node) afterWrite(seq uint64, run func(), fail func(error)) {
	if seq == 0 || seq <= n.completedWriteSeq {
		if run != nil {
			run()
		}
		return
	}
	n.deferredWrites = append(n.deferredWrites, deferredWrite{seq: seq, run: run, fail: fail})
}

// handleWriteCompletions collects everything the storage writer has finished,
// advances the durable point, and releases the work that was waiting on it.
func (n *Node) handleWriteCompletions() {
	done := n.writer.takeDone()
	if len(done) == 0 {
		return
	}
	for i := range done {
		d := &done[i]
		n.reportStorageWrite(d.kind.String(), d.took, d.err)
		if d.err != nil {
			// The log is no longer trustworthy: an append or truncation that
			// failed leaves storage in a state this node cannot describe.
			// Everything waiting on a write fails with it, so no caller is
			// left holding a request that will never be answered.
			if d.seq > n.completedWriteSeq {
				n.completedWriteSeq = d.seq
			}
			n.fail(d.err, "durable "+d.kind.String())
			n.failDeferredWrites(d.err)
			continue
		}
		n.log.stabilize(*d)
		if d.seq > n.completedWriteSeq {
			n.completedWriteSeq = d.seq
		}
		if n.unsafeTermSeq != 0 && d.seq >= n.unsafeTermSeq {
			// The most recent hard-state write has landed, and no later one
			// has been queued or this would have been set again, so the term
			// the node is using is now the term on disk.
			n.unsafeTermSeq = 0
			n.atomicDurableTerm.Store(uint64(n.currentTerm))
		}
		n.runDeferredWrites(d.seq)
	}
	n.onDurableAdvanced()
}

// runDeferredWrites releases the work waiting on writes up to and including
// seq.
//
// The whole list is scanned rather than a leading run of it, because the list
// is not sorted. Work registered in one turn can wait on a write queued in an
// earlier one -- an acknowledgement that needs only the term this node adopted
// two turns ago, say -- and would otherwise sit behind an unrelated append
// that happened to be queued later, which is exactly the delay this change
// exists to remove.
//
// The list is taken before the callbacks run so that anything they register
// waits for its own write rather than being released by this one.
func (n *Node) runDeferredWrites(seq uint64) {
	pending := n.deferredWrites
	n.deferredWrites = nil

	kept := pending[:0]
	for i := range pending {
		if pending[i].seq > seq {
			kept = append(kept, pending[i])
			continue
		}
		if r := pending[i].run; r != nil {
			r()
		}
	}
	n.deferredWrites = append(kept, n.deferredWrites...)
	if len(n.deferredWrites) == 0 {
		n.deferredWrites = nil
	}
}

// failDeferredWrites reports err to everything still waiting on a write.
func (n *Node) failDeferredWrites(err error) {
	pending := n.deferredWrites
	n.deferredWrites = nil
	for i := range pending {
		if f := pending[i].fail; f != nil {
			f(err)
		}
	}
}

// answerSnapInstallAck releases the held InstallSnapshot response, if the
// snapshot it was waiting on is the one that just finished.
func (n *Node) answerSnapInstallAck(index Index, err error) {
	ack := n.snapInstallAck
	if ack == nil || ack.index != index {
		return
	}
	n.snapInstallAck = nil
	ack.answer(err)
}

// failSnapInstallAck reports err to a held InstallSnapshot response that will
// never be completed, so the leader learns at once rather than waiting out its
// own timeout.
func (n *Node) failSnapInstallAck(err error) {
	ack := n.snapInstallAck
	if ack == nil {
		return
	}
	n.snapInstallAck = nil
	ack.answer(err)
}

// onDurableAdvanced reacts to the durable point having moved.
//
// Two things depend on it and nothing else does. The apply loop reads entries
// back from storage, so it may only be told about a commit index that is
// actually there. And a leader counts its own log towards a commit quorum only
// as far as the disk has got, so an append landing can be what finally makes
// an entry committed even though no peer said anything.
func (n *Node) onDurableAdvanced() {
	n.notifyApply()
	if n.state == Leader {
		n.maybeAdvanceCommit()
	}
	// A follower's commit index is usually ahead of its disk, so the value
	// worth recording moves when the disk catches up rather than when the
	// commit index does.
	n.maybeRecordCommit()
}

// reportProposal tells a ProposalMetrics implementation how a proposal ended.
//
// Always called before the proposal's own promise is resolved or rejected,
// because resolving it releases the caller: report afterwards and a caller
// that scrapes its metrics as soon as Propose returns can miss the very
// proposal it just made.
// Event-loop only; a no-op unless Config.Metrics also implements it.
func (n *Node) reportProposal(submitted time.Time, ok bool) {
	if submitted.IsZero() || n.cfg.Metrics == nil {
		return
	}
	pm, isProposalMetrics := n.cfg.Metrics.(ProposalMetrics)
	if !isProposalMetrics {
		return
	}
	pm.ProposalCompleted(n.cfg.ID, n.now().Sub(submitted), ok)
}

// defaultMaxUnstableLogBytes is the backlog limit used when Config leaves
// MaxUnstableLogBytes at zero, so that a configuration written before the
// field existed still has a bound.
const defaultMaxUnstableLogBytes = 64 << 20

// unstableLimit returns the byte limit on log entries held in memory awaiting
// a write.
func (n *Node) unstableLimit() int {
	if n.cfg.MaxUnstableLogBytes != 0 {
		// Negative says no limit, and is the only way to say it: zero selects
		// the default, because a node with no bound answers a slow disk by
		// running out of memory.
		if n.cfg.MaxUnstableLogBytes < 0 {
			return 0
		}
		return n.cfg.MaxUnstableLogBytes
	}
	return defaultMaxUnstableLogBytes
}

// proposalLimit returns the largest command this node will accept, or 0 when
// no limit applies. Config.MaxProposalBytes wins; otherwise the transport is
// asked, leaving headroom for the framing that surrounds the command on the
// wire.
func (n *Node) proposalLimit() int {
	if n.cfg.MaxProposalBytes > 0 {
		return n.cfg.MaxProposalBytes
	}
	limiter, ok := n.cfg.Transport.(MessageSizeLimiter)
	if !ok {
		return 0
	}
	limit := limiter.MaxMessageBytes()
	if limit <= 0 {
		return 0
	}
	// The command is not the whole message: the request carries the term, the
	// leader ID, the previous-log fields and each entry's own header, and the
	// transport adds its framing on top. Reserve a slice of the budget for all
	// of that rather than accepting a command that only just fits on paper.
	reserved := limit / 16
	if reserved < proposalFramingReserve {
		reserved = proposalFramingReserve
	}
	if reserved >= limit {
		return 0
	}
	return limit - reserved
}

// proposalFramingReserve is the minimum headroom left for request and entry
// framing when deriving a proposal limit from the transport.
const proposalFramingReserve = 4096

// checkProposalSize reports whether a command of this size can be replicated.
func (n *Node) checkProposalSize(size int) error {
	limit := n.proposalLimit()
	if limit > 0 && size > limit {
		return fmt.Errorf("%w: %d bytes exceeds the %d the transport can carry",
			ErrProposalTooLarge, size, limit)
	}
	return nil
}

// trailingLogs returns how many entries to retain behind the snapshot point,
// capped so that compaction always reclaims something.
func (n *Node) trailingLogs() Index {
	trailing := Index(n.cfg.TrailingLogs)
	if n.cfg.SnapshotThreshold > 0 && trailing >= Index(n.cfg.SnapshotThreshold) {
		trailing = Index(n.cfg.SnapshotThreshold) - 1
	}
	return trailing
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
		id := peer.ID
		ch := make(chan *AppendEntriesRequest, 1)
		stop := make(chan struct{})
		n.hbPumps[id] = ch
		n.hbStopChs[id] = stop
		go n.runHBPump(id, ch, stop)
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
	// fails counts heartbeats to this peer that got no answer, in a row. It
	// lives here rather than on the event loop so that neither a peer which is
	// up nor one which is down costs a message per heartbeat: only crossing
	// the threshold, and recovering from it, is reported.
	fails := 0
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
		ctx, finish := n.traceRPC(ctx, peer, RPCAppendEntries)
		resp, err := n.cfg.Transport.AppendEntries(ctx, peer, req)
		finish(err)
		cancel()
		if err != nil || resp == nil {
			// A missed heartbeat is not itself news -- the next tick sends
			// another -- but a peer that misses them all is the fact that
			// decides whether the next failure costs the cluster its quorum,
			// and heartbeats are the only traffic an idle leader sends.
			//
			// Only the change is reported, which is the whole of why this
			// counts here rather than sending every failure to the event loop
			// to be counted there: an unreachable peer is retried every
			// heartbeat interval, so a message per failure would put one
			// message per peer per interval into the event loop's queue for as
			// long as the outage lasted. That is the moment a partitioned
			// cluster can least afford the extra traffic, and it is competing
			// with the snapshot the leader is trying to send.
			fails++
			if fails == peerUnresponsiveFailures {
				select {
				case n.rpcCh <- rpcEnvelope{req: &peerReachability{peer: peer}}:
				case <-stop:
					return
				case <-n.stopCtx.Done():
					return
				}
			}
			continue
		}
		if fails >= peerUnresponsiveFailures {
			select {
			case n.rpcCh <- rpcEnvelope{req: &peerReachability{peer: peer, reachable: true}}:
			case <-stop:
				return
			case <-n.stopCtx.Done():
				return
			}
		}
		fails = 0
		select {
		case n.rpcCh <- rpcEnvelope{req: &appendResult{
			peer:          peer,
			term:          resp.Term,
			success:       resp.Success,
			req:           req,
			conflictIndex: resp.ConflictIndex,
			conflictTerm:  resp.ConflictTerm,
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
//
// current is the apply goroutine's current client table. If the snapshot is
// stale (its index ≤ localLastApplied) the restore is skipped and current is
// returned unchanged. This guards against the following race: a duplicate
// install result (caused by two concurrent snapshot goroutines both completing
// for the same snapshot index) can push two restores onto restoreSnapshotCh;
// if the second fires after log entries beyond the snapshot have already been
// applied, skipping it prevents overwriting the newer SM state.
func (n *Node) applyRestore(ctx context.Context, si snapshotInstall, localLastApplied *Index, current *clientLRU) *clientLRU {
	defer func() { _ = si.r.Close() }()
	if si.meta.LastIncludedIndex <= *localLastApplied {
		// Stale restore: the SM already reflects a more recent state.
		return current
	}
	if err := n.cfg.StateMachine.Restore(ctx, si.meta, si.r); err != nil {
		n.logger.Error("applyLoop: Restore", "err", err)
	}
	*localLastApplied = si.meta.LastIncludedIndex
	n.atomicLastApplied.Store(uint64(si.meta.LastIncludedIndex))
	select {
	case n.applyAdvancedCh <- struct{}{}:
	default:
	}
	capacity := current.capacity()
	if si.hasClientTableCap {
		capacity = si.clientTableCap
	}
	newTable := newClientLRU(capacity)
	newTable.loadFrom(si.clientTable)
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

	// sat measures how much of this loop's time goes on work rather than on
	// waiting for it. It is nil, and every call on it a no-op, unless
	// Config.Metrics implements ApplyMetrics. See apply_saturation.go.
	sat := n.applySaturationTracker()
	defer sat.stop()

	// localClientTable is the apply goroutine's own copy of the dedup table.
	// It is used to enforce exactly-once semantics for ProposeOnce entries: if
	// a (clientID, seqNum) pair has already been applied, SM.Apply is skipped
	// and the cached result is returned instead.
	//
	// Keeping a separate copy here (rather than reading n.clientTable) is
	// necessary because n.clientTable is owned by the event-loop goroutine and
	// must not be read from the apply goroutine without synchronisation. It is
	// bounded exactly like the event loop's copy and updated from the same
	// sequence of entries, so the two hold the same contents, and so does every
	// other replica's: whether a retry is deduplicated must not depend on which
	// replica applies it.
	// It starts under the bound in effect at the snapshot, when there was
	// one, and follows the log's cap entries from there in apply order.
	localClientTable := newClientLRU(n.applyStartCap)

	// On restart from a snapshot: restore the state machine once before
	// processing any committed entries. initialSnap is set once in New()
	// and is only read here, so there is no data race.
	if n.initialSnap != nil {
		_, r, err := n.cfg.Storage.LoadSnapshot(ctx)
		if err == nil {
			// Skip the framing header (meta and table already handled in New).
			_, _, _, smReader, rerr := readWrappedSnapshot(r)
			if rerr == nil {
				if restoreErr := n.cfg.StateMachine.Restore(ctx, n.initialSnap.meta, smReader); restoreErr != nil {
					n.logger.Error("applyLoop: initial snapshot restore", "err", restoreErr)
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
		localClientTable.loadFrom(n.initialSnap.clientTable)
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
			sat.working()
			localClientTable = n.applyRestore(ctx, si, &localLastApplied, localClientTable)
			continue
		default:
		}

		// Everything below the priority select is time spent waiting for work;
		// everything a case does with the work is time spent on it.
		sat.waiting()

		select {
		case si := <-n.restoreSnapshotCh:
			sat.working()
			localClientTable = n.applyRestore(ctx, si, &localLastApplied, localClientTable)

		case <-sat.samples():
			// Nothing to apply, but the measurement still has to move: a gauge
			// left at the last busy reading would report a loop that stopped
			// working as one that never stops.
			sat.sample()

		case trig := <-n.snapshotTriggerCh:
			sat.working()
			// A state machine that can hand over a point-in-time capture
			// cheaply lets the serialisation move off this goroutine, so
			// entries keep applying while the snapshot is written. Otherwise
			// Snapshot runs here, because it must not run concurrently with
			// Apply, and apply waits for it.
			if capturer, ok := n.cfg.StateMachine.(SnapshotCapturer); ok {
				captured, err := capturer.Capture(ctx)
				if err != nil {
					n.logger.Error("snapshot: capture failed", "err", err)
					select {
					case n.snapshotResultCh <- snapshotResult{meta: trig.meta, err: err}:
					case <-n.stopCh:
						return
					}
					continue
				}
				n.snapshotWriteWg.Add(1)
				go func(trig snapshotTrigger, captured Snapshot) {
					defer n.snapshotWriteWg.Done()
					defer captured.Release()
					res := n.writeSnapshot(&trig, captured.Write)
					select {
					case n.snapshotResultCh <- res:
					case <-n.stopCh:
					}
				}(trig, captured)
				continue
			}

			res := n.writeSnapshot(&trig, func(wctx context.Context, w io.Writer) error {
				return n.cfg.StateMachine.Snapshot(wctx, w)
			})
			select {
			case n.snapshotResultCh <- res:
			case <-n.stopCh:
				return
			}

		case commitIdx := <-n.commitNotifyCh:
			sat.working()
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
			// pending collects a run of entries the state machine can be given
			// in one call, when it can take them that way. Anything the Raft
			// layer answers itself -- a config entry, a duplicate whose result
			// is already known -- interrupts the run, so results still leave
			// this loop in index order.
			var (
				pending      []LogEntry
				pendingRaw   []LogEntry
				pendingDedup []dedupNote
			)
			flush := func() bool {
				if len(pending) == 0 {
					return true
				}
				ok := n.applyPending(ctx, pending, pendingRaw, pendingDedup,
					localClientTable, &localLastApplied)
				pending, pendingRaw, pendingDedup = nil, nil, nil
				return ok
			}

			for _, entry := range entries {
				i := entry.Index
				// Config entries are handled by the Raft layer; do not forward
				// to the user state machine.
				var ar applyResult
				switch {
				case isConfigEntry(entry.Command):
					if !flush() {
						return
					}
					if capacity, ok := decodeClientTableCapEntry(entry.Command); ok {
						// The event loop reports the evictions; this copy
						// only has to make the same ones.
						localClientTable.setCap(capacity)
					}
					ar = applyResult{index: i, configCmd: entry.Command, cmd: entry.Command}
				case isDedupCmd(entry.Command):
					// ProposeOnce command: enforce exactly-once by checking the
					// local dedup table BEFORE calling SM.Apply. This closes the
					// window where a retry is sent to a new leader while the
					// original committed entry has not yet been applied, resulting
					// in two log entries with the same (clientID, seqNum).
					clientID, seqNum, payload, decErr := decodeDedupCmd(entry.Command)
					switch {
					case decErr != nil:
						// Malformed dedup header; apply as-is.
						pending = append(pending, entry)
						pendingRaw = append(pendingRaw, entry)
						pendingDedup = append(pendingDedup, dedupNote{})
						continue
					default:
						if cached, ok := localClientTable.get(clientID); ok && seqNum == cached.seqNum {
							// Exact duplicate: the answer is already known, so
							// the state machine must not see it again.
							if !flush() {
								return
							}
							ar = applyResult{index: i, val: cached.result, cmd: entry.Command}
							break
						}
						// New or first-seen: strip the header and queue it.
						smEntry := entry
						smEntry.Command = payload
						pending = append(pending, smEntry)
						pendingRaw = append(pendingRaw, entry)
						pendingDedup = append(pendingDedup, dedupNote{
							set: true, clientID: clientID, seqNum: seqNum,
						})
						continue
					}
				default:
					pending = append(pending, entry)
					pendingRaw = append(pendingRaw, entry)
					pendingDedup = append(pendingDedup, dedupNote{})
					continue
				}
				select {
				case n.applyResultCh <- ar:
					localLastApplied = i
				case <-n.stopCh:
					return
				}
			}
			if !flush() {
				return
			}

		case <-n.stopCh:
			return
		}
	}
}

// ClientTableSize returns how many clients the exactly-once table currently
// holds. Safe for concurrent use; reads an atomic mirror the event loop keeps
// in sync.
//
// It is the measurement to watch before anything goes wrong, as opposed to the
// eviction reports, which arrive after. The table is bounded by
// Config.MaxClientTableSize, and while it is below that bound every ProposeOnce
// retry is answered from it and the exactly-once guarantee holds absolutely.
// Once it reaches the bound, admitting a client means forgetting one, and a
// forgotten client's retry runs a second time. So the useful alert is on this
// value approaching MaxClientTableSize, with the eviction count as the
// confirmation that the deadline was missed.
//
// The value is the same on every node in a healthy cluster: the table is
// replicated state, built from the same entries in the same order everywhere,
// under a bound the group agreed on rather than one each node configured.
func (n *Node) ClientTableSize() int {
	return int(n.clientTableSize.Load())
}

// MaxClientTableSize returns the bound the exactly-once table is kept under,
// 0 meaning none. Safe for concurrent use.
//
// It is the group's value, not this node's Config.MaxClientTableSize: the
// bound is replicated, so that every node evicts the same client at the same
// point. Until the group has agreed one -- which happens when a leader
// appends the first ProposeOnce entry -- it is this node's own configuration.
func (n *Node) MaxClientTableSize() int {
	return int(n.atomicClientTableCap.Load())
}

// SetMaxClientTableSize changes the bound of the exactly-once table for the
// whole group, 0 meaning none. It blocks until the change is committed and
// applied, or until ctx is cancelled, and returns ErrNotLeader on any node but
// the leader.
//
// This is the way to resize the table once a cluster is running, because
// Config.MaxClientTableSize is only what a node proposes when the group has no
// agreed bound yet. A smaller bound evicts the oldest entries everywhere at
// the same point in the log, each eviction reported as usual; a larger one
// takes effect from that point on and recovers nothing already forgotten.
//
// Like a membership change, only one is in flight at a time:
// ErrConfigChangeInProgress is returned while another configuration change is
// pending.
func (n *Node) SetMaxClientTableSize(ctx context.Context, size int) error {
	if size < 0 {
		return errors.New("raft: SetMaxClientTableSize: size must not be negative (0 means unlimited)")
	}
	_, err := n.Propose(ctx, encodeClientTableCapEntry(size))
	return err
}

// adoptClientTableCap puts a group-agreed bound into effect on the event
// loop's table, reporting each client it evicts. source says where the bound
// came from, for the log line written when it differs from this node's own
// configuration. Event-loop only, or before Start.
func (n *Node) adoptClientTableCap(capacity int, source string) {
	if capacity != n.cfg.MaxClientTableSize && (!n.clientTableCapAgreed || capacity != n.clientTableCap) {
		n.logger.Warn("exactly-once table bound taken from the cluster, not this node's Config",
			"clusterMaxClientTableSize", capacity,
			"configMaxClientTableSize", n.cfg.MaxClientTableSize,
			"source", source)
	}
	n.clientTableCap = capacity
	n.clientTableCapAgreed = true
	n.clientTableCapReplicated = true
	n.atomicClientTableCap.Store(int64(capacity))
	for _, id := range n.clientTable.setCap(capacity) {
		n.reportClientForgotten(id)
	}
	n.syncClientTableSize()
}

// syncClientTableSize refreshes the mirror ClientTableSize reads. Called from
// the event loop after anything that changes the table's occupancy, which is a
// put and the wholesale replacement a snapshot restore does.
func (n *Node) syncClientTableSize() {
	n.clientTableSize.Store(int64(n.clientTable.len()))
}

// witnessStateMachine is what a witness applies to: nothing. Its snapshots
// are empty and it drains whatever a restore hands it, so that a witness
// takes part in log compaction and snapshot installs without holding state.
type witnessStateMachine struct{}

func (witnessStateMachine) Apply(context.Context, LogEntry) ([]byte, error) { return nil, nil }
func (witnessStateMachine) Snapshot(context.Context, io.Writer) error       { return nil }
func (witnessStateMachine) Restore(_ context.Context, _ SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// stripForWitness returns entries with every command a witness does not need
// removed. Config entries are kept: a witness tracks membership and the
// group's policies like any member, and they are small. Everything else is
// reduced to its index and term. The result is a fresh slice; entries is not
// modified, since it may alias the log.
func stripForWitness(entries []LogEntry) []LogEntry {
	out := make([]LogEntry, len(entries))
	for i, e := range entries {
		out[i] = LogEntry{Index: e.Index, Term: e.Term}
		if isConfigEntry(e.Command) {
			out[i].Command = e.Command
		}
	}
	return out
}

// isWitnessPeer reports whether the membership marks peer as a witness.
// Event-loop only.
func (n *Node) isWitnessPeer(peer NodeID) bool {
	if i := indexOfPeer(n.cfg.Peers, peer); i >= 0 {
		return n.cfg.Peers[i].Witness
	}
	return false
}
