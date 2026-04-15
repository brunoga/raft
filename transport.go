package raft

import "context"

// RequestVoteRequest is sent by a Candidate to gather votes.
type RequestVoteRequest struct {
	GroupID      uint64
	Term         Term
	CandidateID  NodeID
	LastLogIndex Index
	LastLogTerm  Term
	// PreVote indicates this is a pre-vote (does not increment term).
	PreVote bool
}

// RequestVoteResponse is the reply to a RequestVote RPC.
type RequestVoteResponse struct {
	Term        Term
	VoteGranted bool
}

// AppendEntriesRequest is sent by the Leader to replicate log entries and as a
// heartbeat (Entries == nil).
type AppendEntriesRequest struct {
	GroupID      uint64
	Term         Term
	LeaderID     NodeID
	PrevLogIndex Index
	PrevLogTerm  Term
	Entries      []LogEntry
	LeaderCommit Index
	// ReadBarrier, when non-zero, identifies a read-index confirmation round.
	// The leader sets this when broadcasting a barrier heartbeat to confirm its
	// leadership for linearizable reads. Followers echo it back in the response
	// via the appendResult so the leader can count quorum acks.
	ReadBarrier uint64
}

// AppendEntriesResponse is the reply to an AppendEntries RPC.
type AppendEntriesResponse struct {
	Term    Term
	Success bool
	// ConflictIndex and ConflictTerm are set on failure to help the leader
	// quickly back-track nextIndex (the "fast backup" optimisation).
	ConflictIndex Index
	ConflictTerm  Term
}

// InstallSnapshotRequest is sent by the Leader to bring a lagging follower
// up to date via a snapshot transfer.
type InstallSnapshotRequest struct {
	GroupID           uint64
	Term              Term
	LeaderID          NodeID
	LastIncludedIndex Index
	LastIncludedTerm  Term
	Offset            int64
	Data              []byte
	Done              bool
}

// InstallSnapshotResponse is the reply to an InstallSnapshot RPC.
type InstallSnapshotResponse struct {
	Term Term
}

// TimeoutNowRequest asks the target to start an election immediately
// (used for leadership transfer).
type TimeoutNowRequest struct {
	GroupID  uint64
	Term     Term
	LeaderID NodeID
}

// TimeoutNowResponse is the reply to a TimeoutNow RPC.
type TimeoutNowResponse struct {
	Term Term
}

// ReadIndexRequest is sent by a Follower to the Leader to get a linearizable
// read index.
type ReadIndexRequest struct {
	GroupID uint64
	Term    Term
}

// ReadIndexResponse is the reply to a ReadIndex RPC.
type ReadIndexResponse struct {
	Term  Term
	Index Index // the commitIndex as of the ReadIndex call on the leader
}

// Transport abstracts the network layer. All methods are non-blocking from
// the caller's perspective; they respect the supplied context for timeouts
// and cancellation.
type Transport interface {
	// RequestVote sends a RequestVote Request to the given peer.
	RequestVote(ctx context.Context, to NodeID, req *RequestVoteRequest) (*RequestVoteResponse, error)

	// AppendEntries sends an AppendEntries RPC to the given peer.
	AppendEntries(ctx context.Context, to NodeID, req *AppendEntriesRequest) (*AppendEntriesResponse, error)

	// InstallSnapshot sends an InstallSnapshot RPC to the given peer.
	InstallSnapshot(ctx context.Context, to NodeID, req *InstallSnapshotRequest) (*InstallSnapshotResponse, error)

	// TimeoutNow sends a TimeoutNow RPC to the given peer.
	TimeoutNow(ctx context.Context, to NodeID, req *TimeoutNowRequest) (*TimeoutNowResponse, error)

	// ReadIndex sends a ReadIndex RPC to the given peer.
	ReadIndex(ctx context.Context, to NodeID, req *ReadIndexRequest) (*ReadIndexResponse, error)

	// Register makes a handler available to receive inbound RPCs for the
	// node identified by id. Multiple nodes may share one Transport in tests.
	Register(id NodeID, handler Handler)

	// Close shuts down the transport and releases all resources.
	Close() error
}

// Handler is the server-side RPC dispatcher. The Raft node implements this
// interface and registers itself with the Transport.
type Handler interface {
	HandleRequestVote(ctx context.Context, req *RequestVoteRequest) (*RequestVoteResponse, error)
	HandleAppendEntries(ctx context.Context, req *AppendEntriesRequest) (*AppendEntriesResponse, error)
	HandleInstallSnapshot(ctx context.Context, req *InstallSnapshotRequest) (*InstallSnapshotResponse, error)
	HandleTimeoutNow(ctx context.Context, req *TimeoutNowRequest) (*TimeoutNowResponse, error)
	HandleReadIndex(ctx context.Context, req *ReadIndexRequest) (*ReadIndexResponse, error)
}
