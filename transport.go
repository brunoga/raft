package raft

import "context"

// RequestVoteRequest is sent by a Candidate to gather votes.
type RequestVoteRequest struct {
	// GroupID selects the Raft group on a host that runs more than one.
	GroupID uint64
	// Term is the term the candidate is standing in.
	Term Term
	// CandidateID is the node asking for the vote.
	CandidateID NodeID
	// LastLogIndex and LastLogTerm describe the end of the candidate's log.
	// A voter grants its vote only to a candidate whose log is at least as
	// up to date as its own, which is what keeps a committed entry from being
	// lost to an election.
	LastLogIndex Index
	LastLogTerm  Term
	// PreVote indicates this is a pre-vote (does not increment term).
	PreVote bool
}

// RequestVoteResponse is the reply to a RequestVote RPC.
type RequestVoteResponse struct {
	// Term is the responder's current term. A candidate that sees a higher
	// one abandons its election.
	Term Term
	// VoteGranted reports whether the vote was given.
	VoteGranted bool
}

// AppendEntriesRequest is sent by the Leader to replicate log entries and as a
// heartbeat (Entries == nil).
type AppendEntriesRequest struct {
	// GroupID selects the Raft group on a host that runs more than one.
	GroupID uint64
	// Term is the leader's term.
	Term Term
	// LeaderID is the node sending the entries, so a follower can redirect
	// clients to it.
	LeaderID NodeID
	// PrevLogIndex and PrevLogTerm identify the entry immediately before
	// Entries. A follower accepts the request only if it holds that exact
	// entry, which is the single check that makes the whole prefix agree.
	PrevLogIndex Index
	PrevLogTerm  Term
	// Entries are the entries to append, empty for a heartbeat.
	Entries []LogEntry
	// LeaderCommit is how far the leader has committed. A follower takes the
	// lower of this and the last index this request vouches for.
	LeaderCommit Index
	// ReadBarrier, when non-zero, identifies a read-index confirmation round.
	// The leader sets this when broadcasting a barrier heartbeat to confirm its
	// leadership for linearizable reads. Followers echo it back in the response
	// via the appendResult so the leader can count quorum acks.
	ReadBarrier uint64
}

// AppendEntriesResponse is the reply to an AppendEntries RPC.
type AppendEntriesResponse struct {
	// Term is the responder's current term. A leader that sees a higher one
	// steps down.
	Term Term
	// Success reports that the entries are in the responder's log and on its
	// disk. A leader counts it towards the quorum that commits them.
	Success bool
	// ConflictIndex and ConflictTerm are set on failure to help the leader
	// quickly back-track nextIndex (the "fast backup" optimisation).
	ConflictIndex Index
	ConflictTerm  Term
}

// InstallSnapshotRequest is sent by the Leader to bring a lagging follower
// up to date via a snapshot transfer.
type InstallSnapshotRequest struct {
	// GroupID selects the Raft group on a host that runs more than one.
	GroupID uint64
	// Term is the leader's term.
	Term Term
	// LeaderID is the node sending the snapshot.
	LeaderID NodeID
	// LastIncludedIndex and LastIncludedTerm describe the log position the
	// snapshot replaces.
	LastIncludedIndex Index
	LastIncludedTerm  Term
	// Offset is where Data belongs in the snapshot byte stream. Chunks must
	// arrive in order; a receiver refuses one that does not continue where the
	// last left off.
	Offset int64
	// Data is this chunk of the snapshot.
	Data []byte
	// Done marks the final chunk. Its response is what the leader records as
	// the receiver's progress, so it is not sent until the snapshot is on the
	// receiver's disk.
	Done bool
}

// InstallSnapshotResponse is the reply to an InstallSnapshot RPC.
type InstallSnapshotResponse struct {
	// Term is the responder's current term.
	Term Term
}

// TimeoutNowRequest asks the target to start an election immediately
// (used for leadership transfer).
type TimeoutNowRequest struct {
	// GroupID selects the Raft group on a host that runs more than one.
	GroupID uint64
	// Term is the term of the leader giving up leadership.
	Term Term
	// LeaderID is that leader.
	LeaderID NodeID
}

// TimeoutNowResponse is the reply to a TimeoutNow RPC.
type TimeoutNowResponse struct {
	// Term is the term the recipient is standing in, which tells the old
	// leader the transfer was taken up.
	Term Term
}

// ReadIndexRequest is sent by a Follower to the Leader to get a linearizable
// read index.
type ReadIndexRequest struct {
	// GroupID selects the Raft group on a host that runs more than one.
	GroupID uint64
	// Term is the asking node's term, and carries only the term it has on
	// disk: this request is built outside the event loop, where there is no
	// pending write to wait for, and the leader steps down when it sees a term
	// above its own.
	Term Term
}

// ReadIndexResponse is the reply to a ReadIndex RPC.
type ReadIndexResponse struct {
	// Term is the leader's term.
	Term Term
	// Index is the leader's commit index at the moment it confirmed it was
	// still the leader. A follower that waits for its own state machine to
	// reach this index can then serve a linearizable read locally.
	Index Index
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

	// Unregister removes a handler from the transport. Use this when a node
	// is stopped to prevent memory leaks and stop receiving RPCs.
	Unregister(id NodeID)

	// Close shuts down the transport and releases all resources.
	Close() error
}

// MessageSizeLimiter is an optional interface a Transport may implement to
// report the largest message it can carry, in bytes.
//
// The node uses it to refuse a proposal that could never be replicated. A
// transport that cannot answer usefully should not implement this; the limit
// can be stated directly with Config.MaxProposalBytes instead.
type MessageSizeLimiter interface {
	// MaxMessageBytes returns the maximum size of a single RPC payload. A
	// value of zero or less means the transport imposes no limit of its own.
	MaxMessageBytes() int
}

// Handler is the server-side RPC dispatcher. The Raft node implements this
// interface and registers itself with the Transport.
type Handler interface {
	// HandleRequestVote answers a candidate asking for this node's vote.
	HandleRequestVote(ctx context.Context, req *RequestVoteRequest) (*RequestVoteResponse, error)

	// HandleAppendEntries answers a leader replicating entries, or sending a
	// heartbeat when the request carries none.
	HandleAppendEntries(ctx context.Context, req *AppendEntriesRequest) (*AppendEntriesResponse, error)

	// HandleInstallSnapshot answers a leader sending one chunk of a snapshot.
	HandleInstallSnapshot(ctx context.Context, req *InstallSnapshotRequest) (*InstallSnapshotResponse, error)

	// HandleTimeoutNow answers a leader handing leadership to this node.
	HandleTimeoutNow(ctx context.Context, req *TimeoutNowRequest) (*TimeoutNowResponse, error)

	// HandleReadIndex answers a follower asking this node, as leader, for an
	// index it can read at.
	HandleReadIndex(ctx context.Context, req *ReadIndexRequest) (*ReadIndexResponse, error)
}
