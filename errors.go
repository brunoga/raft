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
