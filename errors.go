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

	// ErrWriteBacklogFull is returned by Propose and ProposeOnce when the
	// leader is already holding Config.MaxUnstableLogBytes of log entries that
	// storage has not caught up with. It means the disk is behind, not that
	// anything is wrong with the proposal: retry, after a pause or against
	// another node.
	ErrWriteBacklogFull = errors.New("raft: log writes are behind; proposal refused")

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
