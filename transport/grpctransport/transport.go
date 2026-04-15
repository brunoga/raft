// Package grpctransport provides a production-ready gRPC implementation of the
// raft.Transport interface.
//
// Each GRPCTransport instance is both a gRPC server (receiving inbound RPCs
// from peers) and a gRPC client pool (sending outbound RPCs to peers).
//
// Usage:
//
//	t, err := grpctransport.Listen(":50051", nil)
//	cfg.Transport = t
//	cfg.Transport.Register(cfg.ID, node)
//	node, _ := raft.New(cfg)
//	node.Start()
//	defer t.Close()
package grpctransport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/brunoga/raft"
	pb "github.com/brunoga/raft/transport/grpctransport/raftpb"
)

// GRPCTransport implements raft.Transport using gRPC.
// A single instance may serve multiple node IDs (useful for co-located nodes
// in tests), but the typical production usage is one transport per process.
type GRPCTransport struct {
	server   *grpc.Server
	listener net.Listener
	dialOpts []grpc.DialOption // options applied to every outbound connection
	logger   *slog.Logger

	mu          sync.RWMutex
	handlers    map[raft.NodeID]raft.Handler      // id → registered handler
	peers       map[raft.NodeID]string            // id → "host:port" address
	clients     map[string]*grpc.ClientConn       // address → connection (cached)
	groupLookup func(uint64) (raft.Handler, bool) // optional: route by GroupID

	hbBatcher *heartbeatBatcher // nil until SetGroupLookup is called

	// Counters for BatchHeartbeats server-side processing.
	batchHBServed  atomic.Int64 // number of BatchHeartbeats RPCs received
	batchHBEntries atomic.Int64 // total heartbeat entries processed across all RPCs
	batchHBErrors  atomic.Int64 // entries that failed (lookup or dispatch error)
}

// BatchHeartbeatsServed returns the number of BatchHeartbeats RPCs this
// transport has served as a receiver. Intended for testing and monitoring.
func (t *GRPCTransport) BatchHeartbeatsServed() int64 { return t.batchHBServed.Load() }

// BatchHeartbeatEntriesServed returns the total number of individual heartbeat
// entries dispatched across all BatchHeartbeats RPCs. Dividing by
// BatchHeartbeatsServed gives the average batch size.
func (t *GRPCTransport) BatchHeartbeatEntriesServed() int64 { return t.batchHBEntries.Load() }

// BatchHeartbeatErrors returns the number of heartbeat entries that could not
// be dispatched (group not found or HandleAppendEntries error). A non-zero
// sustained value warrants investigation.
func (t *GRPCTransport) BatchHeartbeatErrors() int64 { return t.batchHBErrors.Load() }

// heartbeatRPCTimeout returns the timeout for a single BatchHeartbeats call.
// Heartbeats are best-effort, so we use a moderate fixed timeout.
func (t *GRPCTransport) heartbeatRPCTimeout() time.Duration { return 5 * time.Second }

// Option is a functional option for GRPCTransport.
type Option func(*options)

type options struct {
	serverOpts []grpc.ServerOption
	dialOpts   []grpc.DialOption
	tlsCfg     *tls.Config
}

// WithServerOptions appends extra gRPC server options (e.g. TLS credentials).
func WithServerOptions(opts ...grpc.ServerOption) Option {
	return func(o *options) { o.serverOpts = append(o.serverOpts, opts...) }
}

// WithDialOptions appends extra gRPC dial options (e.g. TLS credentials).
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(o *options) { o.dialOpts = append(o.dialOpts, opts...) }
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
// Default keepalive settings are applied to both the server and the outbound
// client connections. They can be overridden with WithServerOptions /
// WithDialOptions.
func Listen(addr string, opts ...Option) (*GRPCTransport, error) {
	o := &options{}
	for _, fn := range opts {
		fn(o)
	}

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
	}

	// Keepalive lets either side detect a dead connection quickly even when
	// no RPCs are in flight — important for a Raft cluster where a silent
	// network partition should not stall elections indefinitely.
	defaultServerOpts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// Close idle connections after 15 s of no activity.
			MaxConnectionIdle: 15 * time.Second,
			// Recycle long-lived connections every 30 s (spreads reconnect load).
			MaxConnectionAge: 30 * time.Second,
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

	defaultDialOpts := []grpc.DialOption{
		dialCreds,
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

	t := &GRPCTransport{
		server:   grpc.NewServer(defaultServerOpts...),
		listener: ln,
		dialOpts: defaultDialOpts,
		logger:   slog.Default().With("component", "grpctransport"),
		handlers: make(map[raft.NodeID]raft.Handler),
		peers:    make(map[raft.NodeID]string),
		clients:  make(map[string]*grpc.ClientConn),
	}

	pb.RegisterRaftServiceServer(t.server, &grpcServer{transport: t})
	go t.server.Serve(ln) //nolint:errcheck // Serve always returns a non-nil error after GracefulStop; unactionable here.
	return t, nil
}

// Addr returns the listening address (useful when addr was ":0").
func (t *GRPCTransport) Addr() string {
	return t.listener.Addr().String()
}

// AddPeer registers the address of a remote Raft node. Must be called for
// every peer before RPCs are sent to it.
func (t *GRPCTransport) AddPeer(id raft.NodeID, addr string) {
	t.mu.Lock()
	t.peers[id] = addr
	t.mu.Unlock()
}

// Register implements raft.Transport. It associates a handler with a node ID
// so inbound RPCs addressed to that node are dispatched correctly.
func (t *GRPCTransport) Register(id raft.NodeID, h raft.Handler) {
	t.mu.Lock()
	t.handlers[id] = h
	t.mu.Unlock()
}

// SetGroupLookup installs a function that maps a GroupID to its Handler.
// When set, inbound RPCs are routed by the GroupID embedded in the request
// proto rather than by the x-raft-node-id metadata header. This is the
// primary routing mechanism for multi-Raft deployments; single-group usage
// does not need to call this method.
//
// SetGroupLookup also enables heartbeat batching: outbound pure heartbeats
// (AppendEntries with no entries and no ReadBarrier) are coalesced per-peer
// into a single BatchHeartbeats RPC, reducing cost from O(G×P) to O(P) per
// tick interval.
//
// The lookup function is typically Manager.Lookup.
func (t *GRPCTransport) SetGroupLookup(fn func(uint64) (raft.Handler, bool)) {
	t.mu.Lock()
	t.groupLookup = fn
	if t.hbBatcher == nil {
		t.hbBatcher = newHeartbeatBatcher(t)
	}
	t.mu.Unlock()
}

// Close shuts down the server gracefully and closes all cached client
// connections.
func (t *GRPCTransport) Close() error {
	t.mu.RLock()
	batcher := t.hbBatcher
	t.mu.RUnlock()
	if batcher != nil {
		batcher.stop()
	}
	t.server.GracefulStop()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, cc := range t.clients {
		cc.Close() //nolint:errcheck // best-effort cleanup; error is unactionable during shutdown.
	}
	t.clients = make(map[string]*grpc.ClientConn)
	return nil
}

// ---- client-side helpers ---------------------------------------------------

// clientFor returns a cached (or newly dialled) gRPC connection to the peer.
func (t *GRPCTransport) clientFor(to raft.NodeID) (pb.RaftServiceClient, error) {
	t.mu.RLock()
	addr, ok := t.peers[to]
	t.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("grpctransport: no address registered for peer %q", to)
	}

	t.mu.RLock()
	cc, cached := t.clients[addr]
	t.mu.RUnlock()
	if cached {
		return pb.NewRaftServiceClient(cc), nil
	}

	// Dial outside the lock to avoid blocking other goroutines.
	// grpc.NewClient is non-blocking; the connection is established lazily.
	cc, err := grpc.NewClient(addr, t.dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpctransport: dial %q: %w", addr, err)
	}

	t.mu.Lock()
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

// ---- raft.Transport implementation -----------------------------------------

func (t *GRPCTransport) RequestVote(ctx context.Context, to raft.NodeID, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	c, err := t.clientFor(to)
	if err != nil {
		return nil, err
	}
	pbResp, err := c.RequestVote(ctx, rvReqToProto(req))
	if err != nil {
		return nil, err
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
		batcher := t.hbBatcher
		t.mu.RUnlock()
		if batcher != nil {
			return batcher.Send(ctx, to, req)
		}
	}
	c, err := t.clientFor(to)
	if err != nil {
		return nil, err
	}
	pbResp, err := c.AppendEntries(ctx, aeReqToProto(req))
	if err != nil {
		return nil, err
	}
	return aeRespFromProto(pbResp), nil
}

func (t *GRPCTransport) InstallSnapshot(ctx context.Context, to raft.NodeID, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	c, err := t.clientFor(to)
	if err != nil {
		return nil, err
	}
	pbResp, err := c.InstallSnapshot(ctx, isReqToProto(req))
	if err != nil {
		return nil, err
	}
	return &raft.InstallSnapshotResponse{Term: raft.Term(pbResp.Term)}, nil
}

func (t *GRPCTransport) TimeoutNow(ctx context.Context, to raft.NodeID, req *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	c, err := t.clientFor(to)
	if err != nil {
		return nil, err
	}
	pbResp, err := c.TimeoutNow(ctx, &pb.TimeoutNowRequest{
		Term:     uint64(req.Term),
		LeaderId: string(req.LeaderID),
	})
	if err != nil {
		return nil, err
	}
	return &raft.TimeoutNowResponse{Term: raft.Term(pbResp.Term)}, nil
}

func (t *GRPCTransport) ReadIndex(ctx context.Context, to raft.NodeID, req *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	c, err := t.clientFor(to)
	if err != nil {
		return nil, err
	}
	pbResp, err := c.ReadIndex(ctx, riReqToProto(req))
	if err != nil {
		return nil, err
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
	h, err := s.handlerForGroup(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	resp, err := h.HandleTimeoutNow(ctx, &raft.TimeoutNowRequest{
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
// Unknown groups are skipped with a non-success result rather than failing the
// whole batch, so a late-joiner or just-removed group doesn't disrupt others.
func (s *grpcServer) BatchHeartbeats(ctx context.Context, req *pb.BatchedHeartbeatRequest) (*pb.BatchedHeartbeatResponse, error) {
	s.transport.batchHBServed.Add(1)
	s.transport.batchHBEntries.Add(int64(len(req.Entries)))
	resp := &pb.BatchedHeartbeatResponse{
		Results: make([]*pb.HeartbeatResult, 0, len(req.Entries)),
	}
	for _, entry := range req.Entries {
		h, hErr := s.handlerForGroup(ctx, entry.GroupId)
		if hErr != nil {
			// Not-found is expected for late-joiners / just-removed groups;
			// log at debug. Any other error is unexpected.
			s.transport.logger.LogAttrs(ctx, slog.LevelDebug,
				"BatchHeartbeats: group lookup failed",
				slog.Uint64("group_id", entry.GroupId),
				slog.Any("err", hErr),
			)
			s.transport.batchHBErrors.Add(1)
			resp.Results = append(resp.Results, &pb.HeartbeatResult{
				GroupId: entry.GroupId,
				Success: false,
			})
			continue
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
			resp.Results = append(resp.Results, &pb.HeartbeatResult{
				GroupId: entry.GroupId,
				Success: false,
			})
			continue
		}
		resp.Results = append(resp.Results, &pb.HeartbeatResult{
			GroupId: entry.GroupId,
			Term:    uint64(aeResp.Term),
			Success: aeResp.Success,
		})
	}
	return resp, nil
}
