// Package grpctransport provides a production-ready gRPC implementation of the
// raft.Transport interface.
//
// Each GRPCTransport instance is both a gRPC server (receiving inbound RPCs
// from peers) and a gRPC client pool (sending outbound RPCs to peers).
//
// Usage:
//
//	t, err := grpctransport.Listen(":50051")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer t.Close()
//
//	node, err := raft.NewNode(raft.Config{
//	    ID:        "node1",
//	    Transport: t,
//	    // ...
//	})
//	t.Register("node1", node)
//	node.Start()
//
// # Security
//
// The default configuration is plaintext and unauthenticated. A Raft peer is
// by definition fully trusted: an AppendEntries carrying a high term makes
// every node step down, and a TimeoutNow makes a node start an election. A
// transport listening without TLS therefore lets ANY host that can reach the
// port take over the cluster. Never expose a default-configured transport to
// an untrusted network.
//
// For a secure deployment combine two options:
//
//	tlsCfg := ... // mutual TLS: certificates, RootCAs, ClientCAs,
//	              // ClientAuth: tls.RequireAndVerifyClientCert
//	t, err := grpctransport.Listen(":50051",
//	    grpctransport.WithTLSConfig(tlsCfg),
//	    grpctransport.WithPeerAuthorizer(grpctransport.MTLSPeerAuthorizer(nil)),
//	)
//
// WithTLSConfig authenticates the connection; WithPeerAuthorizer authorizes
// each request by checking that the node ID the request claims (LeaderID or
// CandidateID) matches the identity in the peer's verified certificate.
// Without the authorizer, any holder of a certificate issued by the configured
// CA can impersonate any other node. Installing an authorizer also enables
// strict request validation (see WithStrictRequestValidation).
//
// A transport created without TLS logs a warning once at Listen time.
package grpctransport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/brunoga/raft"
	pb "github.com/brunoga/raft/transport/grpctransport/raftpb"
)

// Sentinel errors returned by GRPCTransport. Callers can test for them with
// errors.Is; every error returned by a send method that maps to one of these
// conditions wraps the corresponding sentinel.
var (
	// ErrMessageTooLarge indicates that an outbound or inbound RPC exceeded the
	// configured maximum message size (see WithMaxMessageSize). It is a
	// permanent failure for that particular message: retrying the same request
	// unchanged will fail identically. Callers should react by reducing the
	// payload — a smaller Config.SnapshotChunkSize or a smaller
	// Config.MaxLogEntriesPerRPC — rather than by retrying.
	ErrMessageTooLarge = errors.New("grpctransport: message exceeds the configured maximum size")

	// ErrTransportClosed is returned by every send method once Close has been
	// called. A closed transport never dials again, so a send after Close
	// cannot leak a connection that nothing would go on to close.
	ErrTransportClosed = errors.New("grpctransport: transport is closed")

	// ErrUnauthorizedPeer is returned to a caller whose request was rejected by
	// the peer authorizer installed with WithPeerAuthorizer.
	ErrUnauthorizedPeer = errors.New("grpctransport: peer is not authorized for the claimed node ID")
)

// GRPCTransport implements raft.Transport using gRPC.
// A single instance may serve multiple node IDs (useful for co-located nodes
// in tests), but the typical production usage is one transport per process.
type GRPCTransport struct {
	server   *grpc.Server
	listener net.Listener
	dialOpts []grpc.DialOption // options applied to every outbound connection
	logger   *slog.Logger

	// peerAuth authorizes inbound requests against the claimed node ID. nil
	// disables authorization (the default). Set once at Listen time.
	peerAuth PeerAuthorizer
	// strictValidation rejects inbound requests with an empty claimed node ID.
	// Implied by peerAuth != nil.
	strictValidation bool
	// closeTimeout bounds how long Close waits for in-flight handlers.
	closeTimeout time.Duration

	// afterDial, when non-nil, runs in clientFor between the dial and the
	// decision to keep the resulting connection. It exists so tests can hold a
	// dial open across a concurrent RemovePeer or Close and check that the
	// connection is not orphaned; it is nil in every other configuration.
	afterDial func()

	mu          sync.RWMutex
	closed      bool                              // set by Close; no further dials or sends
	handlers    map[raft.NodeID]raft.Handler      // id → registered handler
	peers       map[raft.NodeID]string            // id → "host:port" address
	clients     map[string]*grpc.ClientConn       // address → connection (cached)
	clientRefs  map[string]int                    // address → number of NodeIDs mapped to it
	groupLookup func(uint64) (raft.Handler, bool) // optional: route by GroupID

	hbWindow     time.Duration     // collection window for heartbeat batching
	hbRPCTimeout time.Duration     // per-RPC timeout for BatchHeartbeats
	hbChanSize   int               // per-peer batcher channel depth
	hbInflight   int               // max concurrent BatchHeartbeats RPCs per peer
	hbBatcher    *heartbeatBatcher // nil until SetGroupLookup is called

	// Counters for BatchHeartbeats server-side processing.
	batchHBServed  atomic.Int64 // number of BatchHeartbeats RPCs received
	batchHBEntries atomic.Int64 // total heartbeat entries processed across all RPCs
	batchHBErrors  atomic.Int64 // entries that failed (lookup or dispatch error)

	// hbSendBlocked counts the number of times a heartbeat Send had to wait
	// because the per-peer batcher channel was full. A sustained non-zero value
	// means hbChanSize is too small for the current group count. Use
	// HeartbeatSendBlocked to observe this counter.
	hbSendBlocked atomic.Int64
}

// BatchHeartbeatsServed returns the number of BatchHeartbeats RPCs this
// transport has served as a receiver. Intended for testing and monitoring.
func (t *GRPCTransport) BatchHeartbeatsServed() int64 { return t.batchHBServed.Load() }

// ResetBatchHeartbeatsServed atomically resets the BatchHeartbeatsServed
// counter to zero and returns the previous value. Use this for rate monitoring:
// call once per reporting interval and treat the return as the count during
// that interval.
func (t *GRPCTransport) ResetBatchHeartbeatsServed() int64 { return t.batchHBServed.Swap(0) }

// BatchHeartbeatEntriesServed returns the total number of individual heartbeat
// entries dispatched across all BatchHeartbeats RPCs. Dividing by
// BatchHeartbeatsServed gives the average batch size.
func (t *GRPCTransport) BatchHeartbeatEntriesServed() int64 { return t.batchHBEntries.Load() }

// ResetBatchHeartbeatEntriesServed atomically resets the
// BatchHeartbeatEntriesServed counter to zero and returns the previous value.
// Use this for rate monitoring: call once per reporting interval and treat the
// return as the count during that interval.
func (t *GRPCTransport) ResetBatchHeartbeatEntriesServed() int64 {
	return t.batchHBEntries.Swap(0)
}

// BatchHeartbeatErrors returns the number of heartbeat entries that could not
// be dispatched: an unregistered group, a rejected authorization, a failing
// HandleAppendEntries, or a request context that was already cancelled. Each
// such entry is reported to its sender as an error rather than as a failed
// AppendEntries. A sustained non-zero value warrants investigation.
func (t *GRPCTransport) BatchHeartbeatErrors() int64 { return t.batchHBErrors.Load() }

// ResetBatchHeartbeatErrors atomically resets the BatchHeartbeatErrors counter
// to zero and returns the previous value. Use this for rate monitoring: call
// once per reporting interval and treat the return as the count during that
// interval.
func (t *GRPCTransport) ResetBatchHeartbeatErrors() int64 { return t.batchHBErrors.Swap(0) }

// HeartbeatSendBlocked returns the cumulative number of times a heartbeat
// Send had to block waiting for space in a per-peer batcher channel. A
// sustained non-zero rate means the cluster has more groups than the channel
// can absorb without back-pressure; increase WithHeartbeatChannelSize.
func (t *GRPCTransport) HeartbeatSendBlocked() int64 { return t.hbSendBlocked.Load() }

// ResetHeartbeatSendBlocked atomically resets the HeartbeatSendBlocked counter
// to zero and returns the previous value. This enables rate monitoring: call
// this method once per reporting interval and treat the return value as the
// count of blocked sends during that interval, rather than the cumulative total.
func (t *GRPCTransport) ResetHeartbeatSendBlocked() int64 { return t.hbSendBlocked.Swap(0) }

const (
	defaultHeartbeatRPCTimeout = 5 * time.Second

	// DefaultMaxMessageSize is the maximum size, in bytes, of a single gRPC
	// message that the transport will send or receive unless overridden with
	// WithMaxMessageSize.
	//
	// gRPC's own default is 4 MiB, which is exactly the core's default
	// Config.SnapshotChunkSize. A full chunk marshals to slightly more than
	// 4 MiB once the surrounding InstallSnapshot fields are added, so the gRPC
	// default makes every large snapshot transfer fail permanently. An
	// AppendEntries batch is capped by entry count rather than by bytes, so it
	// too can grow well past 4 MiB with large commands. 64 MiB leaves ample
	// headroom for both while still bounding the memory a single malformed or
	// hostile message can force the receiver to buffer.
	DefaultMaxMessageSize = 64 << 20

	// DefaultReconnectBaseDelay and DefaultReconnectMaxDelay bound the
	// exponential backoff applied to reconnect attempts. gRPC's defaults (1 s
	// base, 120 s ceiling) are meant for client applications talking to a
	// service; in a Raft mesh a peer that is unreachable for two minutes after
	// it has already come back causes spurious elections and stalled
	// replication, because both delays dwarf any sane election timeout.
	DefaultReconnectBaseDelay = 50 * time.Millisecond
	DefaultReconnectMaxDelay  = 2 * time.Second

	// DefaultCloseTimeout bounds how long Close waits for in-flight RPC
	// handlers to finish before forcing the server down.
	DefaultCloseTimeout = 5 * time.Second

	// defaultHeartbeatInflight is the number of BatchHeartbeats RPCs that may
	// be in flight to a single peer at once. More than one is required so that
	// a wedged peer cannot delay the next tick's heartbeats for every group
	// behind a single stuck RPC.
	defaultHeartbeatInflight = 4

	// maxNodeIDLen bounds the length of a node ID accepted from the wire. Node
	// IDs are operator-assigned host names or UUIDs; anything longer is
	// malformed or hostile and is rejected before it reaches a handler.
	maxNodeIDLen = 256
)

// heartbeatRPCTimeout returns the configured timeout for a single BatchHeartbeats call.
func (t *GRPCTransport) heartbeatRPCTimeout() time.Duration { return t.hbRPCTimeout }

// Option is a functional option for GRPCTransport.
type Option func(*options)

type options struct {
	serverOpts          []grpc.ServerOption
	dialOpts            []grpc.DialOption
	tlsCfg              *tls.Config
	heartbeatWindow     time.Duration
	heartbeatRPCTimeout time.Duration
	heartbeatChanSize   int
	heartbeatInflight   int
	maxMessageSize      int
	reconnectBaseDelay  time.Duration
	reconnectMaxDelay   time.Duration
	closeTimeout        time.Duration
	peerAuth            PeerAuthorizer
	strictValidation    bool
}

// WithServerOptions appends extra gRPC server options (e.g. TLS credentials).
func WithServerOptions(opts ...grpc.ServerOption) Option {
	return func(o *options) { o.serverOpts = append(o.serverOpts, opts...) }
}

// WithDialOptions appends extra gRPC dial options (e.g. TLS credentials).
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(o *options) { o.dialOpts = append(o.dialOpts, opts...) }
}

// WithHeartbeatRPCTimeout sets the per-RPC timeout for BatchHeartbeats calls.
// The batcher applies this timeout to each outbound BatchHeartbeats RPC. The
// default is 5 s. Reduce this value when a fast failure response is preferable
// to waiting for a slow or unreachable peer; increase it on high-latency links.
func WithHeartbeatRPCTimeout(d time.Duration) Option {
	return func(o *options) { o.heartbeatRPCTimeout = d }
}

// WithHeartbeatWindow sets the collection window used by the heartbeat batcher.
// The batcher waits up to this duration after the first heartbeat entry arrives
// before flushing the batch to the peer. The default is 1 ms, which is
// sufficient for same-datacenter clusters where a RunTicker tick disperses all
// G heartbeats within microseconds. Increase this value in high-latency or
// loaded environments where OS scheduling may spread heartbeats across several
// milliseconds.
func WithHeartbeatWindow(d time.Duration) Option {
	return func(o *options) { o.heartbeatWindow = d }
}

// WithHeartbeatChannelSize sets the per-peer channel buffer depth used by the
// heartbeat batcher. Each peerBatcher goroutine drains its channel before each
// RPC flush, so the buffer must be deep enough to absorb one full tick's worth
// of enqueued heartbeats without blocking the RunTicker goroutines. The default
// (1024) is sufficient for clusters with up to ~1000 groups. Increase this
// value if you observe stalls at higher group counts.
func WithHeartbeatChannelSize(n int) Option {
	return func(o *options) { o.heartbeatChanSize = n }
}

// WithHeartbeatMaxInflight sets how many BatchHeartbeats RPCs may be in flight
// to a single peer at the same time. The default is 4.
//
// The batcher collects a window's worth of heartbeats and sends them as one
// RPC. With a single in-flight RPC, a peer whose event loop is wedged stalls
// every subsequent window for that peer: heartbeats queue up behind the stuck
// RPC and are eventually sent long after their callers have given up.
// Allowing a few concurrent RPCs lets healthy windows overtake a wedged one.
// Values below 1 are treated as 1.
func WithHeartbeatMaxInflight(n int) Option {
	return func(o *options) { o.heartbeatInflight = n }
}

// WithMaxMessageSize sets the maximum size in bytes of a single gRPC message,
// applied to sends and receives on both the server and every outbound client
// connection. The default is DefaultMaxMessageSize (64 MiB).
//
// The limit must be large enough for the biggest message the core can produce:
// a full Config.SnapshotChunkSize snapshot chunk plus proto framing, and a
// Config.MaxLogEntriesPerRPC batch of the largest commands the application
// proposes. A message that exceeds the limit fails permanently with an error
// wrapping ErrMessageTooLarge; it is never retryable.
//
// Values below 1 select the default.
func WithMaxMessageSize(n int) Option {
	return func(o *options) { o.maxMessageSize = n }
}

// WithReconnectBackoff configures the exponential backoff applied to reconnect
// attempts on outbound connections. base is the delay after the first failed
// attempt and max is the ceiling the delay grows towards. The defaults are
// DefaultReconnectBaseDelay (50 ms) and DefaultReconnectMaxDelay (2 s), chosen
// so that a peer returning from an outage is retried well within a Raft
// election timeout rather than up to two minutes later.
//
// Non-positive values select the corresponding default.
func WithReconnectBackoff(base, maxDelay time.Duration) Option {
	return func(o *options) {
		o.reconnectBaseDelay = base
		o.reconnectMaxDelay = maxDelay
	}
}

// WithCloseTimeout bounds how long Close waits for in-flight RPC handlers to
// return before forcing the server down and dropping their connections. The
// default is DefaultCloseTimeout (5 s).
//
// A bound is necessary because a handler blocks on its node's event loop; if
// that loop is itself stuck, an unbounded graceful shutdown never completes.
// Non-positive values select the default.
func WithCloseTimeout(d time.Duration) Option {
	return func(o *options) { o.closeTimeout = d }
}

// WithPeerAuthorizer installs a hook that authorizes every inbound request
// against the node ID the request claims to come from (LeaderID for
// AppendEntries, InstallSnapshot and TimeoutNow; CandidateID for RequestVote).
// The hook is called with the request's context, from which it can recover the
// peer's verified TLS identity, and must return nil to accept the request.
//
// Authentication alone is not authorization: with mutual TLS, any holder of a
// certificate issued by the configured CA can complete the handshake and then
// claim to be any node. Use MTLSPeerAuthorizer to bind the claimed node ID to
// the verified certificate.
//
// Installing an authorizer also enables strict request validation, so requests
// with an empty or over-long claimed node ID are rejected before the hook runs
// (see WithStrictRequestValidation).
func WithPeerAuthorizer(fn PeerAuthorizer) Option {
	return func(o *options) { o.peerAuth = fn }
}

// WithStrictRequestValidation rejects inbound requests whose claimed node ID
// (LeaderID or CandidateID) is empty. It is implied by WithPeerAuthorizer and
// is only needed separately when running without an authorizer.
//
// It is not the default because a bare transport is commonly driven directly
// in tests with partially-filled requests; production deployments should
// enable it. Node IDs longer than 256 bytes are always rejected, with or
// without this option.
func WithStrictRequestValidation() Option {
	return func(o *options) { o.strictValidation = true }
}

// WithTLSConfig enables TLS on both the gRPC server and all outbound client
// connections. For mutual TLS, include client certificates in the config passed
// to servers and server certificates in the config passed to clients; the same
// *tls.Config can be used for both when all nodes share a common CA.
//
// WithTLSConfig replaces the default insecure transport credential. It is
// applied before WithServerOptions and WithDialOptions, so callers can still
// append additional options on top.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *options) { o.tlsCfg = cfg }
}

// Listen creates a GRPCTransport bound to addr (e.g. ":50051") and starts the
// gRPC server. Pass functional options to customise the server or client.
//
// Default keepalive settings, message size limits (DefaultMaxMessageSize) and
// reconnect backoff (DefaultReconnectBaseDelay to DefaultReconnectMaxDelay)
// are applied to both the server and the outbound client connections. They can
// be overridden with WithMaxMessageSize, WithReconnectBackoff, and — as a last
// resort — WithServerOptions / WithDialOptions, which are appended last and so
// win over every default.
//
// Unless WithTLSConfig is supplied the transport is plaintext and
// unauthenticated; see the package documentation for why that is unsafe on an
// untrusted network. Listen logs a warning once in that case.
func Listen(addr string, opts ...Option) (*GRPCTransport, error) {
	o := &options{}
	for _, fn := range opts {
		fn(o)
	}

	logger := slog.Default().With("component", "grpctransport")

	// Choose transport credentials based on whether TLS was configured.
	// WithTLSConfig takes precedence; without it the transport is plaintext.
	var serverCreds grpc.ServerOption
	var dialCreds grpc.DialOption
	if o.tlsCfg != nil {
		creds := credentials.NewTLS(o.tlsCfg)
		serverCreds = grpc.Creds(creds)
		dialCreds = grpc.WithTransportCredentials(creds)
	} else {
		dialCreds = grpc.WithTransportCredentials(insecure.NewCredentials())
		warnInsecureOnce.Do(func() {
			logger.Warn("listening without TLS: any host that can reach this port " +
				"can join the cluster, force every node to step down, or trigger an " +
				"election; use WithTLSConfig and WithPeerAuthorizer in production")
		})
	}

	maxMsgSize := o.maxMessageSize
	if maxMsgSize <= 0 {
		maxMsgSize = DefaultMaxMessageSize
	}

	// Keepalive lets either side detect a dead connection quickly even when
	// no RPCs are in flight — important for a Raft cluster where a silent
	// network partition should not stall elections indefinitely.
	//
	// MaxConnectionAge and MaxConnectionIdle are deliberately left unset.
	// Recycling a connection on a fixed schedule is useful when a pool of
	// clients must follow a moving set of backends; a Raft mesh is a fixed set
	// of long-lived peer links, where forcing a GOAWAY and a redial on every
	// link every 30 s only adds latency spikes and reconnect races with no
	// rebalancing benefit. Liveness is covered by the keepalive pings below.
	defaultServerOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxMsgSize),
		grpc.MaxSendMsgSize(maxMsgSize),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// Ping the client if silent for 5 s to check liveness.
			Time:    5 * time.Second,
			Timeout: 1 * time.Second, // close if no pong within 1 s
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second, // reject pings faster than this
			PermitWithoutStream: true,            // allow pings when idle
		}),
	}
	if serverCreds != nil {
		defaultServerOpts = append(defaultServerOpts, serverCreds)
	}
	defaultServerOpts = append(defaultServerOpts, o.serverOpts...)

	baseDelay := o.reconnectBaseDelay
	if baseDelay <= 0 {
		baseDelay = DefaultReconnectBaseDelay
	}
	maxDelay := o.reconnectMaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultReconnectMaxDelay
	}
	if maxDelay < baseDelay {
		maxDelay = baseDelay
	}

	defaultDialOpts := []grpc.DialOption{
		dialCreds,
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMsgSize),
			grpc.MaxCallSendMsgSize(maxMsgSize),
		),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  baseDelay,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   maxDelay,
			},
			// Give a single connection attempt at least this long to complete
			// before it is abandoned, even when MaxDelay is shorter.
			MinConnectTimeout: time.Second,
		}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			// Send pings every 10 s when the connection is idle.
			Time:                10 * time.Second,
			Timeout:             5 * time.Second, // close if no pong within 5 s
			PermitWithoutStream: true,            // ping even when no RPCs are outstanding
		}),
	}
	defaultDialOpts = append(defaultDialOpts, o.dialOpts...)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("grpctransport.Listen: %w", err)
	}

	hbWin := o.heartbeatWindow
	if hbWin == 0 {
		hbWin = defaultHBWindow
	}
	hbRPCTimeout := o.heartbeatRPCTimeout
	if hbRPCTimeout == 0 {
		hbRPCTimeout = defaultHeartbeatRPCTimeout
	}
	hbChanSize := o.heartbeatChanSize
	if hbChanSize <= 0 {
		hbChanSize = hbChanSizeDefault
	}
	hbInflight := o.heartbeatInflight
	if hbInflight <= 0 {
		hbInflight = defaultHeartbeatInflight
	}
	closeTimeout := o.closeTimeout
	if closeTimeout <= 0 {
		closeTimeout = DefaultCloseTimeout
	}
	t := &GRPCTransport{
		server:           grpc.NewServer(defaultServerOpts...),
		listener:         ln,
		dialOpts:         defaultDialOpts,
		logger:           logger,
		peerAuth:         o.peerAuth,
		strictValidation: o.strictValidation || o.peerAuth != nil,
		closeTimeout:     closeTimeout,
		hbWindow:         hbWin,
		hbRPCTimeout:     hbRPCTimeout,
		hbChanSize:       hbChanSize,
		hbInflight:       hbInflight,
		handlers:         make(map[raft.NodeID]raft.Handler),
		peers:            make(map[raft.NodeID]string),
		clients:          make(map[string]*grpc.ClientConn),
		clientRefs:       make(map[string]int),
	}

	pb.RegisterRaftServiceServer(t.server, &grpcServer{transport: t})
	go t.server.Serve(ln) //nolint:errcheck // Serve always returns a non-nil error after GracefulStop; unactionable here.
	return t, nil
}

// warnInsecureOnce ensures the plaintext warning is emitted once per process
// rather than once per transport, so a multi-group process does not flood its
// log with the same line.
var warnInsecureOnce sync.Once

// Addr returns the listening address (useful when addr was ":0").
func (t *GRPCTransport) Addr() string {
	return t.listener.Addr().String()
}

// AddPeer registers the address of a remote Raft node. Must be called for
// every peer before RPCs are sent to it.
//
// Multiple NodeIDs may map to the same physical address (common in multi-Raft
// deployments where all groups on a peer share one gRPC server). The
// underlying *grpc.ClientConn is shared and reference-counted; it is only
// closed when the last NodeID mapped to that address calls RemovePeer.
func (t *GRPCTransport) AddPeer(id raft.NodeID, addr string) {
	t.mu.Lock()
	t.peers[id] = addr
	t.clientRefs[addr]++
	t.mu.Unlock()
}

// RemovePeer unregisters a remote Raft node. The underlying *grpc.ClientConn
// is reference-counted (incremented by AddPeer, decremented here) and only
// closed when no remaining NodeID maps to the same address. This prevents a
// RemovePeer call for one Raft group from disrupting other groups that share
// the same physical peer address.
//
// After this call, any outbound RPC to id will return an error until AddPeer
// is called again. Use this when a node is permanently decommissioned to
// prevent stale connection accumulation.
func (t *GRPCTransport) RemovePeer(id raft.NodeID) {
	t.mu.Lock()
	addr, ok := t.peers[id]
	var cc *grpc.ClientConn
	if ok {
		delete(t.peers, id)
		t.clientRefs[addr]--
		if t.clientRefs[addr] <= 0 {
			delete(t.clientRefs, addr)
			cc = t.clients[addr] // nil if not yet dialled
			delete(t.clients, addr)
		}
	}
	t.mu.Unlock()
	if cc != nil {
		cc.Close() //nolint:errcheck // best-effort cleanup; error is unactionable here.
	}
	// Stop the peerBatcher goroutine for this peer so it doesn't accumulate
	// after the peer leaves the cluster. Read hbBatcher under RLock to avoid
	// a data race with the concurrent SetGroupLookup write path.
	t.mu.RLock()
	batcher := t.hbBatcher
	t.mu.RUnlock()
	if batcher != nil {
		batcher.removePeer(id)
	}
}

// Register implements raft.Transport. It associates a handler with a node ID
// so inbound RPCs addressed to that node are dispatched correctly.
func (t *GRPCTransport) Register(id raft.NodeID, h raft.Handler) {
	t.mu.Lock()
	t.handlers[id] = h
	t.mu.Unlock()
}

// Unregister implements raft.Transport. It removes the handler associated
// with id.
func (t *GRPCTransport) Unregister(id raft.NodeID) {
	t.mu.Lock()
	delete(t.handlers, id)
	t.mu.Unlock()
}

// SetGroupLookup installs a function that maps a GroupID to its Handler.
// When set, inbound RPCs are routed by the GroupID embedded in the request
// proto rather than by the x-raft-node-id metadata header. This is the
// primary routing mechanism for multi-Raft deployments; single-group usage
// does not need to call this method.
//
// SetGroupLookup also enables heartbeat batching: all outbound AppendEntries
// with no log entries — including both regular heartbeats and read-barrier
// rounds — are coalesced per-peer into a single BatchHeartbeats RPC, reducing
// cost from O(G×P) to O(P) per tick interval. The ReadBarrier flag is
// preserved through the batched entry so followers can satisfy in-flight
// ReadIndex futures.
//
// The lookup function is typically Manager.Lookup. Canonical multi-Raft
// wiring (one physical node, one transport, multiple groups):
//
//	mgr := raft.NewManager()
//	t, _ := grpctransport.Listen(":50051")
//	t.SetGroupLookup(mgr.Lookup) // must be called before nodes start sending RPCs
//
//	for _, cfg := range groupConfigs {
//	    cfg.Transport = t          // all groups share the same transport
//	    node, _ := raft.New(&cfg)
//	    mgr.AddAndStart(cfg.GroupID, node)
//	    // AddPeer must cover every (groupID, peerNodeID) pair so that
//	    // clientFor can resolve outbound RPCs to physical addresses.
//	}
//
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//	go mgr.RunTicker(ctx, 10*time.Millisecond)
//
// SetGroupLookup is idempotent: subsequent calls update the lookup function
// and reuse the existing heartbeat batcher.
func (t *GRPCTransport) SetGroupLookup(fn func(uint64) (raft.Handler, bool)) {
	t.mu.Lock()
	t.groupLookup = fn
	if t.hbBatcher == nil {
		t.hbBatcher = newHeartbeatBatcher(t)
	}
	t.mu.Unlock()
}

// Close shuts down the server and closes all cached client connections. It
// marks the transport closed first, so any concurrent or subsequent send
// returns ErrTransportClosed instead of dialling a connection that nobody
// would ever close.
//
// The server is stopped gracefully, but only for up to the close timeout (see
// WithCloseTimeout). An RPC handler blocks on its node's event loop; if that
// loop is itself wedged, an unbounded GracefulStop would never return, so
// after the timeout the server is forced down and in-flight handlers are
// abandoned.
//
// Close is idempotent; calling it more than once is a no-op that returns nil.
func (t *GRPCTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	batcher := t.hbBatcher
	t.mu.Unlock()

	if batcher != nil {
		batcher.stop()
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t.server.GracefulStop()
	}()
	timer := time.NewTimer(t.closeTimeout)
	select {
	case <-stopped:
		timer.Stop()
	case <-timer.C:
		t.logger.Warn("graceful shutdown timed out; forcing the server down",
			slog.Duration("timeout", t.closeTimeout))
		t.server.Stop()
		<-stopped
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for _, cc := range t.clients {
		cc.Close() //nolint:errcheck // best-effort cleanup; error is unactionable during shutdown.
	}
	t.clients = make(map[string]*grpc.ClientConn)
	t.clientRefs = make(map[string]int)
	t.hbBatcher = nil // prevent use-after-close from silently returning errBatcherStopped
	return nil
}

// ---- client-side helpers ---------------------------------------------------

// clientFor returns a cached (or newly dialled) gRPC connection to the peer.
//
// The dial itself happens outside the lock (grpc.NewClient is non-blocking but
// still does name-resolver setup), but the decision to keep the new connection
// is re-validated under the write lock against both the closed flag and the
// current peer table. A RemovePeer or Close that lands while the dial is in
// progress therefore causes the fresh connection to be closed rather than
// orphaned in a map nobody will clean up.
func (t *GRPCTransport) clientFor(to raft.NodeID) (pb.RaftServiceClient, error) {
	// Single read lock covers the closed flag plus the peers and clients maps,
	// avoiding a second lock acquisition on the happy path (cached connection).
	t.mu.RLock()
	closed := t.closed
	addr, ok := t.peers[to]
	var cc *grpc.ClientConn
	var cached bool
	if ok {
		cc, cached = t.clients[addr]
	}
	t.mu.RUnlock()
	if closed {
		return nil, ErrTransportClosed
	}
	if !ok {
		return nil, fmt.Errorf("grpctransport: no address registered for peer %q", to)
	}
	if cached {
		return pb.NewRaftServiceClient(cc), nil
	}

	// Dial outside the lock to avoid blocking other goroutines.
	// grpc.NewClient is non-blocking; the connection is established lazily.
	cc, err := grpc.NewClient(addr, t.dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpctransport: dial %q: %w", addr, err)
	}
	if t.afterDial != nil {
		t.afterDial()
	}

	t.mu.Lock()
	switch {
	case t.closed:
		// Close ran while we were dialling; it has already drained t.clients,
		// so storing here would leak the connection.
		t.mu.Unlock()
		cc.Close() //nolint:errcheck // best-effort cleanup; error is unactionable here.
		return nil, ErrTransportClosed
	case t.peers[to] != addr:
		// RemovePeer (or a re-AddPeer to a different address) ran while we were
		// dialling. Storing under the stale address would orphan the connection.
		t.mu.Unlock()
		cc.Close() //nolint:errcheck // best-effort cleanup; error is unactionable here.
		return nil, fmt.Errorf("grpctransport: peer %q was removed while dialling %q", to, addr)
	}
	// Check again after acquiring write lock (another goroutine may have dialled).
	if existing, ok := t.clients[addr]; ok {
		t.mu.Unlock()
		cc.Close() //nolint:errcheck // best-effort cleanup; error is unactionable during shutdown.
		return pb.NewRaftServiceClient(existing), nil
	}
	t.clients[addr] = cc
	t.mu.Unlock()
	return pb.NewRaftServiceClient(cc), nil
}

// outgoingContext stamps the target node ID onto the outgoing gRPC metadata.
// The receiving side uses it to pick between several raft.Handlers registered
// on one transport when requests are not routed by GroupID (see
// SetGroupLookup). It names the intended recipient, not the sender, and so
// carries no authority: peer identity is established by TLS and checked by the
// authorizer installed with WithPeerAuthorizer.
func outgoingContext(ctx context.Context, to raft.NodeID) context.Context {
	return AppendNodeIDToContext(ctx, string(to))
}

// sendErr normalises an error returned by an outbound RPC. A message that
// overflows the configured size limit is reported with a status code shared by
// several unrelated conditions, so it is wrapped with ErrMessageTooLarge to
// make it distinguishable from an ordinary transport failure: the former is
// permanent and must not be retried unchanged, the latter is worth retrying.
func sendErr(rpc string, err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if ok && st.Code() == codes.ResourceExhausted {
		return fmt.Errorf("grpctransport: %s: %w: %v", rpc, ErrMessageTooLarge, err)
	}
	if ok && st.Code() == codes.PermissionDenied {
		return fmt.Errorf("grpctransport: %s: %w: %v", rpc, ErrUnauthorizedPeer, err)
	}
	return err
}

// ---- raft.Transport implementation -----------------------------------------

func (t *GRPCTransport) RequestVote(ctx context.Context, to raft.NodeID, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	c, err := t.clientFor(to)
	if err != nil {
		return nil, err
	}
	pbResp, err := c.RequestVote(outgoingContext(ctx, to), rvReqToProto(req))
	if err != nil {
		return nil, sendErr("RequestVote", err)
	}
	return rvRespFromProto(pbResp), nil
}

func (t *GRPCTransport) AppendEntries(ctx context.Context, to raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	// In multi-Raft mode, all empty AppendEntries (regular heartbeats and
	// read-barrier rounds alike) are batched per-peer to reduce RPC count from
	// O(G×P) to O(P) per tick. ReadBarrier is forwarded through HeartbeatEntry
	// so the follower's response routes back to the correct ReadIndex future.
	if len(req.Entries) == 0 {
		t.mu.RLock()
		closed, batcher := t.closed, t.hbBatcher
		t.mu.RUnlock()
		if closed {
			return nil, ErrTransportClosed
		}
		if batcher != nil {
			return batcher.Send(ctx, to, req)
		}
	}
	c, err := t.clientFor(to)
	if err != nil {
		return nil, err
	}
	pbResp, err := c.AppendEntries(outgoingContext(ctx, to), aeReqToProto(req))
	if err != nil {
		return nil, sendErr("AppendEntries", err)
	}
	return aeRespFromProto(pbResp), nil
}

func (t *GRPCTransport) InstallSnapshot(ctx context.Context, to raft.NodeID, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	c, err := t.clientFor(to)
	if err != nil {
		return nil, err
	}
	pbResp, err := c.InstallSnapshot(outgoingContext(ctx, to), isReqToProto(req))
	if err != nil {
		return nil, sendErr("InstallSnapshot", err)
	}
	return &raft.InstallSnapshotResponse{Term: raft.Term(pbResp.Term)}, nil
}

func (t *GRPCTransport) TimeoutNow(ctx context.Context, to raft.NodeID, req *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	c, err := t.clientFor(to)
	if err != nil {
		return nil, err
	}
	pbResp, err := c.TimeoutNow(outgoingContext(ctx, to), &pb.TimeoutNowRequest{
		GroupId:  req.GroupID,
		Term:     uint64(req.Term),
		LeaderId: string(req.LeaderID),
	})
	if err != nil {
		return nil, sendErr("TimeoutNow", err)
	}
	return &raft.TimeoutNowResponse{Term: raft.Term(pbResp.Term)}, nil
}

func (t *GRPCTransport) ReadIndex(ctx context.Context, to raft.NodeID, req *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	c, err := t.clientFor(to)
	if err != nil {
		return nil, err
	}
	pbResp, err := c.ReadIndex(outgoingContext(ctx, to), riReqToProto(req))
	if err != nil {
		return nil, sendErr("ReadIndex", err)
	}
	return riRespFromProtoErr(pbResp)
}

// ---- gRPC server-side implementation ---------------------------------------

// grpcServer implements pb.RaftServiceServer by dispatching to the registered
// raft.Handler for the node identified by the incoming request's target ID.
// Because a single server may host multiple nodes (uncommon in production but
// convenient in tests), the server extracts the target node ID from a gRPC
// metadata header ("x-raft-node-id"). If the header is absent, it falls back
// to the single registered handler (the common single-node-per-process case).
type grpcServer struct {
	pb.UnimplementedRaftServiceServer
	transport *GRPCTransport
}

// handlerForGroup returns the Handler for the given groupID. If a group-lookup
// function has been installed (multi-Raft mode), it takes priority. Otherwise
// the call falls back to NodeID-header routing (single-group / test mode).
//
// The mu.RLock/RUnlock around the groupLookup read is intentionally lightweight:
// groupLookup is set once at startup via SetGroupLookup and never changed
// afterward, so the lock is released immediately after the pointer is read.
// BatchHeartbeats skips this per-entry lock by reading groupLookup once at the
// top of the handler — an optimization that is valid because groupLookup is
// immutable after initialization. If dynamic re-registration ever becomes
// necessary, both paths must be revisited.
func (s *grpcServer) handlerForGroup(ctx context.Context, groupID uint64) (raft.Handler, error) {
	s.transport.mu.RLock()
	lookup := s.transport.groupLookup
	s.transport.mu.RUnlock()

	if lookup != nil {
		// In multi-Raft mode every inbound RPC must carry a non-zero GroupID.
		// GroupID==0 is only valid for single-group deployments that have not
		// called SetGroupLookup.
		if groupID == 0 {
			return nil, status.Error(codes.InvalidArgument,
				"GroupID must be non-zero when SetGroupLookup is installed")
		}
		h, ok := lookup(groupID)
		if !ok {
			return nil, status.Errorf(codes.NotFound, "no handler for group %d", groupID)
		}
		return h, nil
	}
	return s.handler(ctx)
}

func (s *grpcServer) handler(ctx context.Context) (raft.Handler, error) {
	s.transport.mu.RLock()
	defer s.transport.mu.RUnlock()
	if len(s.transport.handlers) == 1 {
		for _, h := range s.transport.handlers {
			return h, nil
		}
	}
	// Multi-node: look up by metadata header.
	md, ok := grpcMetadataFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "missing x-raft-node-id metadata")
	}
	id := raft.NodeID(md)
	h, exists := s.transport.handlers[id]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "no handler for node %q", id)
	}
	return h, nil
}

func (s *grpcServer) RequestVote(ctx context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	if err := s.transport.authorize(ctx, "RequestVote", req.CandidateId); err != nil {
		return nil, err
	}
	h, err := s.handlerForGroup(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	resp, err := h.HandleRequestVote(ctx, rvReqFromProto(req))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return rvRespToProto(resp), nil
}

func (s *grpcServer) AppendEntries(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	if err := s.transport.authorize(ctx, "AppendEntries", req.LeaderId); err != nil {
		return nil, err
	}
	h, err := s.handlerForGroup(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	resp, err := h.HandleAppendEntries(ctx, aeReqFromProto(req))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return aeRespToProto(resp), nil
}

func (s *grpcServer) InstallSnapshot(ctx context.Context, req *pb.InstallSnapshotRequest) (*pb.InstallSnapshotResponse, error) {
	if err := s.transport.authorize(ctx, "InstallSnapshot", req.LeaderId); err != nil {
		return nil, err
	}
	h, err := s.handlerForGroup(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	resp, err := h.HandleInstallSnapshot(ctx, isReqFromProto(req))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &pb.InstallSnapshotResponse{Term: uint64(resp.Term)}, nil
}

func (s *grpcServer) TimeoutNow(ctx context.Context, req *pb.TimeoutNowRequest) (*pb.TimeoutNowResponse, error) {
	if err := s.transport.authorize(ctx, "TimeoutNow", req.LeaderId); err != nil {
		return nil, err
	}
	h, err := s.handlerForGroup(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	resp, err := h.HandleTimeoutNow(ctx, &raft.TimeoutNowRequest{
		GroupID:  req.GroupId,
		Term:     raft.Term(req.Term),
		LeaderID: raft.NodeID(req.LeaderId),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &pb.TimeoutNowResponse{Term: uint64(resp.Term)}, nil
}

func (s *grpcServer) ReadIndex(ctx context.Context, req *pb.ReadIndexRequest) (*pb.ReadIndexResponse, error) {
	h, err := s.handlerForGroup(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	resp, err := h.HandleReadIndex(ctx, riReqFromProto(req))
	if err != nil {
		// Encode NotLeaderError into the response body so the client can
		// reconstruct the typed error. All other errors map to codes.Internal.
		var nle *raft.NotLeaderError
		if errors.As(err, &nle) {
			return &pb.ReadIndexResponse{LeaderId: string(nle.Leader)}, nil
		}
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return riRespToProto(resp), nil
}

// BatchHeartbeats demultiplexes a batch of heartbeats, dispatching a synthetic
// empty AppendEntries to each registered group and collecting the responses.
// A group that cannot be dispatched to — unknown, or whose handler failed —
// yields a per-entry error result rather than failing the whole batch, so a
// late-joiner or just-removed group doesn't disrupt others. See HeartbeatResult
// in the .proto for the distinction between an error result and a genuine
// Raft-level failure.
//
// Dispatches to all groups concurrently so that a temporarily stalled node
// event channel cannot delay responses to the rest of the batch.
func (s *grpcServer) BatchHeartbeats(ctx context.Context, req *pb.BatchedHeartbeatRequest) (*pb.BatchedHeartbeatResponse, error) {
	s.transport.batchHBServed.Add(1)
	n := len(req.Entries)
	s.transport.batchHBEntries.Add(int64(n))

	// Pre-fetch the lookup function once under a single RLock acquisition.
	// Without this, each concurrently spawned dispatch goroutine would acquire
	// and release mu.RLock independently, causing O(G) lock round-trips per RPC.
	s.transport.mu.RLock()
	lookup := s.transport.groupLookup
	s.transport.mu.RUnlock()

	// Reuse a bounded set of goroutines for the common case, but never let a
	// slow group hold up the dispatch of the groups queued behind it. A
	// semaphore that the enqueue loop *waits* on would do exactly that: once
	// GOMAXPROCS×4 groups with busy event loops are in flight, every remaining
	// group in the batch — all of them healthy — would sit undispatched behind
	// them, and their leaders would see heartbeat timeouts caused purely by
	// unrelated groups co-located on this node. So a full semaphore falls
	// through to an unslotted goroutine instead of blocking. Either way the
	// goroutine count stays bounded by the batch size; the semaphore only caps
	// how many of them are recycled across entries.
	maxWorkers := min(n, runtime.GOMAXPROCS(0)*4)
	sem := make(chan struct{}, maxWorkers)

	// resultCh is buffered to n so that goroutines launched before a ctx
	// cancellation can always write their result and exit without blocking,
	// even if we return early from the collect loop below.
	resultCh := make(chan *pb.HeartbeatResult, n)
	for _, entry := range req.Entries {
		e := entry
		// Respect ctx cancellation: if the RPC context is already done we write an
		// error result directly so the collect loop below always receives exactly
		// n items.
		if ctx.Err() != nil {
			s.transport.batchHBErrors.Add(1)
			resultCh <- heartbeatErrResult(e.GroupId, status.FromContextError(ctx.Err()))
			continue
		}
		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }() // release slot on completion
				resultCh <- s.dispatchHeartbeatEntry(ctx, e, lookup)
			}()
		default:
			go func() {
				resultCh <- s.dispatchHeartbeatEntry(ctx, e, lookup)
			}()
		}
	}

	results := make([]*pb.HeartbeatResult, 0, n)
	for range n {
		// Escape early if the client disconnected or the RPC deadline fired.
		// resultCh is fully buffered so goroutines still running will write their
		// results and exit without blocking; the unread items are GC'd.
		select {
		case r := <-resultCh:
			results = append(results, r)
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	return &pb.BatchedHeartbeatResponse{Results: results}, nil
}

// heartbeatErrResult builds a HeartbeatResult that reports a dispatch failure
// rather than a Raft-level answer. The sender turns it back into a Go error so
// the leader treats the heartbeat as dropped.
func heartbeatErrResult(groupID uint64, st *status.Status) *pb.HeartbeatResult {
	return &pb.HeartbeatResult{
		GroupId:      groupID,
		ErrorCode:    uint32(st.Code()),
		ErrorMessage: st.Message(),
	}
}

// dispatchHeartbeatEntry processes a single entry from a BatchHeartbeats
// request and returns the result. Called concurrently from BatchHeartbeats.
// lookup is the group-lookup function pre-fetched by the caller; it is always
// non-nil when reached via BatchHeartbeats (multi-Raft mode only).
//
// A failure to dispatch — an unregistered group, a rejected authorization, a
// handler that returned an error — is reported as an error result, never as
// Success:false. The two are not interchangeable: Success:false is a Raft-level
// statement that the follower's log does not match, and the leader answers it
// by backing nextIndex up (or, with no conflict hints, all the way to 1 and
// then shipping a full snapshot). Reporting a stopped node or a missing group
// that way would make a leader rewind a follower that is perfectly caught up.
// The unbatched AppendEntries path surfaces these conditions as gRPC errors,
// which the core treats as a dropped RPC; the batched path must match it.
func (s *grpcServer) dispatchHeartbeatEntry(ctx context.Context, entry *pb.HeartbeatEntry, lookup func(uint64) (raft.Handler, bool)) *pb.HeartbeatResult {
	if err := s.transport.authorize(ctx, "BatchHeartbeats", entry.LeaderId); err != nil {
		s.transport.batchHBErrors.Add(1)
		return heartbeatErrResult(entry.GroupId, status.Convert(err))
	}

	var h raft.Handler
	if lookup != nil {
		if entry.GroupId == 0 {
			s.transport.batchHBErrors.Add(1)
			return heartbeatErrResult(0, status.New(codes.InvalidArgument,
				"GroupID must be non-zero when SetGroupLookup is installed"))
		}
		var ok bool
		h, ok = lookup(entry.GroupId)
		if !ok {
			// Not-found is expected for late-joiners / just-removed groups.
			s.transport.logger.LogAttrs(ctx, slog.LevelDebug,
				"BatchHeartbeats: group lookup failed",
				slog.Uint64("group_id", entry.GroupId),
			)
			s.transport.batchHBErrors.Add(1)
			return heartbeatErrResult(entry.GroupId, status.Newf(codes.NotFound,
				"no handler for group %d", entry.GroupId))
		}
	} else {
		// Defensive fallback (should not happen: BatchHeartbeats requires
		// SetGroupLookup to be installed).
		var hErr error
		h, hErr = s.handlerForGroup(ctx, entry.GroupId)
		if hErr != nil {
			s.transport.batchHBErrors.Add(1)
			return heartbeatErrResult(entry.GroupId, status.Convert(hErr))
		}
	}
	aeReq := &raft.AppendEntriesRequest{
		GroupID:      entry.GroupId,
		Term:         raft.Term(entry.Term),
		LeaderID:     raft.NodeID(entry.LeaderId),
		PrevLogIndex: raft.Index(entry.PrevLogIndex),
		PrevLogTerm:  raft.Term(entry.PrevLogTerm),
		LeaderCommit: raft.Index(entry.LeaderCommit),
		ReadBarrier:  entry.ReadBarrier,
	}
	aeResp, aeErr := h.HandleAppendEntries(ctx, aeReq)
	if aeErr != nil {
		s.transport.logger.LogAttrs(ctx, slog.LevelWarn,
			"BatchHeartbeats: HandleAppendEntries failed",
			slog.Uint64("group_id", entry.GroupId),
			slog.Any("err", aeErr),
		)
		s.transport.batchHBErrors.Add(1)
		return heartbeatErrResult(entry.GroupId, status.Newf(codes.Internal, "%v", aeErr))
	}
	// Mirror every field of the AppendEntries answer, conflict hints included.
	// Dropping them forces the leader onto the slow path: without a hint it
	// rewinds nextIndex one entry at a time, or resets it to 1 outright.
	return &pb.HeartbeatResult{
		GroupId:       entry.GroupId,
		Term:          uint64(aeResp.Term),
		Success:       aeResp.Success,
		ConflictIndex: uint64(aeResp.ConflictIndex),
		ConflictTerm:  uint64(aeResp.ConflictTerm),
	}
}
