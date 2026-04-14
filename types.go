// Package raft provides the core types and interfaces for the Raft consensus
// algorithm implementation.
package raft

import "time"

// NodeID uniquely identifies a node in the cluster.
type NodeID string

// Term is a logical clock in Raft. It increases monotonically.
type Term uint64

// Index is a 1-based position in the Raft log.
type Index uint64

// State represents the role a Raft node is currently playing.
type State uint8

const (
	Follower     State = iota
	Candidate    State = iota
	Leader       State = iota
	PreCandidate State = iota // running pre-vote round before incrementing term
)

func (s State) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	case PreCandidate:
		return "PreCandidate"
	default:
		return "Unknown"
	}
}

// LogEntry is a single record in the Raft log.
type LogEntry struct {
	Index   Index
	Term    Term
	Command []byte
}

// HardState is the persistent state that must be saved to stable storage
// before responding to any RPC.
type HardState struct {
	CurrentTerm Term
	VotedFor    NodeID // empty string means no vote cast
}

// SnapshotMeta carries the metadata associated with a state-machine snapshot.
type SnapshotMeta struct {
	LastIncludedIndex Index
	LastIncludedTerm  Term
}

// Clock is an injectable time source. The default nil value uses time.Now.
// Inject a fake clock in tests to make lease expiry deterministic.
type Clock interface {
	Now() time.Time
}

// Tracer is an optional hook called for every outbound Raft RPC. It can be
// used to record per-RPC latency, error rates, and traces.
// Implementations must be safe for concurrent use from multiple goroutines
// (there is one RPC goroutine per peer per outstanding RPC).
type Tracer interface {
	// StartRPC is called immediately before an outbound RPC is sent.
	// nodeID is the local node, peer is the destination, rpcType is one of
	// "RequestVote", "AppendEntries", "InstallSnapshot", "TimeoutNow".
	// The returned finish func must be called exactly once when the RPC
	// completes; err is nil on success, non-nil on network error or timeout.
	StartRPC(nodeID NodeID, peer NodeID, rpcType string) (finish func(err error))
}

// Metrics is an optional observability hook. Implementations must be safe for
// concurrent use from the event-loop goroutine.
type Metrics interface {
	// StateChange is called on every role transition.
	StateChange(id NodeID, from, to State, term Term)
	// CommitAdvanced is called each time commitIndex moves forward.
	CommitAdvanced(id NodeID, commitIndex Index)
	// SnapshotTaken is called after a snapshot is successfully persisted.
	SnapshotTaken(id NodeID, lastIncludedIndex Index, sizeBytes int)
}
