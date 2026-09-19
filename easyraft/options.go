package easyraft

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/discovery"
	"github.com/prometheus/client_golang/prometheus"
)

// Config holds the configuration for an EasyRaft node.
type Config struct {
	ID        raft.NodeID
	RaftAddr  string
	HTTPAddr  string
	DataDir   string
	Peers     map[raft.NodeID]string
	Logger    *slog.Logger
	SnapCount uint64

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

// Option configures an EasyRaft node.
type Option func(*Config)

// WithID sets the Raft node ID.
func WithID(id raft.NodeID) Option {
	return func(c *Config) { c.ID = id }
}

// WithRaftAddr sets the listen address for Raft RPCs (e.g., ":7001").
func WithRaftAddr(addr string) Option {
	return func(c *Config) { c.RaftAddr = addr }
}

// WithHTTPAddr sets the optional listen address for the HTTP API (e.g., ":8001").
//
// The HTTP API can reshape cluster membership, so the listener must not be
// reachable by untrusted clients. Bind it to a management interface (or
// "127.0.0.1:8001") and pair it with [WithHTTPAuth] or [WithBearerTokenAuth].
// The address is also advertised to the rest of the cluster as this node's URL
// for leader redirects.
func WithHTTPAddr(addr string) Option {
	return func(c *Config) { c.HTTPAddr = addr }
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
	return func(c *Config) { c.HTTPMux = mux }
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
	return func(c *Config) { c.HTTPAuth = fn }
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
	return func(c *Config) {
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
	return func(c *Config) { c.HTTPTLS = tlsCfg }
}

// WithInsecureHTTPAcknowledged suppresses the startup warning about serving the
// HTTP API without an authorization hook. Use it only when the exposure is
// mitigated elsewhere — a loopback-only bind, a service mesh, a network policy.
func WithInsecureHTTPAcknowledged() Option {
	return func(c *Config) { c.AcknowledgeInsecureHTTP = true }
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
	return func(c *Config) { c.LeaseReads = true }
}

// WithDataDir sets the directory for persistent log and snapshots.
func WithDataDir(dir string) Option {
	return func(c *Config) { c.DataDir = dir }
}

// WithPeers adds a static list of initial peers. The map keys are node IDs
// and the values are host:port addresses for Raft RPC (e.g. "127.0.0.1:7002").
// WithPeers may be called multiple times; maps are merged.
// Peers are pre-populated as known Raft members, so discovery will not issue
// redundant AddServer calls for them.
func WithPeers(peers map[raft.NodeID]string) Option {
	return func(c *Config) {
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
	return func(c *Config) { c.Logger = logger }
}

// WithSnapCount sets the number of log entries between snapshots.
func WithSnapCount(count uint64) Option {
	return func(c *Config) {
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
	return func(c *Config) {
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
	return func(c *Config) { c.DiscoveryAsVoter = true }
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
	return func(c *Config) {
		c.JoinAddrs = append(c.JoinAddrs, addrs...)
	}
}

// WithJoinAsLearner causes the node to join as a non-voting member
// (learner/observer) when [WithJoinAddr] is also set. Learners replicate the
// log but do not vote in elections or count toward commit quorum. Useful for
// read-replica nodes or nodes that should be promoted to voter later.
func WithJoinAsLearner() Option {
	return func(c *Config) { c.JoinAsLearner = true }
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
	return func(c *Config) { c.LeaveOnStop = true }
}

// WithTLS sets the TLS configuration for Raft RPCs. It does not affect the
// HTTP API; use [WithHTTPTLS] for that.
func WithTLS(tlsCfg *tls.Config) Option {
	return func(c *Config) {
		c.TLS = tlsCfg
	}
}

// WithRaftTiming overrides the Raft protocol timing parameters.
// Zero values are ignored and the easyraft defaults are used instead
// (100 ms tick/heartbeat, 1 s / 2 s election timeout).
func WithRaftTiming(tick, heartbeat, electionMin, electionMax time.Duration) Option {
	return func(c *Config) {
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
	return func(c *Config) {
		c.PromRegisterer = reg
	}
}
