package raft

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// PeerConfig describes the role of a single peer in the Raft cluster.
type PeerConfig struct {
	// ID is the unique identity of the peer node.
	ID NodeID

	// Voter indicates whether this peer participates in elections and counts
	// toward the commit quorum. A non-voter, usually called a learner,
	// replicates the log and receives snapshots but does not vote or
	// contribute to any quorum, so adding one never weakens the cluster while
	// it catches up. Promote it with Node.PromoteMember once it has.
	//
	// This is not a witness in the sense of the Raft dissertation §11.7.2. A
	// witness there votes but does not store the full log; a learner here
	// stores the full log but does not vote. They are opposites, and witnesses
	// are not implemented.
	Voter bool
}

// ProposalOverflowPolicy says what a node does with a proposal that arrives
// while its proposal queue is full. See Config.ProposalOverflow.
type ProposalOverflowPolicy uint8

const (
	// ProposalOverflowWait blocks Propose and ProposeOnce until there is room,
	// the caller's context is done, or the node stops.
	ProposalOverflowWait ProposalOverflowPolicy = iota
	// ProposalOverflowReject makes Propose and ProposeOnce return
	// ErrProposalQueueFull at once instead of waiting.
	ProposalOverflowReject
)

// String returns the policy's name.
func (p ProposalOverflowPolicy) String() string {
	switch p {
	case ProposalOverflowWait:
		return "Wait"
	case ProposalOverflowReject:
		return "Reject"
	default:
		return "Unknown"
	}
}

// DefaultProposalQueueSize is the proposal queue capacity used when
// Config.ProposalQueueSize is zero.
const DefaultProposalQueueSize = 1024

// Config holds all tunables for a Raft node. Start with DefaultConfig() and
// override only what you need.
//
// Required fields (have no sensible default): ID, Storage, StateMachine, Transport.
// Validate returns an error if any required field is missing or any constraint
// is violated.
type Config struct {
	// ID is this node's unique identity within the cluster. Required; must be
	// non-empty and must not change for the lifetime of the node. It is used to
	// identify the node in RPCs, log messages, and the client dedup table.
	// Every node in a cluster must have a distinct ID.
	ID NodeID

	// Voter indicates whether this node is a voting member of the cluster.
	// A non-voter, usually called a learner, replicates the log and can be
	// promoted to a voter through a configuration change, but does not vote in
	// elections or count toward any quorum. See PeerConfig.Voter for why this
	// is not what Raft calls a witness.
	//
	// Note that the zero value is false, so a Config assembled by hand rather
	// than from DefaultConfig describes a non-voter. Validate refuses a
	// configuration in which neither this node nor any peer votes, since no
	// leader could ever be elected, but it cannot tell a deliberate learner
	// from a forgotten field in a cluster that has voters elsewhere.
	//
	// Default: true, via DefaultConfig.
	Voter bool

	// GroupID identifies the Raft group this node belongs to. It is stamped on
	// every outbound RPC so that a shared transport (see Manager) can route
	// incoming messages to the correct Node. A value of 0 is valid and is the
	// default for single-group deployments where the transport serves exactly
	// one Node.
	GroupID uint64

	// Peers is the initial set of peer node configurations, not including this
	// node's own configuration. For a fresh three-node cluster {A, B, C},
	// node A's Config should set Peers to []PeerConfig{{ID: "B", Voter: true},
	// {ID: "C", Voter: true}}.
	//
	// When the cluster is bootstrapped from a snapshot the membership list is
	// restored from the snapshot; setting Peers to the last known membership
	// acts as a fallback for the window between snapshot restore and applying
	// subsequent config-change log entries.
	//
	// New copies this slice, so the node never writes to the caller's array
	// and the caller is free to keep, read or reuse it. The node's own copy is
	// rewritten by the event loop as configuration changes commit, so this
	// field is the membership the node started from and not the membership it
	// has. Read the current one with Node.Members.
	Peers []PeerConfig

	// ElectionTimeoutMin is the lower bound of the randomised election timeout.
	// A follower that receives no valid heartbeat for this long starts an
	// election. Must be at least 2 × HeartbeatInterval (enforced by Validate).
	//
	// This value also determines the leader read-lease duration: a read lease
	// granted at time T is valid until T + ElectionTimeoutMin.
	//
	// Typical production value: 150 ms. Default: 150 ms.
	ElectionTimeoutMin time.Duration

	// LeaseSafetyMargin shortens the read lease ReadIndexLease relies on, and
	// sets how much the wall clock and the monotonic clock may disagree
	// before the lease is treated as expired.
	//
	// A lease read is served without contacting any follower, on the strength
	// of an argument about time: no follower can have elected another leader
	// yet, because none has gone without a heartbeat for ElectionTimeoutMin.
	// That argument is only as good as the clocks. Go measures elapsed time on
	// the monotonic clock, which no NTP correction can move, but two things
	// can still break it. Clock rates differ between machines, so a
	// follower's ElectionTimeoutMin may be shorter than this node's; the
	// margin is taken off the lease to cover that. And a machine that is
	// suspended -- a virtual machine paused or live-migrated -- has a
	// monotonic clock that does not advance while the wall clock does, so
	// on resume it believes far less time has passed than really has. A
	// leader that resumes from a suspend longer than an election timeout
	// would serve stale reads from a lease it thinks it still holds. When
	// this is set, a lease is also checked against the wall clock, and one
	// under which the two clocks have diverged by more than the margin is
	// treated as expired.
	//
	// Zero keeps the lease at exactly ElectionTimeoutMin and skips the
	// divergence check, which is the behaviour before this field existed.
	// Must be less than ElectionTimeoutMin. A lease that has expired is not
	// an error the caller has to handle beyond falling back to ReadIndex,
	// which ReadIndexLease documents.
	//
	// Default: 15 ms, via DefaultConfig (a tenth of the default
	// ElectionTimeoutMin).
	LeaseSafetyMargin time.Duration

	// ElectionTimeoutMax is the upper bound of the randomised election timeout.
	// The actual timeout per election attempt is sampled uniformly from
	// [ElectionTimeoutMin, ElectionTimeoutMax). A wider spread reduces the
	// probability of split votes when multiple followers time out simultaneously.
	// Must be strictly greater than ElectionTimeoutMin.
	//
	// Typical production value: 300 ms. Default: 300 ms.
	ElectionTimeoutMax time.Duration

	// HeartbeatInterval is how often the leader sends heartbeat AppendEntries
	// RPCs to followers. Must be significantly smaller than ElectionTimeoutMin
	// so that followers never mistake a live leader for one that has crashed.
	// The rule of thumb is HeartbeatInterval ≤ ElectionTimeoutMin / 5.
	//
	// Typical production value: 50 ms. Default: 50 ms.
	HeartbeatInterval time.Duration

	// MaxLogEntriesPerRPC caps the number of log entries sent in a single
	// AppendEntries RPC. Smaller values bound peak message size and per-RPC
	// memory allocation; larger values reduce round-trips when a follower is
	// catching up. Values in the range 32–256 are typical.
	//
	// Default: 64.
	MaxLogEntriesPerRPC int

	// MaxBytesPerRPC caps the total size of the entry payloads in a single
	// AppendEntries RPC. It complements MaxLogEntriesPerRPC, which caps their
	// number: a count alone says nothing about the size of the message, and
	// MaxLogEntriesPerRPC entries of a megabyte each is a message no transport
	// will carry.
	//
	// That matters because there is no smaller batch to fall back on. A leader
	// whose message is rejected for being too large re-sends the same batch,
	// and the follower behind it never catches up again.
	//
	// A single entry larger than this budget is still sent, on its own:
	// refusing to send it would stall replication permanently, and the
	// transport may well accept it.
	//
	// Set to 0 for no byte limit (the count limit still applies).
	//
	// Default: 1 MiB.
	MaxBytesPerRPC uint64

	// MaxProposalBytes is the largest command Propose and ProposeOnce will
	// accept, in bytes. A larger one is refused with ErrProposalTooLarge.
	//
	// This exists because a command that cannot be replicated is far worse
	// than one that is refused. An oversized command is appended to the
	// leader's log, rejected by the transport on every send, and retried for
	// ever: the entry never commits, every later proposal queues behind it,
	// and the group stops making progress with nothing having reported an
	// error.
	//
	// When zero, the limit is taken from the transport if it implements
	// MessageSizeLimiter, leaving room for the entry's own framing. When the
	// transport cannot report one either, no limit is applied — set this
	// explicitly if your transport has a limit it cannot advertise.
	//
	// Default: 0 (ask the transport).
	MaxProposalBytes int

	// MaxUnstableLogBytes caps the total size of log entries a leader will
	// hold in memory waiting to be written to storage. Beyond it, Propose and
	// ProposeOnce are refused with ErrWriteBacklogFull until the writes catch
	// up.
	//
	// This is the backpressure that replaces waiting for the disk. Appends do
	// not block the event loop, so without a limit a leader whose storage has
	// stalled would keep accepting proposals it cannot write and grow its
	// backlog until the process runs out of memory -- trading a node that is
	// slow for one that dies, and taking the cluster's leader with it. Refusing
	// the proposal hands the decision to the caller, which can retry, shed the
	// request, or fail it, and leaves the node healthy enough to be given away
	// to a peer with a working disk.
	//
	// A follower is not throttled this way and does not need to be: its
	// backlog is bounded by what its leader will send before it acknowledges,
	// which MaxInflightRPCs and MaxBytesPerRPC already limit.
	//
	// Zero is not "no limit" here, unlike MaxBytesPerRPC and
	// MaxClientTableSize: it selects the default, because a node with no bound
	// at all is a node that answers a slow disk by running out of memory. Set
	// a negative value to say no limit and mean it.
	//
	// Default: 64 MiB.
	MaxUnstableLogBytes int

	// ProposalQueueSize is how many proposals may wait for the event loop at
	// once. Propose and ProposeOnce hand their command to the event loop
	// through this queue, and what happens when it is full is decided by
	// ProposalOverflow.
	//
	// It is the other half of admission control. MaxUnstableLogBytes bounds
	// what a leader accepts when its disk is behind; this bounds what waits
	// when the event loop itself is behind, for any reason -- a slow state
	// machine, a long snapshot, heavy replication. The queue is also the batch:
	// the event loop drains it in one go, so a larger queue means fewer, larger
	// storage writes under load, at the cost of a longer wait for the
	// proposals at the back.
	//
	// Zero selects the default. Negative is refused by Validate.
	//
	// Default: 1024.
	ProposalQueueSize int

	// ProposalOverflow says what Propose and ProposeOnce do when the proposal
	// queue is full: wait for space, or refuse with ErrProposalQueueFull.
	//
	// Waiting is the right default for a caller that would only retry anyway,
	// and it is bounded by the caller's context. Refusing is for a caller that
	// would rather shed the request than hold a goroutine on it -- a request
	// handler with its own deadline, or a service that must stay responsive
	// under overload -- and it makes the queue's fullness visible as an error
	// rather than as latency.
	//
	// Default: ProposalOverflowWait.
	ProposalOverflow ProposalOverflowPolicy

	// Zones records which failure domain each node sits in: a rack, an
	// availability zone, a datacentre, whatever fails as one unit. It is used
	// with MinCommitZones and ignored without it.
	//
	// It is deliberately local to this node and never replicated. Placement is
	// a fact about infrastructure rather than about consensus, it changes when
	// machines move rather than when the cluster agrees on something, and
	// keeping it out of the log means it costs nothing on the wire and can be
	// corrected by a restart rather than a configuration change. Every node
	// that might lead should be given the same map; a node that is not leading
	// does not read it.
	//
	// A node absent from the map is in no known zone, and does not count
	// towards the spread MinCommitZones requires. That is the conservative
	// reading: a node nobody placed cannot be evidence that a write survived
	// the loss of a zone.
	//
	// Default: nil.
	Zones map[NodeID]ZoneID

	// MinCommitZones is how many distinct zones an entry must reach before it
	// counts as committed, in addition to reaching a majority.
	//
	// A majority says nothing about where the replicas are. Three replicas in
	// one availability zone are a quorum, and the loss of that zone loses
	// every acknowledged write with it. Requiring two zones means an
	// acknowledged write is on hardware in two failure domains before anyone
	// is told it succeeded.
	//
	// The cost is liveness, and it is the point rather than a side effect: if
	// the second zone is unreachable, nothing commits. A cluster that would
	// rather keep taking writes into one zone should leave this unset.
	//
	// Validate refuses a configuration whose initial voters do not span this
	// many zones, since it could never commit anything.
	//
	// Default: 0, which together with 1 means a plain majority.
	MinCommitZones int

	// CommitQuorum is how many voters an entry must reach to commit, 0
	// meaning a simple majority. The election quorum follows from it, as
	// voters - CommitQuorum + 1 or a majority, whichever is larger, so the
	// two always intersect and the safety argument holds whatever the split;
	// see Node.SetCommitQuorum for what the split trades.
	//
	// It is group state, like membership, and this field is only what a
	// group is created with: the first leader writes it into the log when
	// the group has agreed no policy yet, and every node counts by the
	// agreed value from then on, never by its own Config. Until then every
	// node counts by a majority. Change a running group's policy with
	// Node.SetCommitQuorum; read the one in effect with Node.CommitQuorum.
	//
	// Validate refuses a value larger than the number of voters in Config.
	//
	// Default: 0 (a majority).
	CommitQuorum int

	// SnapshotThreshold is the number of log entries after which the leader
	// automatically requests a snapshot from the state machine:
	//   trigger when  lastApplied − lastSnapshotIndex >= SnapshotThreshold
	//
	// Setting SnapshotThreshold to 0 disables automatic snapshots; the caller
	// is then responsible for triggering compaction through their own mechanism.
	// This is useful when the state machine is trivially small, manages its own
	// compaction, or is under test.
	//
	// Default: 10 000.
	SnapshotThreshold uint64

	// TrailingLogs is the number of log entries retained behind the snapshot
	// point when the log is compacted.
	//
	// Compaction that keeps nothing behind the snapshot point makes a full
	// state transfer the only way to catch up a follower that was even one
	// entry behind at that instant, and with automatic snapshots that instant
	// comes round again and again. Retaining a tail lets those followers catch
	// up from the log instead, which on a large state machine is the difference
	// between shipping a few entries and shipping the whole thing.
	//
	// Size it to cover how far a healthy follower can fall behind: a brief
	// pause, a garbage collection, a slow disk. The cost is disk space for that
	// many entries.
	//
	// A value at or above SnapshotThreshold would leave compaction with nothing
	// to reclaim, so it is capped at SnapshotThreshold-1 in use; New logs a
	// warning when that cap applies. Lowering SnapshotThreshold without
	// lowering this therefore still works, it just retains less.
	//
	// Set to 0 to retain nothing.
	//
	// Default: 1024.
	TrailingLogs uint64

	// SnapshotSemaphore is an optional semaphore used to limit the number of
	// concurrent snapshots across multiple Raft nodes on a single physical
	// machine. If nil, snapshots are not throttled.
	//
	// Multi-raft deployments should share a single semaphore across all nodes
	// to prevent "snapshot storms" that could starve the node's CPU and I/O.
	SnapshotSemaphore chan struct{}

	// MaxInflightRPCs is the maximum number of concurrent unacknowledged
	// AppendEntries RPCs per follower (the per-peer send window). Higher values
	// increase throughput under high latency by keeping the network pipe full,
	// at the cost of higher peak memory usage — each in-flight RPC holds a
	// reference to its log entry slice. Must be at least 1.
	//
	// Default: 4.
	MaxInflightRPCs int

	// CheckQuorum enables the leader-stepdown-on-quorum-loss safety check. When
	// true, the leader tracks successful AppendEntries responses from peers and
	// steps down as a follower if it does not hear from a majority within one
	// election timeout. This prevents a partitioned leader from accepting
	// proposals (which would never commit) and rejecting reads indefinitely.
	//
	// This should be true in all production deployments. Setting it to false is
	// only useful for contrived test scenarios where heartbeat responses are
	// never delivered (e.g. a simulation that delivers proposals but drops all
	// AppendEntries replies). DefaultConfig sets this to true.
	//
	// Default: true.
	CheckQuorum bool

	// MaxClientTableSize caps the number of entries in the client dedup table
	// used by ProposeOnce. Each entry records the latest (seqNum, result) pair
	// for one client NodeID. When the table would exceed this size, the entry
	// written longest ago is evicted. Eviction order depends only on the order
	// entries were written, which is the order of the log, so every replica
	// evicts the same entry at the same point.
	//
	// The bound is replicated, so that every replica keeps the same table. A
	// leader whose group has not agreed one yet writes this value into the
	// log ahead of the first ProposeOnce entry, and from then on every node
	// uses the agreed value whatever its own Config says, with a warning when
	// the two differ. Snapshots carry it. So this is the value a group is
	// created with; Node.MaxClientTableSize reports the value in effect, and
	// Node.SetMaxClientTableSize changes it for the whole group.
	//
	// Eviction is a real limit on the exactly-once guarantee: a client that
	// retries a request after its entry has been evicted has that request
	// executed a second time. Size the table so that it comfortably outlives
	// the retry window of the slowest client.
	//
	// Whether it does is measurable rather than a matter of hope.
	// Node.ClientTableSize is the current occupancy, and while it stays below
	// this bound nothing is ever evicted and exactly-once holds absolutely;
	// that is the value to alert on. Once the bound is reached each eviction
	// is reported through ClientTableMetrics, as EventClientForgotten, and as
	// a warning on Logger -- but those say the guarantee has already lapsed
	// for the named client, so they are a confirmation rather than a warning.
	//
	// The bound is on entry count, not bytes; each entry also retains the
	// result the state machine returned, so a state machine with large results
	// needs a smaller table. In long-running clusters with many ephemeral
	// client IDs the table grows without bound if this is zero, consuming
	// memory indefinitely. DefaultConfig sets this to 100_000.
	//
	// Set to 0 to disable eviction (not recommended in production).
	//
	// Default: 100_000.
	MaxClientTableSize int

	// SnapshotChunkSize is the maximum number of bytes sent in a single
	// InstallSnapshot RPC payload. When a snapshot exceeds this size, the
	// leader splits it into sequential chunks and sends them in order; the
	// follower reassembles the chunks before applying.
	//
	// Chunking bounds the peak heap allocation per RPC to roughly
	// SnapshotChunkSize bytes on both sender and receiver, and keeps each
	// message inside whatever limit the transport enforces.
	//
	// Leave room for framing when choosing this: a chunk sized at exactly the
	// transport's message limit does not fit, because the request carries its
	// other fields too. The default is deliberately well below gRPC's own
	// default limit of 4 MiB for that reason.
	//
	// Set to 0 to send the entire snapshot in a single RPC, which is suitable
	// only when snapshots are known to be small.
	//
	// Default: 1 MiB.
	SnapshotChunkSize int

	// RPCTimeout is the per-RPC deadline applied to every outbound Raft RPC
	// (RequestVote, AppendEntries, TimeoutNow). InstallSnapshot RPCs use
	// 4 × RPCTimeout because they carry arbitrarily large state-machine
	// payloads. A zero value falls back to ElectionTimeoutMin.
	//
	// Set RPCTimeout low enough that a failed RPC is detected before the next
	// heartbeat interval, but high enough to accommodate typical network RTT
	// plus serialisation overhead.
	//
	// Default: 0 (uses ElectionTimeoutMin).
	RPCTimeout time.Duration

	// TickInterval is the wall-clock period of one logical tick. When positive,
	// Start launches an internal goroutine that calls Tick at this rate,
	// converting wall-clock time into logical ticks for the election and
	// heartbeat timers.
	//
	// Set TickInterval to 0 to drive ticks manually by calling Tick(). This is
	// the recommended approach for deterministic tests, because it gives the
	// test full control over time. All timing-related fields (ElectionTimeoutMin,
	// ElectionTimeoutMax, HeartbeatInterval) are still expressed as durations;
	// they are converted to tick counts internally using TickInterval.
	//
	// Default: 10 ms (via DefaultConfig). A typical production value is 10 ms.
	TickInterval time.Duration

	// Storage is the persistent storage backend for the Raft log, hard state,
	// and snapshots. Required.
	//
	// Implementations must be crash-safe: AppendLogEntries and SaveHardState
	// must fsync to durable media before returning. Two built-in implementations
	// are provided:
	//   - storage/memstore: in-memory, non-durable; suitable for tests only.
	//   - storage/filestore: file-backed with CRC32 checksums, fsyncs, and
	//     automatic segment rotation; suitable for production use.
	Storage Storage

	// StateMachine is the application-level deterministic state machine.
	// Required.
	//
	//   - Apply is called in log-index order for every committed non-config
	//     entry. It must be deterministic: the same entry must produce the same
	//     result on every node.
	//   - Snapshot is called from a background goroutine when the log exceeds
	//     SnapshotThreshold; it must not block the event loop.
	//   - Restore is called when an InstallSnapshot RPC arrives or when the
	//     node restarts with a snapshot on disk. It replaces the entire state
	//     machine state with the snapshot.
	StateMachine StateMachine

	// Transport is the network layer for all outbound and inbound Raft RPCs.
	// Required.
	//
	// The node calls Transport.Register(id, handler) in Start to receive
	// inbound RPCs. Peers must be registered via AddPeer before the node
	// starts sending RPCs to them. Two built-in implementations are provided:
	//   - transport/memtransport: in-process, with controllable partitioning
	//     and drop rates; suitable for tests.
	//   - transport/grpctransport: production gRPC over TCP, with connection
	//     pooling and default keepalive settings.
	Transport Transport

	// Logger is the structured logger for internal Raft events (state
	// transitions, elections, commits, errors). When nil, slog.Default() is
	// used with the node ID added as a permanent "node" attribute.
	//
	// Set a custom logger to route Raft output to a separate sink, adjust
	// verbosity (e.g. slog.LevelWarn to suppress routine traffic), or silence
	// output in unit tests.
	Logger *slog.Logger

	// Metrics is an optional observability hook called from the event-loop
	// goroutine on state transitions, commit advances, and snapshot events.
	// When nil, no metrics are recorded.
	//
	// Implementations must not block; they are called synchronously from the
	// event loop. A reference Prometheus implementation is provided in
	// metrics/prommetrics.
	Metrics Metrics

	// Tracer is an optional hook called for every outbound Raft RPC. When nil,
	// no tracing is performed. Implementations must be safe for concurrent use
	// (one goroutine per in-flight RPC).
	//
	// A structured-logging reference implementation is provided in
	// metrics/rpctracer. An OpenTelemetry adapter can be layered on top by
	// implementing the Tracer interface.
	Tracer Tracer

	// Clock is an optional injectable time source used by the leader lease
	// subsystem (ReadIndexLease). When nil, time.Now is used.
	//
	// Inject a fake clock in tests to make lease expiry fully deterministic
	// without relying on wall-clock timing.
	Clock Clock

	// OnFatal is an optional callback invoked once, from its own goroutine, when
	// this node stops because a durable write failed. The error it receives is
	// the same one FatalError reports, and it matches ErrNodeFailed.
	//
	// A node in this state has already stopped; the callback exists so that a
	// process running many groups can raise an alarm, or tear down and rebuild
	// the affected group, rather than discovering the failure by noticing that
	// one group has gone quiet. Do not call Stop on the node from here: it has
	// stopped itself.
	//
	// Default: nil (the failure is logged at error level and nothing else).
	OnFatal func(error)

	// OnRemoved is an optional callback invoked once, from its own goroutine,
	// when a committed configuration change removes this node from the
	// cluster. It fires whether the node removed itself (a leader that left
	// through ReconfigureCluster) or was removed by another node.
	//
	// A removed node is not stopped: it goes on running as a non-voting
	// follower that nobody replicates to, which is deliberate, so that
	// whatever owns it can decide what to do -- shut it down, wipe its
	// storage, keep it for inspection. This callback is where that decision
	// is made, and it may call Stop on the node. The same transition is
	// reported as EventPeerRemoved with Peer set to this node's own ID, for a
	// consumer that already watches Events.
	//
	// It is not invoked again when a restarted node replays the removal from
	// its own log: it reports the removal committing, not the state of being
	// removed. Read Members after a restart to find out whether this node is
	// still part of the cluster it started in.
	//
	// Default: nil.
	OnRemoved func()

	// PreferredLeader is an optional node ID that should hold leadership
	// whenever possible. When a node that is not the preferred leader wins an
	// election, it will automatically initiate a leadership transfer to the
	// preferred node once its no-op entry is committed (i.e. once it is safe
	// to serve reads and accept proposals).
	//
	// Useful for heterogeneous hardware (pin leadership to the most powerful
	// node) or geographic affinity (keep the leader co-located with clients).
	//
	// If the preferred node is unavailable, the transfer times out and the
	// current leader continues normally; it will retry on the next election.
	// Setting PreferredLeader to this node's own ID or leaving it empty both
	// mean "no preference".
	//
	// Default: "" (no preference).
	PreferredLeader NodeID
}

// DefaultConfig returns a Config populated with production-ready defaults.
// The following fields still require explicit values before calling New:
//
//	cfg.ID           = "node-1"
//	cfg.Peers        = []raft.PeerConfig{{ID: "node-2", Voter: true}}
//	cfg.Storage      = ...   // e.g. filestore.Open(dir)
//	cfg.StateMachine = ...
//	cfg.Transport    = ...   // e.g. grpctransport.Listen(":50051")
func DefaultConfig() Config {
	return Config{
		Voter:               true,
		ElectionTimeoutMin:  150 * time.Millisecond,
		ElectionTimeoutMax:  300 * time.Millisecond,
		HeartbeatInterval:   50 * time.Millisecond,
		LeaseSafetyMargin:   15 * time.Millisecond,
		MaxLogEntriesPerRPC: 64,
		MaxBytesPerRPC:      1 << 20, // 1 MiB
		SnapshotThreshold:   10_000,
		TrailingLogs:        1024,
		MaxInflightRPCs:     4,
		CheckQuorum:         true,
		MaxClientTableSize:  100_000,
		MaxUnstableLogBytes: 64 << 20, // 64 MiB
		ProposalQueueSize:   DefaultProposalQueueSize,
		SnapshotChunkSize:   1 << 20, // 1 MiB
		TickInterval:        10 * time.Millisecond,
	}
}

// Validate returns an error if the configuration is invalid or inconsistent.
// It checks:
//   - ID, Storage, StateMachine, Transport are non-nil/non-empty
//   - ElectionTimeoutMin < ElectionTimeoutMax
//   - ElectionTimeoutMin ≥ 2 × HeartbeatInterval
//   - MaxLogEntriesPerRPC and MaxInflightRPCs are positive
func (c *Config) Validate() error {
	if c.ID == "" {
		return errors.New("raft: Config.ID must not be empty")
	}
	if c.Storage == nil {
		return errors.New("raft: Config.Storage must not be nil")
	}
	if c.StateMachine == nil {
		return errors.New("raft: Config.StateMachine must not be nil")
	}
	if c.Transport == nil {
		return errors.New("raft: Config.Transport must not be nil")
	}
	if c.ElectionTimeoutMin <= 0 || c.ElectionTimeoutMax <= 0 {
		return errors.New("raft: election timeout values must be positive")
	}
	if c.ElectionTimeoutMin >= c.ElectionTimeoutMax {
		// Equal bounds leave no randomness in the election timeout. Every
		// follower then times out at the same moment, splits the vote, and
		// splits it again on the next attempt: the cluster can stay leaderless
		// for a long time with nothing obviously wrong.
		return errors.New("raft: ElectionTimeoutMax must be greater than ElectionTimeoutMin, " +
			"otherwise elections have no randomised spread and split votes repeat")
	}
	if c.HeartbeatInterval <= 0 {
		return errors.New("raft: HeartbeatInterval must be positive")
	}
	if c.ElectionTimeoutMin < 2*c.HeartbeatInterval {
		return errors.New("raft: ElectionTimeoutMin must be at least 2× HeartbeatInterval")
	}
	if c.LeaseSafetyMargin < 0 {
		return errors.New("raft: LeaseSafetyMargin must not be negative")
	}
	if c.LeaseSafetyMargin >= c.ElectionTimeoutMin {
		return errors.New("raft: LeaseSafetyMargin must be less than ElectionTimeoutMin, " +
			"otherwise no lease read could ever be served")
	}
	if c.MaxLogEntriesPerRPC <= 0 {
		return errors.New("raft: MaxLogEntriesPerRPC must be positive")
	}
	if c.MaxInflightRPCs <= 0 {
		return errors.New("raft: MaxInflightRPCs must be positive")
	}
	if c.MaxClientTableSize < 0 {
		return errors.New("raft: MaxClientTableSize must not be negative (use 0 for unlimited)")
	}
	if c.ProposalQueueSize < 0 {
		return errors.New("raft: ProposalQueueSize must not be negative (use 0 for the default)")
	}
	if c.ProposalOverflow > ProposalOverflowReject {
		return fmt.Errorf("raft: ProposalOverflow %d is not a known policy", c.ProposalOverflow)
	}
	if c.SnapshotChunkSize < 0 {
		return errors.New("raft: SnapshotChunkSize must not be negative (use 0 to send whole snapshots)")
	}
	if c.RPCTimeout < 0 {
		return errors.New("raft: RPCTimeout must not be negative")
	}
	if c.TickInterval < 0 {
		return errors.New("raft: TickInterval must not be negative (use 0 to drive ticks manually)")
	}
	// When TickInterval drives the clock, the timing fields must resolve to at
	// least one tick each. If TickInterval is larger than HeartbeatInterval,
	// heartbeatTicks rounds to 0 (clamped to 1), so the actual heartbeat
	// period would be TickInterval — silently far longer than configured.
	if c.TickInterval > 0 && c.TickInterval > c.HeartbeatInterval {
		return errors.New("raft: TickInterval must be ≤ HeartbeatInterval")
	}
	if c.MinCommitZones < 0 {
		return errors.New("raft: MinCommitZones must not be negative")
	}
	if c.CommitQuorum < 0 {
		return errors.New("raft: CommitQuorum must not be negative (0 means a majority)")
	}
	if c.CommitQuorum > 0 {
		voters := 0
		if c.Voter {
			voters++
		}
		for _, p := range c.Peers {
			if p.Voter {
				voters++
			}
		}
		if c.CommitQuorum > voters {
			return fmt.Errorf("raft: CommitQuorum is %d but Config describes %d voters, "+
				"so nothing could ever commit", c.CommitQuorum, voters)
		}
	}
	if c.MinCommitZones > 1 {
		zones := make(map[ZoneID]struct{})
		if c.Voter {
			if z, ok := c.Zones[c.ID]; ok {
				zones[z] = struct{}{}
			}
		}
		for _, p := range c.Peers {
			if !p.Voter {
				continue
			}
			if z, ok := c.Zones[p.ID]; ok {
				zones[z] = struct{}{}
			}
		}
		if len(zones) < c.MinCommitZones {
			return fmt.Errorf("raft: MinCommitZones is %d but the voters in Config.Peers span "+
				"%d known zones, so nothing could ever commit (a node missing from Config.Zones "+
				"is in no known zone)", c.MinCommitZones, len(zones))
		}
	}

	// A configuration that names peers but gives none of them, nor itself, a
	// vote describes a cluster that can never elect a leader and so can never
	// commit anything. It is easy to arrive at by accident, because the zero
	// value of Voter is false: a peer list written as
	// []PeerConfig{{ID: "n2"}, {ID: "n3"}} is a list of learners. Nothing
	// about the resulting deployment looks broken -- every node starts, every
	// node reports itself a follower, and no election is ever held.
	//
	// A node with no peers at all is left alone. That is how a node waits to
	// be added to a cluster it has not been told about yet, and refusing it
	// would refuse the join.
	if !c.Voter && len(c.Peers) > 0 {
		hasVoter := false
		for _, p := range c.Peers {
			if p.Voter {
				hasVoter = true
				break
			}
		}
		if !hasVoter {
			return errors.New("raft: no voters: Config.Voter is false and no peer in " +
				"Config.Peers is a voter, so no leader can ever be elected " +
				"(Voter defaults to false; DefaultConfig sets it true)")
		}
	}
	return nil
}
