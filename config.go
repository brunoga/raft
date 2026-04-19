package raft

import (
	"errors"
	"log/slog"
	"time"
)

// PeerConfig describes the role of a single peer in the Raft cluster.
type PeerConfig struct {
	// ID is the unique identity of the peer node.
	ID NodeID

	// Voter indicates whether this peer participates in elections and counts
	// toward the commit quorum. Witnesses (non-voting members) replicate the
	// log and receive snapshots but do not vote or contribute to quorums.
	Voter bool
}

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
	// Non-voters (witnesses) replicate the log and can be promoted to voters
	// via configuration changes, but they do not vote in elections or count
	// toward quorums.
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
	// Peers is mutated in-place by the event loop as AddServer/RemoveServer
	// entries are applied; callers must not touch it after Start is called.
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
	// with the smallest seqNum (least recently active client) is evicted.
	//
	// In long-running clusters with many ephemeral client IDs the table grows
	// without bound if this is zero, consuming memory indefinitely.
	// DefaultConfig sets this to 100_000, which comfortably covers typical
	// client populations while bounding the per-node overhead to a few tens
	// of MiB.
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
	// Chunking prevents large snapshots from exceeding gRPC's
	// MaxCallRecvMsgSize (default 4 MiB) and bounds the peak heap allocation
	// per RPC to roughly SnapshotChunkSize bytes on both sender and receiver.
	//
	// Set to 0 to send the entire snapshot in a single RPC (the original
	// behaviour, suitable only when snapshots are known to be small).
	//
	// Default: 4 MiB.
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
		MaxLogEntriesPerRPC: 64,
		SnapshotThreshold:   10_000,
		MaxInflightRPCs:     4,
		CheckQuorum:         true,
		MaxClientTableSize:  100_000,
		SnapshotChunkSize:   4 * 1024 * 1024, // 4 MiB
		TickInterval:        10 * time.Millisecond,
	}
}

// Validate returns an error if the configuration is invalid or inconsistent.
// It checks:
//   - ID, Storage, StateMachine, Transport are non-nil/non-empty
//   - ElectionTimeoutMin ≤ ElectionTimeoutMax
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
	if c.ElectionTimeoutMin > c.ElectionTimeoutMax {
		return errors.New("raft: ElectionTimeoutMin must be <= ElectionTimeoutMax")
	}
	if c.HeartbeatInterval <= 0 {
		return errors.New("raft: HeartbeatInterval must be positive")
	}
	if c.ElectionTimeoutMin < 2*c.HeartbeatInterval {
		return errors.New("raft: ElectionTimeoutMin must be at least 2× HeartbeatInterval")
	}
	if c.MaxLogEntriesPerRPC <= 0 {
		return errors.New("raft: MaxLogEntriesPerRPC must be positive")
	}
	if c.MaxInflightRPCs <= 0 {
		return errors.New("raft: MaxInflightRPCs must be positive")
	}
	// When TickInterval drives the clock, the timing fields must resolve to at
	// least one tick each. If TickInterval is larger than HeartbeatInterval,
	// heartbeatTicks rounds to 0 (clamped to 1), so the actual heartbeat
	// period would be TickInterval — silently far longer than configured.
	if c.TickInterval > 0 && c.TickInterval > c.HeartbeatInterval {
		return errors.New("raft: TickInterval must be ≤ HeartbeatInterval")
	}
	return nil
}
