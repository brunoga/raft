package raft

import (
	"context"
	"fmt"
	"time"
)

// NodeID uniquely identifies a node in the cluster.
type NodeID string

// Term is a logical clock in Raft. It increases monotonically.
type Term uint64

// Index is a 1-based position in the Raft log.
type Index uint64

// State represents the role a Raft node is currently playing.
type State uint8

const (
	// Follower replicates the leader's log and votes. It is where every node
	// starts and where every node returns when it sees a higher term.
	Follower State = iota
	// Candidate is standing in an election, having raised its own term and
	// voted for itself.
	Candidate
	// Leader is the single node in a term that accepts proposals and drives
	// replication.
	Leader
	// PreCandidate is testing whether it could win an election before raising
	// its term, so that a node which cannot win does not disrupt a healthy
	// cluster by forcing everyone to a new term.
	PreCandidate
)

// String returns the role's name, as it appears in logs and metrics.
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

// MarshalText implements encoding.TextMarshaler so that State serialises as a
// human-readable string (e.g. "Leader") in JSON and other text formats.
func (s State) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler, reversing MarshalText.
func (s *State) UnmarshalText(text []byte) error {
	switch string(text) {
	case "Follower":
		*s = Follower
	case "Candidate":
		*s = Candidate
	case "Leader":
		*s = Leader
	case "PreCandidate":
		*s = PreCandidate
	default:
		return fmt.Errorf("raft: unknown State %q", string(text))
	}
	return nil
}

// LogEntry is a single record in the Raft log.
type LogEntry struct {
	// Index is the entry's position in the log, counting from 1.
	Index Index
	// Term is the term of the leader that created this entry. An index and a
	// term together identify an entry uniquely across the whole cluster,
	// which is what every consistency check in Raft compares.
	Term Term
	// Command is the opaque payload handed to StateMachine.Apply once the
	// entry commits. The Raft layer never interprets it, except to recognise
	// the entries it creates for itself.
	Command []byte
}

// HardState is the persistent state that must be saved to stable storage
// before responding to any RPC.
type HardState struct {
	// CurrentTerm is the highest term this node has seen.
	CurrentTerm Term
	// VotedFor is the candidate this node voted for in CurrentTerm, or empty
	// when it has not voted in that term. Losing this across a restart is how
	// one term ends up with two leaders, which is why it is persisted before
	// the node acts on it.
	VotedFor NodeID
}

// SnapshotMeta carries the metadata associated with a state-machine snapshot.
type SnapshotMeta struct {
	// LastIncludedIndex is the last log index whose effect the snapshot
	// contains. Everything at or below it may be compacted away.
	LastIncludedIndex Index
	// LastIncludedTerm is the term of the entry at LastIncludedIndex. It is
	// what lets a node that has compacted the entry itself still answer the
	// consistency check for that position.
	LastIncludedTerm Term
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
	// StartRPC is called immediately before an outbound RPC is sent. nodeID is
	// the local node, peer is the destination.
	//
	// The context passed in is the one the RPC will be made with, and the
	// context returned replaces it. That is what makes this usable for
	// tracing rather than only for timing: a span created here can be a child
	// of the caller's, and an implementation that propagates trace context
	// over the wire can attach it to the returned context for the transport
	// to carry. Returning ctx unchanged is fine for an implementation that
	// only measures.
	//
	// The returned finish func must be called exactly once when the RPC
	// completes; err is nil on success, non-nil on network error or timeout.
	StartRPC(ctx context.Context, nodeID, peer NodeID, rpcType RPCType) (rpcCtx context.Context, finish func(err error))
}

// HostID identifies a physical machine that hosts Raft nodes, as distinct from
// NodeID, which identifies one Raft node within one group.
//
// The two are not interchangeable and the difference is the whole point of
// leader balancing: the thing being balanced is the number of groups each
// machine leads, and a machine runs many nodes. Both used to be NodeID, which
// meant the balancing API took a map whose keys and whose values' NodeID
// fields were different kinds of name that happened to share a type.
type HostID string

// ZoneID names a failure domain: a rack, an availability zone, a datacentre.
// It is whatever unit of infrastructure is expected to fail as one.
type ZoneID string

// RPCType names one of the Raft RPCs, as reported to a Tracer.
type RPCType string

// The RPCs a node sends. These are the only values passed to Tracer.StartRPC.
const (
	RPCRequestVote     RPCType = "RequestVote"
	RPCAppendEntries   RPCType = "AppendEntries"
	RPCInstallSnapshot RPCType = "InstallSnapshot"
	RPCTimeoutNow      RPCType = "TimeoutNow"
	RPCReadIndex       RPCType = "ReadIndex"
)

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

// StorageMetrics is an optional interface that a Config.Metrics implementation
// may also satisfy. When it does, the node reports how long each durable write
// took.
//
// These are the writes the event loop waits on: the term and vote, and log
// entries. A disk that has become slow shows up here before it shows up
// anywhere else, and it explains a rise in proposal latency that nothing in
// the Raft state would otherwise account for. Watching it is how an operator
// tells "the network is slow" from "this node's disk is slow".
//
// Implementations must not block; they are called from the goroutine that made
// the write.
type StorageMetrics interface {
	// StorageWrite is called after each durable write. op is one of
	// "hardstate", "append" or "snapshot"; err is the error the storage
	// backend returned, so failures can be counted as well as timed.
	StorageWrite(id NodeID, op string, d time.Duration, err error)
}

// ProposalMetrics is an optional interface that a Config.Metrics implementation
// may also satisfy. When it does, the node reports how long each proposal took
// and whether it succeeded.
//
// This is the number an operator actually watches: how long a write takes from
// being submitted to being applied, covering the queue at the event loop, the
// durable append, the replication round-trip and the state machine. None of
// that is visible from the commit index alone, and the parts of it that get
// slow -- a stalling disk, one lagging follower -- are exactly the ones worth
// alerting on.
//
// It is a separate interface so that adding it does not break existing Metrics
// implementations. Implementations must not block; they are called from the
// event-loop goroutine.
type ProposalMetrics interface {
	// ProposalCompleted is called once per proposal, with the time from
	// submission to the proposal being resolved. ok is false when the proposal
	// failed, including when it was rejected because this node is not the
	// leader.
	ProposalCompleted(id NodeID, latency time.Duration, ok bool)
}

// ApplyMetrics is an optional interface that a Config.Metrics implementation
// may also satisfy. When it does, the node reports how much of its time the
// apply loop spends applying rather than waiting for work.
//
// It is the number that says where a slow write is slow. Proposal latency
// covers consensus and the state machine together, so a rise in it does not
// say which of the two to fix. A saturation close to 1 says the apply loop
// never gets to wait: the state machine is the constraint, and faster
// consensus buys nothing. A low saturation alongside slow proposals says the
// opposite, and points at the disk or at replication instead.
//
// It is a separate interface so that adding it does not break existing Metrics
// implementations. Implementations must not block; they are called from the
// apply goroutine, which is the goroutine the measurement is about.
type ApplyMetrics interface {
	// ApplySaturation is called with the fraction of the last window, in the
	// range [0,1], that the apply goroutine spent working rather than waiting
	// for committed entries. It is called a few times per window, each time
	// with the ratio over the whole of the recent window rather than since the
	// process started, so that a node that was busy an hour ago does not look
	// busy now.
	ApplySaturation(id NodeID, saturation float64)
}
