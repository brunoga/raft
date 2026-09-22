package raft

import "errors"

// Sentinel errors returned by the Raft node and Storage implementations.
var (
	// ErrNotLeader is returned when a client proposal is sent to a non-leader.
	// Use errors.As(err, &NotLeaderError{}) to extract the leader hint.
	ErrNotLeader = errors.New("raft: node is not the leader")

	// ErrNotFound is returned when a requested log index does not exist.
	ErrNotFound = errors.New("raft: log entry not found")

	// ErrNoSnapshot is returned when no snapshot has been saved yet.
	ErrNoSnapshot = errors.New("raft: no snapshot available")

	// ErrCompacted is returned when the requested index has been compacted
	// away (i.e., it is before FirstIndex).
	ErrCompacted = errors.New("raft: log entry has been compacted")

	// ErrConfigChangeInProgress is returned when a membership change is
	// proposed while one is already outstanding.
	ErrConfigChangeInProgress = errors.New("raft: a config change is already in progress")

	// ErrLeadershipTransferInProgress is returned when TransferLeadership is
	// called while a transfer is already underway.
	ErrLeadershipTransferInProgress = errors.New("raft: a leadership transfer is already in progress")

	// ErrObsoleteSeqNum is returned by ProposeOnce when the supplied sequence
	// number is strictly less than the highest sequence number already recorded
	// for that client. The client should not retry with this seqNum.
	ErrObsoleteSeqNum = errors.New("raft: sequence number is obsolete")

	// ErrNodeFailed is returned by every operation on a node that has stopped
	// because it could not complete a durable write. Use errors.Is to detect
	// it; the returned error also wraps the underlying storage error.
	//
	// Raft's safety argument assumes that a node's term, vote and log reach
	// stable storage before it acts on them. A node that cannot write can no
	// longer honour that, so it stops rather than continuing with state that
	// may not survive a restart. Operator action is required: the node has to
	// be restarted once its storage is healthy.
	ErrNodeFailed = errors.New("raft: node stopped after a durable write failed")

	// ErrManagerStopping is returned by Manager.Add while StopAll is in
	// progress. A node added at that moment would not be stopped and nothing
	// would track it. Retry once StopAll has returned.
	ErrManagerStopping = errors.New("raft: manager is stopping")

	// ErrMemberNotCaughtUp is returned by PromoteMember when the member is too
	// far behind to be made a voter. Retry once it has caught up.
	ErrMemberNotCaughtUp = errors.New("raft: member is too far behind to become a voter")

	// ErrNotMember is returned when an operation names a node that is not part
	// of the cluster.
	ErrNotMember = errors.New("raft: node is not a member of this cluster")

	// ErrLeaseReadUnavailable is returned by ReadIndexLease when
	// Config.CheckQuorum is disabled.
	//
	// A lease read assumes a leader that has lost contact with its cluster
	// stops being one, and check-quorum is what makes that true. Without it a
	// partitioned leader keeps its lease and serves reads from a state machine
	// the rest of the cluster has moved past. Use ReadIndex, which pays for a
	// round of heartbeats and needs no such assumption, or enable
	// Config.CheckQuorum.
	ErrLeaseReadUnavailable = errors.New("raft: lease reads require Config.CheckQuorum")

	// ErrWriteBacklogFull means a node is already holding
	// Config.MaxUnstableLogBytes of log entries that storage has not caught up
	// with, and will not take on more until it has.
	//
	// Propose and ProposeOnce return it to the caller. A follower also refuses
	// an AppendEntries with it, which the leader sees as a failed RPC and
	// retries; in ordinary operation that cannot happen, because what a leader
	// sends before being acknowledged is already bounded by MaxInflightRPCs
	// and MaxBytesPerRPC, and the limit exists for the leader that does not
	// respect them.
	//
	// It means the disk is behind, not that anything is wrong with the
	// request: retry, after a pause or against another node.
	ErrWriteBacklogFull = errors.New("raft: log writes are behind")

	// ErrProposalTooLarge is returned by Propose and ProposeOnce for a command
	// that could never be replicated, because the resulting log entry would
	// exceed the largest message the transport can carry.
	//
	// The alternative to refusing it is worse: the entry is durable in the
	// leader's log, the transport rejects every attempt to send it, and the
	// leader retries the identical message for ever. The entry never commits,
	// so every later proposal queues behind it and the group stops making
	// progress, with nothing in the API having reported a problem.
	ErrProposalTooLarge = errors.New("raft: proposal is too large to replicate")

	// ErrProposalQueueFull is returned by Propose and ProposeOnce when the
	// proposal queue is full and Config.ProposalOverflow is
	// ProposalOverflowReject.
	//
	// It means the event loop is behind, not that anything is wrong with the
	// request. Nothing was appended; retry after a pause, or against another
	// node. A caller that would rather wait than see this should leave
	// ProposalOverflow at its default.
	ErrProposalQueueFull = errors.New("raft: proposal queue is full")

	// ErrWitnessMismatch reports that the cluster's membership and a node's
	// Config disagree about whether it is a witness. New returns it when the
	// membership recovered from disk disagrees with Config.Witness; a running
	// full node that applies a membership entry calling it a witness stops
	// with it, since it would otherwise apply stripped entries. Fix the
	// Config or the membership, and restart.
	ErrWitnessMismatch = errors.New("raft: membership and Config disagree about whether this node is a witness")

	// ErrLeaseExpired is returned by ReadIndexLease when the leader does not
	// currently hold a valid clock-based read lease. The caller should fall back
	// to ReadIndex (which performs a heartbeat round-trip) or retry after the
	// next heartbeat interval.
	ErrLeaseExpired = errors.New("raft: read lease has expired")
)

// NotLeaderError is returned when a client proposal is sent to a non-leader.
// The Leader field carries a hint about who the current leader is; it may be
// empty if this node does not know.
type NotLeaderError struct {
	// Leader is the NodeID of the current leader, or empty if unknown.
	Leader NodeID
}

// Error implements the error interface, naming the leader when one is known.
func (e *NotLeaderError) Error() string {
	if e.Leader == "" {
		return "raft: node is not the leader"
	}
	return "raft: node is not the leader (leader: " + string(e.Leader) + ")"
}

// Is makes errors.Is(err, ErrNotLeader) return true for *NotLeaderError.
func (e *NotLeaderError) Is(target error) bool {
	return target == ErrNotLeader
}
