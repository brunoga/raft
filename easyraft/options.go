package easyraft

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/discovery"
	"github.com/brunoga/raft/v2/transport/grpctransport"
	"github.com/prometheus/client_golang/prometheus"
)

// config holds everything the With* options set.
//
// It is deliberately unexported. Every constructor in this package -- New,
// NewStore, NewManager, Manager.AddStore -- takes options and nothing else, so
// an exported struct here would be a promise about a field set that has
// nowhere to be passed in, and one that could not be added to without breaking
// anyone who built it as a literal.
type config struct {
	ID       raft.NodeID
	RaftAddr string
	HTTPAddr string
	// AdvertiseRaftAddr and AdvertiseHTTPAddr are what peers are told to use
	// to reach this node, when that differs from what it binds. Set via
	// [WithAdvertiseRaftAddr] and [WithAdvertiseHTTPAddr]; each defaults to
	// the corresponding bind address.
	AdvertiseRaftAddr string
	AdvertiseHTTPAddr string
	DataDir           string
	Peers             map[raft.NodeID]string
	Logger            *slog.Logger
	SnapCount         uint64

	Discovery         discovery.Discovery
	DiscoveryInterval time.Duration
	TLS               *tls.Config
	PromRegisterer    prometheus.Registerer

	// DiscoveryAsVoter promotes peers found through Discovery straight to
	// voting members. It is false by default: discovered peers join as
	// non-voting learners, because adding a voter changes the quorum size on
	// the strength of a network announcement alone. Set via
	// [WithDiscoveryAsVoter].
	DiscoveryAsVoter bool

	// HTTPAuth authorises every HTTP request before it reaches a handler.
	// Returning a non-nil error rejects the request. Set via [WithHTTPAuth];
	// see [BearerTokenAuth] and [ClientCertAuth] for ready-made policies.
	//
	// When nil, the HTTP API is unauthenticated and anyone who can reach the
	// listener can reshape cluster membership. The store logs a warning once
	// at startup in that case.
	HTTPAuth func(*http.Request) error

	// HTTPCredential is sent as the Authorization header on the cluster-control
	// requests this node makes to its peers (joining at startup, leaving at
	// shutdown). [WithBearerTokenAuth] sets it alongside HTTPAuth so both
	// directions use the same token.
	HTTPCredential string

	// AcknowledgeInsecureHTTP suppresses the startup warning emitted when the
	// HTTP API is served without an HTTPAuth hook. Set via
	// [WithInsecureHTTPAcknowledged] to record that the exposure is understood
	// and mitigated elsewhere (a loopback bind, a service mesh, a firewall).
	AcknowledgeInsecureHTTP bool

	// AcknowledgeInsecureTransport lets the Raft transport run without TLS.
	// Without it, or TLS, NewStore and NewManager refuse to start. Set via
	// [WithInsecureTransportAcknowledged].
	AcknowledgeInsecureTransport bool

	// Witness makes this node a witness: it votes and counts towards every
	// quorum, keeps the shape of the log, and holds none of the data. Set via
	// [WithWitness].
	Witness bool

	// TLSCertFile, TLSKeyFile and TLSCAFile are the PEM files [WithTLSFiles]
	// builds the Raft transport's mutual TLS from. They are read when the
	// store or manager is constructed, not here, so that a missing or
	// malformed file is an error the caller is handed rather than a panic.
	TLSCertFile string
	TLSKeyFile  string
	TLSCAFile   string

	// SharedWAL makes every group on a Manager share one write-ahead log at
	// the Manager's DataDir. Set via [WithSharedWAL]; ignored by a plain
	// Store, which has only one group to write for.
	SharedWAL bool

	// Witnesses names the peers from [WithPeers] that are witnesses, so that
	// this node's view of the bootstrap membership matches theirs. Set via
	// [WithWitnessPeers].
	Witnesses map[raft.NodeID]bool

	// CommitQuorum is how many voters an entry must reach to commit, 0
	// meaning a majority. It is only what a group is created with; see
	// [WithCommitQuorum].
	CommitQuorum int

	// MinCommitZones is how many distinct zones an entry must reach before it
	// counts as committed, and Zones says which zone each node is in. Set via
	// [WithZones].
	Zones          map[raft.NodeID]raft.ZoneID
	MinCommitZones int

	// PreferredLeader is the node that should hold leadership whenever
	// possible. Set via [WithPreferredLeader].
	PreferredLeader raft.NodeID

	// MaxClientTableSize bounds the exactly-once table behind [Session]. Set
	// via [WithMaxClientTableSize].
	MaxClientTableSize int

	// LeaseSafetyMargin shortens the read lease and bounds what it assumes
	// about clocks. Set via [WithLeaseSafetyMargin].
	LeaseSafetyMargin time.Duration

	// KeyLeaseSweepInterval is how often the leader looks for expired key
	// leases. Set via [WithKeyLeaseSweepInterval].
	KeyLeaseSweepInterval time.Duration

	// Transport replaces the gRPC transport this store would otherwise
	// build. Set via [WithTransport].
	Transport raft.Transport

	// MaxProposalBytes caps the size of a single command. Set via
	// [WithMaxProposalBytes].
	MaxProposalBytes int

	// BalanceHosts, BalanceInterval and BalanceOptions configure automatic
	// leader balancing on a [Manager]. Set via [WithLeaderBalancing].
	BalanceHosts    map[raft.HostID]string
	BalanceInterval time.Duration
	BalanceOptions  []raft.BalanceOption

	// ProposalQueueSize and ProposalOverflow govern how many writes may wait
	// for the event loop and what happens to the next one. Set via
	// [WithProposalQueue].
	ProposalQueueSize int
	ProposalOverflow  raft.ProposalOverflowPolicy

	// OnRemoved is called once, from its own goroutine, when a committed
	// configuration change removes this node from the cluster. Set via
	// [WithOnRemoved].
	OnRemoved func()

	// PeerAuthorizer decides whether the node a Raft RPC claims to come from
	// is the node that sent it. Set via [WithPeerAuthorizer]; left nil, a
	// configuration whose TLS requires and verifies client certificates gets
	// grpctransport.MTLSPeerAuthorizer, which is the one that matches the
	// certificate's identity against the claim.
	PeerAuthorizer grpctransport.PeerAuthorizer

	// HTTPTLS, when set, makes the built-in HTTP server serve HTTPS and makes
	// leader redirects use the https scheme. Set via [WithHTTPTLS]. It is
	// independent of TLS, which covers only the gRPC Raft transport.
	HTTPTLS *tls.Config

	// LeaseReads allows linearizable reads to be served from the leader's
	// clock-based read lease instead of a heartbeat round-trip. It is false by
	// default because a lease is only sound under bounded clock drift. Even
	// when enabled, an expired lease falls back to a quorum-confirmed read
	// rather than failing. Set via [WithLeaseReads].
	LeaseReads bool

	// JoinAddrs are HTTP addresses of existing cluster nodes to contact on
	// startup. The joining node POSTs its ID and Raft address to each seed in
	// turn until one succeeds. The seed registers this node with the Raft
	// cluster and returns the current peer list so the joiner can seed its
	// own transport. Set via [WithJoinAddr].
	JoinAddrs []string

	// JoinAsLearner, when true, causes the node to join as a non-voting
	// member (learner/observer). Learners replicate the log but do not
	// participate in elections or commit quorum. Set via [WithJoinAsLearner].
	JoinAsLearner bool

	// LeaveOnStop, when true, causes Stop to call RemoveServer(self) with a
	// 5-second timeout before shutting down, gracefully removing this node
	// from the cluster. Set via [WithLeaveOnStop].
	LeaveOnStop bool

	// HTTPMux, when set, causes the store to register its management routes
	// (join, members, CRUD, etc.) on the provided mux instead of starting its
	// own HTTP server. The caller is responsible for serving the mux.
	// HTTPAddr is still used to advertise this node's URL to the cluster for
	// leader redirects — set it to the address the caller will listen on.
	// Set via [WithHTTPMux].
	HTTPMux *http.ServeMux

	// Raft timing overrides. Zero means use the easyraft defaults
	// (100 ms tick/heartbeat, 1–2 s election timeout).
	TickInterval       time.Duration
	HeartbeatInterval  time.Duration
	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration
}

// Option configures an EasyRaft node, and is the only way to: the struct it
// writes to is not exported, so what can be set is exactly the set of With*
// functions below, and adding to that set is never a breaking change.
type Option func(*config)

// WithID sets the Raft node ID.
func WithID(id raft.NodeID) Option {
	return func(c *config) { c.ID = id }
}

// WithRaftAddr sets the listen address for Raft RPCs (e.g., ":7001").
func WithRaftAddr(addr string) Option {
	return func(c *config) { c.RaftAddr = addr }
}

// WithAdvertiseRaftAddr sets the address peers are told to dial to reach this
// node's Raft port, when that differs from the address it binds.
//
// A node binding 0.0.0.0:7001 to accept connections on every interface cannot
// tell a peer to dial "0.0.0.0:7001", and one binding ":7001" cannot tell it to
// dial ":7001" either: neither names a host. Whatever a peer is given has to be
// reachable from that peer, which only the operator knows -- a hostname, a
// service address, a pod IP.
//
// Defaults to [WithRaftAddr]. That default is right when the bind address
// already names a reachable host, which for a local cluster means
// "127.0.0.1:7001" rather than ":7001".
func WithAdvertiseRaftAddr(addr string) Option {
	return func(c *config) { c.AdvertiseRaftAddr = addr }
}

// WithAdvertiseHTTPAddr sets the address peers are told to redirect clients to
// in order to reach this node's HTTP API, when that differs from the address it
// binds.
//
// This is what a follower puts in a 307 when a write arrives and this node is
// the leader. A node whose advertised address names no host cannot be
// redirected to at all: the follower answers 503 and says so.
//
// Defaults to [WithHTTPAddr].
func WithAdvertiseHTTPAddr(addr string) Option {
	return func(c *config) { c.AdvertiseHTTPAddr = addr }
}

// WithHTTPAddr sets the optional listen address for the HTTP API (e.g., ":8001").
//
// The HTTP API can reshape cluster membership, so the listener must not be
// reachable by untrusted clients. Bind it to a management interface (or
// "127.0.0.1:8001") and pair it with [WithHTTPAuth] or [WithBearerTokenAuth].
// The address is also advertised to the rest of the cluster as this node's URL
// for leader redirects, unless [WithAdvertiseHTTPAddr] overrides it.
func WithHTTPAddr(addr string) Option {
	return func(c *config) { c.HTTPAddr = addr }
}

// WithHTTPMux registers the store's management routes on mux instead of
// starting a dedicated HTTP server. Use this when your application already
// runs its own HTTP server and you want easyraft routes (/join, /members, /{collection}/...,
// etc.) to share the same listener. The caller must start a server using mux.
//
// [WithHTTPAddr] should still be set to the address the caller will listen on
// so easyraft can advertise the correct URL to peers for leader redirects.
//
// The authorization hook from [WithHTTPAuth] still applies to the routes
// easyraft registers; it does not affect the caller's own routes.
func WithHTTPMux(mux *http.ServeMux) Option {
	return func(c *config) { c.HTTPMux = mux }
}

// WithHTTPAuth installs an authorization hook that runs before every easyraft
// HTTP handler, including the cluster-mutating ones (/join, member removal,
// leadership transfer) and every write. Returning nil admits the request;
// returning an error rejects it.
//
// Wrap [ErrUnauthorized] to answer 401 and [ErrForbidden] to answer 403; any
// other error is reported as 403 without echoing its text to the client.
//
//	er, _ := easyraft.New[Counter](
//	    easyraft.WithHTTPAuth(easyraft.BearerTokenAuth(os.Getenv("RAFT_TOKEN"))),
//	)
//
// Without this option the HTTP API is open to anyone who can reach the
// listener; see the security section of the package README.
func WithHTTPAuth(fn func(*http.Request) error) Option {
	return func(c *config) { c.HTTPAuth = fn }
}

// WithBearerTokenAuth is the batteries-included form of [WithHTTPAuth]: it
// requires an "Authorization: Bearer <token>" header on every HTTP request and
// sends the same header on the cluster-control requests this node makes to its
// peers, so [WithJoinAddr] and [WithLeaveOnStop] keep working against an
// authenticated cluster.
//
// Use the same token on every node. An empty token is ignored, leaving the API
// unauthenticated.
func WithBearerTokenAuth(token string) Option {
	return func(c *config) {
		if token == "" {
			return
		}
		c.HTTPAuth = BearerTokenAuth(token)
		c.HTTPCredential = "Bearer " + token
	}
}

// WithHTTPTLS serves the built-in HTTP API over TLS and makes leader redirects
// use the https scheme. It is independent of [WithTLS], which covers only the
// gRPC Raft transport.
//
// Pair it with [ClientCertAuth] and a tls.Config requiring client certificates
// for mutual TLS:
//
//	tlsCfg := &tls.Config{
//	    Certificates: []tls.Certificate{serverCert},
//	    ClientCAs:    pool,
//	    ClientAuth:   tls.RequireAndVerifyClientCert,
//	}
//	easyraft.WithHTTPTLS(tlsCfg)
//	easyraft.WithHTTPAuth(easyraft.ClientCertAuth("admin", "n1", "n2"))
//
// Ignored when [WithHTTPMux] is used, since the caller owns that listener.
func WithHTTPTLS(tlsCfg *tls.Config) Option {
	return func(c *config) { c.HTTPTLS = tlsCfg }
}

// WithInsecureHTTPAcknowledged lets the HTTP API be served without an
// authorization hook. Without it, or [WithHTTPAuth], a store or manager that
// serves the API refuses to start: the API adds and removes cluster members,
// and an unauthenticated listener is a control plane open to anyone who can
// reach it. Use it only when the exposure is mitigated elsewhere -- a loopback
// bind, a service mesh, a network policy. The node logs a warning at startup.
func WithInsecureHTTPAcknowledged() Option {
	return func(c *config) { c.AcknowledgeInsecureHTTP = true }
}

// WithInsecureTransportAcknowledged lets the Raft transport run in plaintext.
// Without it, or [WithTLS], NewStore and NewManager refuse to start: a Raft
// peer is fully trusted, so a transport anyone can connect to is a cluster
// anyone can take over. Use it on a network that is trusted for reasons
// outside this package -- a loopback bind for a local cluster, a private
// link, a mesh that terminates TLS in front of the process -- and never on
// one that is not. The transport logs a warning once per process.
func WithInsecureTransportAcknowledged() Option {
	return func(c *config) { c.AcknowledgeInsecureTransport = true }
}

// WithLeaseReads lets linearizable reads be served from the leader's
// clock-based read lease, skipping the heartbeat round-trip.
//
// A read lease is only sound while clock drift between the leader and its
// followers stays below the election timeout. Leave it off (the default) if
// you cannot bound drift; the reads are then confirmed by a quorum heartbeat,
// which costs one round-trip and assumes nothing about clocks.
//
// Either way a caller never sees [raft.ErrLeaseExpired]: an expired lease
// falls back to the quorum-confirmed path automatically.
func WithLeaseReads() Option {
	return func(c *config) { c.LeaseReads = true }
}

// WithDataDir sets the directory for persistent log and snapshots.
func WithDataDir(dir string) Option {
	return func(c *config) { c.DataDir = dir }
}

// WithPeers adds a static list of initial peers. The map keys are node IDs
// and the values are host:port addresses for Raft RPC (e.g. "127.0.0.1:7002").
// WithPeers may be called multiple times; maps are merged.
// Peers are pre-populated as known Raft members, so discovery will not issue
// redundant AddServer calls for them.
func WithPeers(peers map[raft.NodeID]string) Option {
	return func(c *config) {
		if c.Peers == nil {
			c.Peers = make(map[raft.NodeID]string)
		}
		for id, addr := range peers {
			c.Peers[id] = addr
		}
	}
}

// WithLogger sets a custom logger. When unset, easyraft logs through
// slog.Default() rather than staying silent.
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) { c.Logger = logger }
}

// WithSnapCount sets the number of log entries between snapshots.
func WithSnapCount(count uint64) Option {
	return func(c *config) {
		c.SnapCount = count
	}
}

// WithDiscovery enables dynamic peer discovery. EasyRaft polls d every
// interval: for each returned peer it calls tr.AddPeer (transport) and, if the
// peer is not already a known cluster member, node.AddServer (Raft membership).
// AddServer calls that fail because this node is not the leader are retried on
// the next poll. A zero interval defaults to 30 s.
//
// Discovered peers are added as non-voting learners: they replicate the log but
// do not vote, so a bogus announcement cannot change the quorum size. Promote
// them with [Store.AddServer] once an operator has verified them, or use
// [WithDiscoveryAsVoter] to skip that step.
//
// Discovery is only as trustworthy as its source. Configure a shared secret on
// udpbroadcast, or use a resolver you control with dnsdiscovery.
//
// Static peers from [WithPeers] are treated as already-known members and will
// not trigger AddServer calls.
func WithDiscovery(d discovery.Discovery, interval time.Duration) Option {
	return func(c *config) {
		c.Discovery = d
		c.DiscoveryInterval = interval
	}
}

// WithDiscoveryAsVoter makes peers found through [WithDiscovery] join as
// voting members instead of learners.
//
// This lets any source the discovery mechanism trusts change the cluster's
// quorum size, so enable it only when that source is authenticated — for
// example udpbroadcast with a shared secret on a trusted subnet.
func WithDiscoveryAsVoter() Option {
	return func(c *config) { c.DiscoveryAsVoter = true }
}

// WithJoinAddr sets the HTTP address(es) of existing cluster nodes to contact
// on startup for cluster joining. The joining node will POST its ID and Raft
// address to each seed in turn until one succeeds; the seed will call
// AddServer on behalf of the joiner and return the current peer list.
//
// Requires that the seed nodes have [WithHTTPAddr] configured. May be called
// multiple times; addresses are appended. If the seeds require authorization,
// configure the matching credential with [WithBearerTokenAuth].
func WithJoinAddr(addrs ...string) Option {
	return func(c *config) {
		c.JoinAddrs = append(c.JoinAddrs, addrs...)
	}
}

// WithJoinAsLearner causes the node to join as a non-voting member
// (learner/observer) when [WithJoinAddr] is also set. Learners replicate the
// log but do not vote in elections or count toward commit quorum. Useful for
// read-replica nodes or nodes that should be promoted to voter later.
func WithJoinAsLearner() Option {
	return func(c *config) { c.JoinAsLearner = true }
}

// WithLeaveOnStop causes [Store.Stop] to remove this node from the cluster
// before shutting down, so the remaining members do not wait for it during
// elections or commits.
//
// On the leader the removal is proposed directly. On a follower — which cannot
// commit a membership change — it is forwarded to the leader's HTTP API, using
// the credential from [WithBearerTokenAuth] when one is configured. If neither
// path is available the reason is logged and shutdown continues. The whole
// attempt is abandoned after 5 seconds.
func WithLeaveOnStop() Option {
	return func(c *config) { c.LeaveOnStop = true }
}

// WithSharedWAL makes every group on a [Manager] append to one write-ahead
// log instead of one per group. It is a Manager-level option and the log
// lives at the Manager's [WithDataDir]; a plain [Store] ignores it, having
// only one group to write for.
//
// With a log per group, G groups appending at once issue G fsyncs, and fsync
// is the expensive part: it is what makes disk throughput rather than CPU the
// limit on how many writing groups a host can carry. One log, synced once per
// batch, gives every group in that batch the same durability for one fsync.
//
// It also means a Manager can find its groups again. `Manager.SharedWAL().Groups()`
// lists what the log holds, which is what a host that runs a changing set of
// groups needs after a restart, and `Remove` forgets a group that has been
// decommissioned so its space can be reclaimed.
//
// With it, per-group [WithDataDir] is neither needed nor used.
func WithSharedWAL() Option {
	return func(c *config) { c.SharedWAL = true }
}

// WithWitness makes this node a witness: a voter that keeps the index and
// term of every log entry, never the entries themselves, and applies nothing
// (Raft dissertation §11.7.2).
//
// It votes and counts towards every quorum like any voter, so two full
// replicas and a witness survive the loss of any one member at a third of the
// storage and none of the state machine a third full replica would cost -- a
// small machine in a third location whose job is to break ties.
//
// A witness holds no data, so reads against it return [ErrWitness] rather
// than the empty answer its collections would otherwise give. It cannot
// become leader, so writes against it are refused with the leader's address
// to redirect to, exactly as on any follower. Everything else -- membership,
// status, health, joining, discovery -- works as usual.
//
// Every other node's view of this one must agree: the peer entry that names
// it, whether from [WithPeers] at bootstrap or from [Store.AddWitness], has
// to say witness too. A node whose recovered membership disagrees refuses to
// start.
func WithWitness() Option {
	return func(c *config) { c.Witness = true }
}

// WithWitnessPeers names the peers from [WithPeers] that are witnesses.
//
// Membership is agreed state, and every node's view of it has to match: a
// peer this node thinks is a full replica but which was built with
// [WithWitness] would be counted on to hold entries it discards. A node whose
// recovered membership disagrees with what it is refuses to start, so the
// mismatch surfaces at the first restart rather than at the first failure.
//
// Only needed at bootstrap. A witness added to a running cluster through
// [Store.AddWitness] is recorded in the log, and nodes that join later learn
// it from there.
func WithWitnessPeers(ids ...raft.NodeID) Option {
	return func(c *config) {
		if c.Witnesses == nil {
			c.Witnesses = make(map[raft.NodeID]bool, len(ids))
		}
		for _, id := range ids {
			c.Witnesses[id] = true
		}
	}
}

// WithCommitQuorum sets how many voters an entry must reach to commit, 0
// meaning a simple majority. The election quorum follows from it, as
// voters - quorum + 1 or a majority, whichever is larger.
//
// This is only what a group is *created* with. The policy is group state,
// agreed through the log like membership, so the first leader writes this
// value into the log and from then on every node counts by the agreed value
// whatever its own configuration says. Change a running group with
// [Store.SetCommitQuorum].
func WithCommitQuorum(quorum int) Option {
	return func(c *config) { c.CommitQuorum = quorum }
}

// WithZones records which failure domain each node sits in and how many of
// them an entry must reach before it counts as committed.
//
// A majority says nothing about where the replicas are: three replicas in one
// availability zone are a quorum, and losing that zone loses every write they
// acknowledged. Requiring two zones means an acknowledged write is on
// hardware in two failure domains before anyone is told it succeeded. The
// cost is liveness, and it is the point: if the second zone is unreachable,
// nothing commits.
//
// A node absent from the map is in no known zone and does not count towards
// the spread. The map is local to this node and never replicated; give the
// same one to every node that might lead.
func WithZones(zones map[raft.NodeID]raft.ZoneID, minCommitZones int) Option {
	return func(c *config) {
		c.Zones = zones
		c.MinCommitZones = minCommitZones
	}
}

// WithPreferredLeader names the node that should hold leadership whenever
// possible. A node that wins an election without being the preferred one
// hands leadership over as soon as it is safe to.
//
// Useful for heterogeneous hardware, or for keeping the leader beside the
// clients that write to it.
func WithPreferredLeader(id raft.NodeID) Option {
	return func(c *config) { c.PreferredLeader = id }
}

// WithMaxClientTableSize bounds the exactly-once table that [Session] and the
// Once methods rely on, in entries. A client that retries after its entry has
// been evicted has its request executed a second time, so size it to outlive
// the retry window of the slowest client.
//
// The bound is group state, agreed through the log and carried in snapshots,
// so this is what a group is created with rather than what each node uses;
// change a running group with [Store.SetMaxClientTableSize].
func WithMaxClientTableSize(entries int) Option {
	return func(c *config) { c.MaxClientTableSize = entries }
}

// WithLeaderBalancing keeps group leadership spread across the hosts given,
// moving it when it bunches up. It applies to a [Manager] and is ignored by a
// single-group [Store], which has nothing to balance.
//
// hosts maps each host to the HTTP address of the easyraft Manager running on
// it, and must include this one -- a host's ID is the node ID its Manager was
// built with, since one Manager is one physical node. This node is asked
// directly; the others over HTTP, carrying whatever credential
// [WithBearerTokenAuth] set.
//
//	easyraft.WithLeaderBalancing(map[raft.HostID]string{
//		"n1": "10.0.0.1:8001",
//		"n2": "10.0.0.2:8001",
//		"n3": "10.0.0.3:8001",
//	}, 30*time.Second)
//
// # Why this exists
//
// Groups elect leaders independently, and nothing coordinates them. Left
// alone, a host that was up while others restarted ends up leading most of
// them -- and a leader does the replication, serves the linearizable reads
// and takes every write, so one host doing all of it is both a bottleneck and
// the failure that hurts most.
//
// # What it costs
//
// A transfer is an election, so a balanced cluster is worth more than a
// perfectly balanced one. Every group is left alone for a cooldown after it
// moves (five intervals by default), a round where any host fails to report
// is skipped entirely rather than planned from a partial view, and leadership
// is only ever moved to a voter that is close enough to the leader to take
// over. Pass [raft.WithGroupCooldown], [raft.WithStatusTimeout] and
// [raft.WithTransferTimeout] through opts to tune that.
//
// Every host may run this. Two controllers with views that disagree would
// hand a group back and forth, which is what the cooldown is there to stop.
func WithLeaderBalancing(hosts map[raft.HostID]string, interval time.Duration,
	opts ...raft.BalanceOption,
) Option {
	return func(c *config) {
		c.BalanceHosts = hosts
		c.BalanceInterval = interval
		c.BalanceOptions = opts
	}
}

// WithMaxProposalBytes caps the size of a single command, refusing a larger
// one with [raft.ErrProposalTooLarge] rather than accepting it.
//
// This exists because a command that cannot be replicated is far worse than
// one that is refused: it is appended to the leader's log, rejected by the
// transport on every send, and retried for ever, while every later proposal
// queues behind it and the group stops making progress with nothing having
// reported an error.
//
// Zero takes the limit from the transport, which is the right answer
// whenever the transport knows one. Set this when it does not, or to hold
// writes well below what the transport would allow.
//
// [Store.Import] divides a backup into chunks that fit inside this, so
// lowering it makes an import take more entries rather than failing.
func WithMaxProposalBytes(n int) Option {
	return func(c *config) { c.MaxProposalBytes = n }
}

// WithTransport uses tr for Raft RPCs instead of building a gRPC transport
// and listening on [WithRaftAddr].
//
// This is the seam behind the in-process clusters in
// [github.com/brunoga/raft/v2/easyraft/easyrafttest], and it is what a
// transport written for an environment gRPC cannot reach plugs into.
//
// The store does not close a transport it was given: something that outlives
// one store, and may be shared by several, is the caller's to close. It also
// does not enforce the TLS acknowledgement, which is about the listener this
// store would otherwise have opened -- whatever tr carries over the wire, and
// whether that is a wire at all, is tr's business.
//
// If tr has an AddPeer(raft.NodeID, string) method, peer addresses from
// [WithPeers] and from joins are registered on it. If it does not, the store
// assumes tr already knows how to reach every peer, which is how an in-memory
// network works.
func WithTransport(tr raft.Transport) Option {
	return func(c *config) { c.Transport = tr }
}

// WithKeyLeaseSweepInterval sets how often the leader looks for key leases
// that have fallen due, which bounds how long a key can outlive the TTL of
// the lease holding it.
//
// This is about the leases granted by [Store.GrantLease], which make a key
// disappear when nothing renews it. It has nothing to do with
// [WithLeaseReads] or [WithLeaseSafetyMargin], which are about the leader
// serving a read without a round-trip.
//
// A shorter interval expires keys closer to their deadline and proposes a
// little more often; nothing is proposed at all while no lease is due. The
// default is one second. Zero keeps the default; a negative value is refused.
func WithKeyLeaseSweepInterval(d time.Duration) Option {
	return func(c *config) { c.KeyLeaseSweepInterval = d }
}

// WithLeaseSafetyMargin shortens the read lease behind [WithLeaseReads] and
// sets how far the wall clock and the monotonic clock may disagree before the
// lease is treated as expired.
//
// A lease read is served without contacting any follower, on the argument
// that none of them can have elected another leader yet. The margin is taken
// off the lease to cover clock rates that differ between machines, and is the
// tolerance beyond which wall-clock time running ahead of monotonic time --
// which is what a paused or live-migrated virtual machine looks like on
// resume -- drops the lease rather than serving from it.
//
// Zero keeps the whole election timeout as the lease and skips the divergence
// check. The default is a tenth of the election timeout.
func WithLeaseSafetyMargin(d time.Duration) Option {
	return func(c *config) { c.LeaseSafetyMargin = d }
}

// WithProposalQueue sets how many writes may wait for the Raft event loop at
// once, and what happens to the next one when that many already are.
//
// [raft.ProposalOverflowWait], the default, blocks until there is room or the
// caller's context is done. [raft.ProposalOverflowReject] returns
// [raft.ErrProposalQueueFull] at once, for a service that would rather shed a
// request than hold a goroutine on it. A size of 0 keeps the default.
func WithProposalQueue(size int, overflow raft.ProposalOverflowPolicy) Option {
	return func(c *config) {
		c.ProposalQueueSize = size
		c.ProposalOverflow = overflow
	}
}

// WithOnRemoved registers a callback invoked once, from its own goroutine,
// when a committed configuration change removes this node from the cluster.
//
// A removed node is not stopped: it goes on running as a non-voting follower
// that nobody replicates to, so that whatever owns it can decide what to do
// -- shut it down, wipe its data directory, keep it for inspection. This is
// where that decision goes, and it may call [Store.Stop].
func WithOnRemoved(fn func()) Option {
	return func(c *config) { c.OnRemoved = fn }
}

// WithTLSFiles configures mutual TLS for the Raft transport from PEM files:
// this node's certificate and private key, and the certificate authority
// that issued every node's.
//
// It exists because the configuration a Raft mesh needs is the one that is
// easy to get wrong. Every node is both a client and a server, so the
// authority goes in as both RootCAs and ClientCAs, and client certificates
// have to be required *and verified* -- anything weaker leaves the Raft port
// open to any client that can reach it. Getting that right also means the
// peer authorizer is installed, binding the node ID an RPC claims to the
// certificate that carried it; see [WithPeerAuthorizer] for why that is a
// separate thing from holding a valid certificate.
//
// The equivalent by hand:
//
//	cert, _ := tls.LoadX509KeyPair(certFile, keyFile)
//	pool := x509.NewCertPool()
//	pem, _ := os.ReadFile(caFile)
//	pool.AppendCertsFromPEM(pem)
//	easyraft.WithTLS(&tls.Config{
//	    Certificates: []tls.Certificate{cert},
//	    RootCAs:      pool,
//	    ClientCAs:    pool,
//	    ClientAuth:   tls.RequireAndVerifyClientCert,
//	    MinVersion:   tls.VersionTLS13,
//	})
//
// The files are read when the store or manager is constructed, so a path
// that is wrong fails there rather than at the first connection between two
// nodes. It does not affect the HTTP API; use [WithHTTPTLS] for that.
func WithTLSFiles(certFile, keyFile, caFile string) Option {
	return func(c *config) {
		c.TLSCertFile, c.TLSKeyFile, c.TLSCAFile = certFile, keyFile, caFile
	}
}

// WithPeerAuthorizer decides whether the node a Raft RPC claims to come from
// is really the node that sent it, replacing the default that [WithTLS]
// installs.
//
// Authentication is not authorization. TLS establishes that the peer holds a
// certificate your CA issued; it says nothing about which node that peer is,
// and every Raft RPC names the node it claims to come from. Without a check
// binding the two, any holder of any certificate from that CA can claim to
// be the leader and make the whole cluster step down.
//
// The default, installed whenever [WithTLS] is given a configuration that
// requires and verifies client certificates, is
// grpctransport.MTLSPeerAuthorizer with its own identity extraction: the
// certificate's Common Name and DNS names. Pass this when your certificates
// carry node identity somewhere else -- a URI SAN, an organizational unit --
// or when you want a different policy entirely.
func WithPeerAuthorizer(fn grpctransport.PeerAuthorizer) Option {
	return func(c *config) { c.PeerAuthorizer = fn }
}

// WithTLS sets the TLS configuration for Raft RPCs. It does not affect the
// HTTP API; use [WithHTTPTLS] for that.
//
// A configuration whose ClientAuth is tls.RequireAndVerifyClientCert -- which
// is what a Raft mesh wants, since every node is both a client and a server
// -- also gets a peer authorizer, binding the node ID an RPC claims to the
// identity in the certificate that carried it. See [WithPeerAuthorizer] for
// why the certificate alone is not enough, and for how to change the policy.
//
// A configuration without it authenticates the server to its clients and
// leaves the clients anonymous, which on a Raft port means anyone who can
// reach it can claim to be the leader. The store logs a warning at startup
// in that case; it does not refuse, because a deployment may be
// authenticating peers somewhere else entirely, such as a service mesh.
func WithTLS(tlsCfg *tls.Config) Option {
	return func(c *config) {
		c.TLS = tlsCfg
	}
}

// WithRaftTiming overrides the Raft protocol timing parameters.
// Zero values are ignored and the easyraft defaults are used instead
// (100 ms tick/heartbeat, 1 s / 2 s election timeout).
func WithRaftTiming(tick, heartbeat, electionMin, electionMax time.Duration) Option {
	return func(c *config) {
		c.TickInterval = tick
		c.HeartbeatInterval = heartbeat
		c.ElectionTimeoutMin = electionMin
		c.ElectionTimeoutMax = electionMax
	}
}

// WithPrometheus sets the Prometheus registerer. Safe to pass the same
// registerer to every store in a [Manager]: collectors are registered once and
// each group's series are distinguished by a "group" label.
func WithPrometheus(reg prometheus.Registerer) Option {
	return func(c *config) {
		c.PromRegisterer = reg
	}
}

// applySnapshotSettings puts SnapCount into a raft.Config, keeping the two
// settings that govern compaction consistent with each other.
//
// SnapshotThreshold is how many entries pass before a snapshot is taken;
// TrailingLogs is how many are kept after one. If the second is not smaller
// than the first, compacting reclaims nothing, and the engine says so with a
// warning and caps it.
//
// That warning used to fire on every easyraft node with default settings,
// because this package defaulted the threshold to 1000 while leaving
// TrailingLogs at the engine's 1024. A warning everyone sees on a correct
// configuration is worse than no warning: it is the one that teaches people
// their logs are full of warnings they are supposed to ignore.
//
// So the default is now the engine's own, which is already consistent, and an
// explicitly chosen SnapCount brings TrailingLogs down with it rather than
// leaving the engine to notice.
func applySnapshotSettings(rCfg *raft.Config, snapCount uint64) {
	if snapCount > 0 {
		rCfg.SnapshotThreshold = snapCount
	}
	if rCfg.SnapshotThreshold > 0 && rCfg.TrailingLogs >= rCfg.SnapshotThreshold {
		// Half, so compaction reclaims something every time it runs, and at
		// least one entry where there is room for one, so a follower barely
		// behind is caught up from the log rather than a whole snapshot. A
		// threshold of 1 leaves no room, and keeping nothing is then the only
		// consistent answer.
		trailing := rCfg.SnapshotThreshold / 2
		if trailing == 0 && rCfg.SnapshotThreshold > 1 {
			trailing = 1
		}
		rCfg.TrailingLogs = trailing
	}
}
