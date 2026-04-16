package grpctransport_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/grpctransport"
)

// selfSignedTLS returns a *tls.Config usable for both server and client in a
// test (self-signed CA, mTLS). Valid for "localhost" and "127.0.0.1".
func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "raft-test"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ServerName:   "localhost",
	}
}

const electionTimeout = 5 * time.Second

// ---- minimal key-value state machine ---------------------------------------

type kvSM struct {
	mu   sync.RWMutex
	data map[string]string
}

func (sm *kvSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	if len(e.Command) == 0 {
		return nil, nil
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for i, b := range e.Command {
		if b == '=' {
			key := string(e.Command[:i])
			val := string(e.Command[i+1:])
			sm.data[key] = val
			return []byte(val), nil
		}
	}
	return []byte(sm.data[string(e.Command)]), nil
}

func (sm *kvSM) Get(key string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.data[key]
}

func (sm *kvSM) Snapshot(_ context.Context) ([]byte, error) { return nil, nil }
func (sm *kvSM) Restore(_ context.Context, _ raft.SnapshotMeta, _ []byte) error {
	return nil
}

// ---- cluster helpers -------------------------------------------------------

type grpcCluster struct {
	t          *testing.T
	nodes      []*raft.Node
	transports []*grpctransport.GRPCTransport
	sms        []*kvSM
	ids        []raft.NodeID
}

func newGRPCCluster(t *testing.T, n int) *grpcCluster {
	t.Helper()

	ids := make([]raft.NodeID, n)
	for i := range n {
		ids[i] = raft.NodeID(fmt.Sprintf("n%d", i+1))
	}

	// Phase 1: create all transports bound to random ports.
	transports := make([]*grpctransport.GRPCTransport, n)
	addrs := make([]string, n)
	for i := range n {
		tr, err := grpctransport.Listen(":0")
		if err != nil {
			t.Fatalf("Listen node %s: %v", ids[i], err)
		}
		transports[i] = tr
		addrs[i] = tr.Addr()
	}

	// Phase 2: tell every transport about every peer's address.
	for i := range n {
		for j := range n {
			if i != j {
				transports[i].AddPeer(ids[j], addrs[j])
			}
		}
	}

	// Phase 3: create and start nodes.
	c := &grpcCluster{t: t, ids: ids, transports: transports}
	for i := range n {
		peers := make([]raft.NodeID, 0, n-1)
		for j, id := range ids {
			if j != i {
				peers = append(peers, id)
			}
		}
		sm := &kvSM{data: make(map[string]string)}
		cfg := raft.DefaultConfig()
		cfg.ID = ids[i]
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = sm
		cfg.Transport = transports[i]
		cfg.TickInterval = 10 * time.Millisecond // real-time ticks for gRPC tests

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("New node %s: %v", ids[i], err)
		}
		transports[i].Register(ids[i], node)
		c.nodes = append(c.nodes, node)
		c.sms = append(c.sms, sm)
	}

	for _, node := range c.nodes {
		node.Start()
	}

	t.Cleanup(func() {
		for _, node := range c.nodes {
			node.Stop()
		}
		for _, tr := range c.transports {
			tr.Close() //nolint:errcheck // best-effort cleanup in test teardown.
		}
	})

	return c
}

// waitLeader blocks until a single leader is elected or the test times out.
func (c *grpcCluster) waitLeader() *raft.Node {
	c.t.Helper()
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		var leader *raft.Node
		count := 0
		for _, n := range c.nodes {
			if n.StateSnapshot() == raft.Leader {
				leader = n
				count++
			}
		}
		if count == 1 {
			return leader
		}
	}
	c.t.Fatal("no leader elected within timeout")
	return nil
}

// propose submits cmd to the current leader and waits for the result.
func (c *grpcCluster) propose(cmd []byte) ([]byte, error) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()

	var leader *raft.Node
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		for _, n := range c.nodes {
			if n.StateSnapshot() == raft.Leader {
				leader = n
				break
			}
		}
		if leader != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if leader == nil {
		return nil, fmt.Errorf("no leader")
	}

	return leader.Propose(ctx, cmd)
}

// ---- tests -----------------------------------------------------------------

func TestGRPC_LeaderElected(t *testing.T) {
	c := newGRPCCluster(t, 3)
	leader := c.waitLeader()
	if leader == nil {
		t.Fatal("no leader")
	}
	// Exactly one leader.
	count := 0
	for _, n := range c.nodes {
		if n.StateSnapshot() == raft.Leader {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 leader, got %d", count)
	}
}

func TestGRPC_ProposeAndApply(t *testing.T) {
	c := newGRPCCluster(t, 3)
	c.waitLeader()

	val, err := c.propose([]byte("color=green"))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if string(val) != "green" {
		t.Fatalf("expected 'green', got %q", val)
	}

	// Wait for all state machines to reflect the write.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		all := true
		for _, sm := range c.sms {
			if sm.Get("color") != "green" {
				all = false
				break
			}
		}
		if all {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i, sm := range c.sms {
		t.Errorf("node %d: color=%q, want 'green'", i+1, sm.Get("color"))
	}
}

func TestGRPC_MultipleProposals(t *testing.T) {
	c := newGRPCCluster(t, 3)
	c.waitLeader()

	cmds := []string{"a=1", "b=2", "c=3"}
	for _, cmd := range cmds {
		if _, err := c.propose([]byte(cmd)); err != nil {
			t.Fatalf("Propose %q: %v", cmd, err)
		}
	}

	// All state machines must converge.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		ok := true
		for _, sm := range c.sms {
			if sm.Get("a") != "1" || sm.Get("b") != "2" || sm.Get("c") != "3" {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i, sm := range c.sms {
		t.Errorf("node %d: a=%s b=%s c=%s", i+1, sm.Get("a"), sm.Get("b"), sm.Get("c"))
	}
}

func TestGRPC_FiveNodeCluster(t *testing.T) {
	c := newGRPCCluster(t, 5)
	c.waitLeader()

	if _, err := c.propose([]byte("x=42")); err != nil {
		t.Fatalf("Propose: %v", err)
	}
}

func TestGRPC_ReelectAfterLeaderStop(t *testing.T) {
	c := newGRPCCluster(t, 3)
	leader := c.waitLeader()

	// Stop the leader.
	leader.Stop()

	// A new leader must be elected among the remaining two... but wait, 2/3 is
	// still a quorum so a new election succeeds.
	deadline := time.Now().Add(electionTimeout)
	var newLeader *raft.Node
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		for _, n := range c.nodes {
			if n != leader && n.StateSnapshot() == raft.Leader {
				newLeader = n
				break
			}
		}
		if newLeader != nil {
			break
		}
	}
	if newLeader == nil {
		t.Fatal("no new leader after stopping old leader")
	}
}

func TestGRPC_ReadIndex(t *testing.T) {
	c := newGRPCCluster(t, 3)
	leader := c.waitLeader()

	if _, err := c.propose([]byte("k=v")); err != nil {
		t.Fatalf("propose: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()

	idx, err := leader.ReadIndex(ctx)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if idx == 0 {
		t.Fatal("ReadIndex returned 0")
	}
}

// TestGRPC_TLS_RejectsMissingClientCert verifies that a server configured with
// mTLS (RequireAndVerifyClientCert) refuses connections from a client that
// presents no certificate.
func TestGRPC_TLS_RejectsMissingClientCert(t *testing.T) {
	tlsCfg := selfSignedTLS(t)

	// TLS server.
	tlsServer, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithTLSConfig(tlsCfg))
	if err != nil {
		t.Fatalf("Listen TLS server: %v", err)
	}
	defer func() { _ = tlsServer.Close() }()

	// Insecure (plaintext) client transport.
	plainClient, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen plain client: %v", err)
	}
	defer func() { _ = plainClient.Close() }()
	plainClient.AddPeer("srv", tlsServer.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = plainClient.RequestVote(ctx, "srv", &raft.RequestVoteRequest{})
	if err == nil {
		t.Fatal("expected TLS rejection error, got nil")
	}
}

// TestGRPC_TLS verifies that a 3-node cluster works end-to-end with mutual TLS.
func TestGRPC_TLS(t *testing.T) {
	tlsCfg := selfSignedTLS(t)
	opt := grpctransport.WithTLSConfig(tlsCfg)

	ids := []raft.NodeID{"n1", "n2", "n3"}
	transports := make([]*grpctransport.GRPCTransport, 3)
	addrs := make([]string, 3)
	for i := range 3 {
		tr, err := grpctransport.Listen("127.0.0.1:0", opt)
		if err != nil {
			t.Fatalf("Listen node %s: %v", ids[i], err)
		}
		transports[i] = tr
		addrs[i] = tr.Addr()
	}
	for i := range 3 {
		for j := range 3 {
			if i != j {
				transports[i].AddPeer(ids[j], addrs[j])
			}
		}
	}

	var nodes []*raft.Node
	var sms []*kvSM
	for i := range 3 {
		peers := make([]raft.NodeID, 0, 2)
		for j, id := range ids {
			if j != i {
				peers = append(peers, id)
			}
		}
		sm := &kvSM{data: make(map[string]string)}
		cfg := raft.DefaultConfig()
		cfg.ID = ids[i]
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = sm
		cfg.Transport = transports[i]
		cfg.TickInterval = 10 * time.Millisecond
		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("New node %s: %v", ids[i], err)
		}
		transports[i].Register(ids[i], node)
		nodes = append(nodes, node)
		sms = append(sms, sm)
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			n.Stop()
		}
		for _, tr := range transports {
			tr.Close() //nolint:errcheck // best-effort cleanup in test teardown.
		}
	})
	for _, n := range nodes {
		n.Start()
	}

	// Wait for a leader.
	deadline := time.Now().Add(electionTimeout)
	var leader *raft.Node
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.StateSnapshot() == raft.Leader {
				leader = n
				break
			}
		}
		if leader != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if leader == nil {
		t.Fatal("no leader elected")
	}

	// Propose a write and verify it reaches all nodes.
	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	if _, err := leader.Propose(ctx, []byte("tls=ok")); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	deadline = time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		all := true
		for _, sm := range sms {
			if sm.Get("tls") != "ok" {
				all = false
				break
			}
		}
		if all {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i, sm := range sms {
		t.Errorf("node %d: tls=%q, want 'ok'", i+1, sm.Get("tls"))
	}
}

// TestGRPC_GroupIDStamped verifies that GroupID set in Config is carried on the
// wire. A 3-node cluster is formed with GroupID=42; the test intercepts an
// AppendEntries RPC at the handler level and checks that the GroupID field
// equals 42. This exercises both the node-stamping (Step 2) and the
// proto↔Go conversion (Step 1).
func TestGRPC_GroupIDStamped(t *testing.T) {
	const wantGroupID uint64 = 42

	ids := []raft.NodeID{"g1", "g2", "g3"}

	transports := make([]*grpctransport.GRPCTransport, 3)
	addrs := make([]string, 3)
	for i := range 3 {
		tr, err := grpctransport.Listen(":0")
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		t.Cleanup(func() { tr.Close() }) //nolint:errcheck
		transports[i] = tr
		addrs[i] = tr.Addr()
	}
	for i := range 3 {
		for j := range 3 {
			if i != j {
				transports[i].AddPeer(ids[j], addrs[j])
			}
		}
	}

	// groupIDCapture wraps a real node and records the GroupID seen on the
	// first AppendEntries call. It forwards all RPCs to the underlying node.
	type capture struct {
		raft.Handler
		once     sync.Once
		observed chan uint64
	}

	captures := make([]*capture, 3)
	nodes := make([]*raft.Node, 3)
	for i := range 3 {
		peers := make([]raft.NodeID, 0, 2)
		for j, id := range ids {
			if j != i {
				peers = append(peers, id)
			}
		}
		sm := &kvSM{data: make(map[string]string)}
		cfg := raft.DefaultConfig()
		cfg.ID = ids[i]
		cfg.GroupID = wantGroupID
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = sm
		cfg.Transport = transports[i]
		cfg.TickInterval = 10 * time.Millisecond
		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		nodes[i] = node
		captures[i] = &capture{Handler: node, observed: make(chan uint64, 1)}
		transports[i].Register(ids[i], captures[i])
	}
	// Implement AppendEntries capture.
	for _, c := range captures {
		c := c
		orig := c.Handler
		c.Handler = handlerFunc{
			handleAppendEntries: func(ctx context.Context, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
				c.once.Do(func() { c.observed <- req.GroupID })
				return orig.HandleAppendEntries(ctx, req)
			},
			Handler: orig,
		}
	}

	for _, n := range nodes {
		n.Start()
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			n.Stop()
		}
	})

	// Merge all observed channels into one; the leader never receives
	// AppendEntries in normal operation so we only need one observation.
	merged := make(chan uint64, len(captures))
	for _, c := range captures {
		c := c
		go func() {
			if v, ok := <-c.observed; ok {
				merged <- v
			}
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	select {
	case got := <-merged:
		if got != wantGroupID {
			t.Errorf("AppendEntries.GroupID = %d, want %d", got, wantGroupID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for an AppendEntries with GroupID")
	}
}

// handlerFunc lets individual Handle* methods be overridden for testing.
type handlerFunc struct {
	raft.Handler
	handleAppendEntries func(context.Context, *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error)
}

func (h handlerFunc) HandleAppendEntries(ctx context.Context, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	if h.handleAppendEntries != nil {
		return h.handleAppendEntries(ctx, req)
	}
	return h.Handler.HandleAppendEntries(ctx, req)
}

// ---- TestGRPC_SetGroupLookup ------------------------------------------------

// signalHandler is a minimal raft.Handler that fires a channel on every
// HandleAppendEntries call. All other methods return zero-value responses.
type signalHandler struct {
	called chan struct{}
}

func (h *signalHandler) HandleRequestVote(_ context.Context, _ *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	return &raft.RequestVoteResponse{}, nil
}
func (h *signalHandler) HandleAppendEntries(_ context.Context, _ *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	select {
	case h.called <- struct{}{}:
	default:
	}
	return &raft.AppendEntriesResponse{}, nil
}
func (h *signalHandler) HandleInstallSnapshot(_ context.Context, _ *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	return &raft.InstallSnapshotResponse{}, nil
}
func (h *signalHandler) HandleTimeoutNow(_ context.Context, _ *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	return &raft.TimeoutNowResponse{}, nil
}
func (h *signalHandler) HandleReadIndex(_ context.Context, _ *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	return &raft.ReadIndexResponse{}, nil
}

// TestGRPC_SetGroupLookup verifies that SetGroupLookup routes inbound RPCs to
// the correct handler based on the GroupID carried in the request proto.
func TestGRPC_SetGroupLookup(t *testing.T) {
	srv, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	defer srv.Close()

	h1 := &signalHandler{called: make(chan struct{}, 1)}
	h2 := &signalHandler{called: make(chan struct{}, 1)}

	groups := map[uint64]raft.Handler{1: h1, 2: h2}
	srv.SetGroupLookup(func(gid uint64) (raft.Handler, bool) {
		h, ok := groups[gid]
		return h, ok
	})

	cli, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	defer cli.Close()
	cli.AddPeer("srv", srv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Request for group 1 must reach h1.
	if _, err := cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{GroupID: 1, Term: 1}); err != nil {
		t.Fatalf("AppendEntries group 1: %v", err)
	}
	select {
	case <-h1.called:
	case <-ctx.Done():
		t.Fatal("group 1 handler not called")
	}

	// Request for group 2 must reach h2, not h1.
	if _, err := cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{GroupID: 2, Term: 1}); err != nil {
		t.Fatalf("AppendEntries group 2: %v", err)
	}
	select {
	case <-h2.called:
	case <-ctx.Done():
		t.Fatal("group 2 handler not called")
	}
	// h1 must not have been called a second time.
	select {
	case <-h1.called:
		t.Fatal("group 1 handler called for a group-2 request")
	default:
	}
}

// ---- TestGRPC_GroupIDZeroRejectedInMultiRaftMode ----------------------------

// TestGRPC_GroupIDZeroRejectedInMultiRaftMode verifies that a request with
// GroupID==0 is rejected with InvalidArgument when SetGroupLookup is
// installed, rather than silently falling through to NodeID-header routing.
func TestGRPC_GroupIDZeroRejectedInMultiRaftMode(t *testing.T) {
	recv, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer recv.Close()

	recv.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	send, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen send: %v", err)
	}
	defer send.Close()
	send.AddPeer("recv", recv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// GroupID==0 with group lookup installed must return an error.
	_, err = send.RequestVote(ctx, "recv", &raft.RequestVoteRequest{GroupID: 0})
	if err == nil {
		t.Fatal("expected error for GroupID==0 in multi-Raft mode, got nil")
	}
	if !strings.Contains(err.Error(), "GroupID must be non-zero") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ---- TestGRPC_HeartbeatBatching ---------------------------------------------

// TestGRPC_HeartbeatBatching verifies that when SetGroupLookup is installed,
// pure heartbeats from G groups to the same peer are collapsed into a single
// BatchHeartbeats RPC instead of G individual AppendEntries calls.
//
// Setup: two GRPCTransports (sender, receiver). The receiver has 10 groups
// registered via SetGroupLookup. We use a barrier to ensure all 10 sender
// goroutines are ready before any send fires, then assert:
//   - All 10 group handlers on the receiver receive a heartbeat (correctness).
//   - BatchHeartbeatsServed() == 1 on the receiver (strict O(P) assertion).
//   - BatchHeartbeatEntriesServed() == numGroups.
func TestGRPC_HeartbeatBatching(t *testing.T) {
	const numGroups = 10 // large enough that O(G×P) vs O(P) is unambiguous

	recv, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen recv: %v", err)
	}
	defer recv.Close()

	// One signalHandler per group; each fires its channel when HandleAppendEntries is called.
	handlers := make(map[uint64]*signalHandler, numGroups)
	for g := range numGroups {
		handlers[uint64(g+1)] = &signalHandler{called: make(chan struct{}, 1)}
	}
	recv.SetGroupLookup(func(gid uint64) (raft.Handler, bool) {
		h, ok := handlers[gid]
		return h, ok
	})

	send, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen send: %v", err)
	}
	defer send.Close()
	send.AddPeer("recv", recv.Addr())
	// SetGroupLookup on the sender enables heartbeat batching.
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Barrier: all goroutines park here until every one is ready, so all sends
	// hit the batcher within the same 1ms collection window.
	ready := make(chan struct{})

	var wg sync.WaitGroup
	for g := range numGroups {
		wg.Add(1)
		go func(gid uint64) {
			defer wg.Done()
			<-ready // wait for all goroutines to be scheduled
			if _, err := send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
				GroupID: gid,
				Term:    1,
			}); err != nil {
				t.Errorf("AppendEntries group %d: %v", gid, err)
			}
		}(uint64(g + 1))
	}
	close(ready) // release all goroutines simultaneously
	wg.Wait()

	// Every group's handler must have been called.
	for gid, h := range handlers {
		select {
		case <-h.called:
		default:
			t.Errorf("group %d handler was not called", gid)
		}
	}

	// All 10 concurrent calls must have collapsed into exactly 1 RPC.
	served := recv.BatchHeartbeatsServed()
	if served != 1 {
		t.Errorf("BatchHeartbeatsServed = %d, want 1 (all %d groups must batch into one RPC)",
			served, numGroups)
	}
	if entries := recv.BatchHeartbeatEntriesServed(); entries != int64(numGroups) {
		t.Errorf("BatchHeartbeatEntriesServed = %d, want %d", entries, numGroups)
	}
	t.Logf("BatchHeartbeats RPCs: %d for %d groups (%.0f%% reduction)",
		served, numGroups, 100*(1-float64(served)/float64(numGroups)))
}

// ---- TestGRPC_HeartbeatObservabilityCounters ---------------------------------

// TestGRPC_HeartbeatObservabilityCounters verifies that BatchHeartbeatEntriesServed
// and BatchHeartbeatErrors are updated correctly:
//   - Entries counter reflects the total individual heartbeats dispatched.
//   - Errors counter increments for unknown groups, not for successful ones.
func TestGRPC_HeartbeatObservabilityCounters(t *testing.T) {
	recv, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer recv.Close()

	// Register groups 1 and 2; leave group 3 unknown to trigger an error counter.
	known := map[uint64]*signalHandler{
		1: {called: make(chan struct{}, 1)},
		2: {called: make(chan struct{}, 1)},
	}
	recv.SetGroupLookup(func(gid uint64) (raft.Handler, bool) {
		h, ok := known[gid]
		return h, ok
	})

	send, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen sender: %v", err)
	}
	defer send.Close()
	send.AddPeer("recv", recv.Addr())
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Send heartbeats to groups 1, 2 (known) and 3 (unknown) — all in one batch.
	var wg sync.WaitGroup
	for _, gid := range []uint64{1, 2, 3} {
		wg.Add(1)
		go func(g uint64) {
			defer wg.Done()
			//nolint:errcheck // group 3 will return success=false; that's expected
			send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{GroupID: g, Term: 1})
		}(gid)
	}
	wg.Wait()

	if got := recv.BatchHeartbeatEntriesServed(); got != 3 {
		t.Errorf("BatchHeartbeatEntriesServed = %d, want 3", got)
	}
	if got := recv.BatchHeartbeatErrors(); got != 1 {
		t.Errorf("BatchHeartbeatErrors = %d, want 1 (unknown group 3)", got)
	}
}

// ---- TestGRPC_HeartbeatWindowOption -----------------------------------------

// TestGRPC_HeartbeatWindowOption verifies that WithHeartbeatWindow is
// accepted and that the batcher still coalesces concurrent heartbeats
// (correctness smoke-test; the window value itself is an implementation
// detail not directly observable from the outside).
func TestGRPC_HeartbeatWindowOption(t *testing.T) {
	const numGroups = 5

	// Use a deliberately large window to confirm the option is wired through
	// without panicking or altering correctness.
	recv, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithHeartbeatWindow(10*time.Millisecond))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer recv.Close()

	handlers := make(map[uint64]*signalHandler, numGroups)
	for g := range numGroups {
		handlers[uint64(g+1)] = &signalHandler{called: make(chan struct{}, 1)}
	}
	recv.SetGroupLookup(func(gid uint64) (raft.Handler, bool) {
		h, ok := handlers[gid]
		return h, ok
	})

	send, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithHeartbeatWindow(10*time.Millisecond))
	if err != nil {
		t.Fatalf("Listen send: %v", err)
	}
	defer send.Close()
	send.AddPeer("recv", recv.Addr())
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ready := make(chan struct{})
	var wg sync.WaitGroup
	for g := range numGroups {
		wg.Add(1)
		go func(gid uint64) {
			defer wg.Done()
			<-ready
			send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{GroupID: gid, Term: 1}) //nolint:errcheck
		}(uint64(g + 1))
	}
	close(ready)
	wg.Wait()

	for gid, h := range handlers {
		select {
		case <-h.called:
		default:
			t.Errorf("group %d handler not called with custom window", gid)
		}
	}
	if served := recv.BatchHeartbeatsServed(); served == 0 {
		t.Error("BatchHeartbeats was never called")
	}
}

// ---- TestGRPC_HeartbeatRPCTimeoutOption -------------------------------------

// TestGRPC_HeartbeatRPCTimeoutOption verifies that WithHeartbeatRPCTimeout is
// wired through: when the receiver is unreachable and the timeout is very
// short, BatchHeartbeats must fail quickly instead of hanging for 5s.
func TestGRPC_HeartbeatRPCTimeoutOption(t *testing.T) {
	// Sender configured with a 50ms RPC timeout.
	send, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithHeartbeatRPCTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("Listen send: %v", err)
	}
	defer send.Close()

	// Register a peer address that nobody is listening on.
	send.AddPeer("dead", "127.0.0.1:1") // port 1 is unroutable on localhost
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err = send.AppendEntries(ctx, "dead", &raft.AppendEntriesRequest{GroupID: 1, Term: 1})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error sending to unreachable peer, got nil")
	}
	// Must fail well within the 5s default; the 50ms option should bound it.
	if elapsed > time.Second {
		t.Errorf("heartbeat took %v to fail — WithHeartbeatRPCTimeout not applied", elapsed)
	}
	t.Logf("failed in %v (timeout=50ms): %v", elapsed, err)
}

// ---- TestGRPC_HeartbeatChannelSizeOption ------------------------------------

// TestGRPC_HeartbeatChannelSizeOption verifies that WithHeartbeatChannelSize
// is wired through: a small channel size still admits up to that many
// concurrent heartbeats without blocking or panicking.
func TestGRPC_HeartbeatChannelSizeOption(t *testing.T) {
	const numGroups = 4
	const chanSize = 8 // small but larger than numGroups

	recv, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen recv: %v", err)
	}
	defer recv.Close()

	handlers := make(map[uint64]*signalHandler, numGroups)
	for g := range numGroups {
		handlers[uint64(g+1)] = &signalHandler{called: make(chan struct{}, 1)}
	}
	recv.SetGroupLookup(func(gid uint64) (raft.Handler, bool) {
		h, ok := handlers[gid]
		return h, ok
	})

	send, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithHeartbeatChannelSize(chanSize))
	if err != nil {
		t.Fatalf("Listen send: %v", err)
	}
	defer send.Close()
	send.AddPeer("recv", recv.Addr())
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	ready := make(chan struct{})
	for g := range numGroups {
		wg.Add(1)
		go func(gid uint64) {
			defer wg.Done()
			<-ready
			send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{GroupID: gid, Term: 1}) //nolint:errcheck
		}(uint64(g + 1))
	}
	close(ready)
	wg.Wait()

	for gid, h := range handlers {
		select {
		case <-h.called:
		default:
			t.Errorf("group %d handler not called", gid)
		}
	}
}

// ---- TestGRPC_RemovePeer ----------------------------------------------------

// TestGRPC_RemovePeer verifies that after RemovePeer, outbound RPCs to the
// removed node return an error (address no longer registered), and that a
// subsequent AddPeer + RPC works again.
func TestGRPC_RemovePeer(t *testing.T) {
	recv, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen recv: %v", err)
	}
	defer recv.Close()

	h := &signalHandler{called: make(chan struct{}, 1)}
	recv.Register("recv", h)

	send, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen send: %v", err)
	}
	defer send.Close()
	send.AddPeer("recv", recv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Sanity: RPC works before removal.
	if _, err := send.RequestVote(ctx, "recv", &raft.RequestVoteRequest{}); err != nil {
		t.Fatalf("RequestVote before RemovePeer: %v", err)
	}

	// Remove the peer.
	send.RemovePeer("recv")

	// RPC must now fail with "no address registered".
	_, err = send.RequestVote(ctx, "recv", &raft.RequestVoteRequest{})
	if err == nil {
		t.Fatal("expected error after RemovePeer, got nil")
	}
	if !strings.Contains(err.Error(), "no address registered") {
		t.Errorf("unexpected error: %v", err)
	}

	// Re-adding the peer must restore connectivity.
	send.AddPeer("recv", recv.Addr())
	if _, err := send.RequestVote(ctx, "recv", &raft.RequestVoteRequest{}); err != nil {
		t.Errorf("RequestVote after re-AddPeer: %v", err)
	}
}

// ---- TestGRPC_BatchHeartbeatsBoundedDispatch --------------------------------

// TestGRPC_BatchHeartbeatsBoundedDispatch verifies that BatchHeartbeats
// correctly dispatches all entries and collects all responses even when the
// batch size exceeds GOMAXPROCS (exercising the bounded-semaphore path).
func TestGRPC_BatchHeartbeatsBoundedDispatch(t *testing.T) {
	// Use more groups than GOMAXPROCS to force the semaphore to throttle.
	numGroups := runtime.GOMAXPROCS(0)*4 + 1

	recv, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()

	handlers := make(map[uint64]*signalHandler, numGroups)
	for g := range numGroups {
		handlers[uint64(g+1)] = &signalHandler{called: make(chan struct{}, 1)}
	}
	recv.SetGroupLookup(func(gid uint64) (raft.Handler, bool) {
		h, ok := handlers[gid]
		return h, ok
	})

	send, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer send.Close()
	send.AddPeer("recv", recv.Addr())
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ready := make(chan struct{})
	var wg sync.WaitGroup
	for g := range numGroups {
		wg.Add(1)
		go func(gid uint64) {
			defer wg.Done()
			<-ready
			send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{GroupID: gid, Term: 1}) //nolint:errcheck
		}(uint64(g + 1))
	}
	close(ready)
	wg.Wait()

	// Every group handler must have been called — no entries silently dropped.
	for gid, h := range handlers {
		select {
		case <-h.called:
		default:
			t.Errorf("group %d handler not called (entry dropped or dispatch blocked)", gid)
		}
	}
}

// ---- TestGRPC_HeartbeatBatcherStopWaits ------------------------------------

// TestGRPC_HeartbeatBatcherStopWaits verifies that Close() does not return
// until all peerBatcher goroutines have exited. Before this fix, stop() only
// cancelled the context, leaving goroutines running after Close() returned.
func TestGRPC_HeartbeatBatcherStopWaits(t *testing.T) {
	recv, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()

	send, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	// Enable multi-raft mode — this creates the heartbeat batcher.
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })
	send.AddPeer("recv", recv.Addr())

	// Trigger peerBatcher goroutine creation by sending a heartbeat (empty
	// Entries == heartbeat path in AppendEntries).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{GroupID: 1})
	// Error is expected (recv has no handler for group 1) — we only need the
	// peerBatcher goroutine to have been created and settled.

	before := runtime.NumGoroutine()

	// Close() must block until all peerBatcher goroutines exit.
	closeDone := make(chan struct{})
	go func() {
		send.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not return — peerBatcher goroutines likely leaked")
	}

	// Allow goroutines to fully exit.
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)

	after := runtime.NumGoroutine()
	// Goroutine count must have dropped: at minimum the peerBatcher goroutine
	// created above should have exited.
	if after >= before {
		t.Errorf("goroutine leak: before Close=%d, after=%d (expected drop)", before, after)
	}
}

// ---- TestGRPC_HeartbeatBatcherStoppedFlag -----------------------------------

// TestGRPC_HeartbeatBatcherStoppedFlag verifies that Send returns an error
// (not a panic or hang) when called after Close(). The stopped flag in
// heartbeatBatcher must prevent peerBatcherFor from creating new goroutines
// after stop() has been called.
func TestGRPC_HeartbeatBatcherStoppedFlag(t *testing.T) {
	recv, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	recv.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	send, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })
	send.AddPeer("recv", recv.Addr())

	// Warm up a batcher goroutine.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{GroupID: 1})

	// Close the sender — this must stop the batcher.
	send.Close()

	// Send after close must return an error quickly, not hang or panic.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	_, err = send.AppendEntries(ctx2, "recv", &raft.AppendEntriesRequest{GroupID: 1})
	if err == nil {
		t.Fatal("expected error after Close(), got nil")
	}
}

// ---- TestGRPC_RemovePeer_StopsBatcher ---------------------------------------

// TestGRPC_RemovePeer_StopsBatcher verifies that RemovePeer stops the
// peerBatcher goroutine for the removed peer so it doesn't linger. Before
// this fix, peerBatcher goroutines accumulated indefinitely as peers joined
// and left the cluster.
func TestGRPC_RemovePeer_StopsBatcher(t *testing.T) {
	recv, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	recv.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	send, err := grpctransport.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer send.Close()
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })
	send.AddPeer("recv", recv.Addr())

	// Trigger peerBatcher goroutine creation.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{GroupID: 1})

	before := runtime.NumGoroutine()

	send.RemovePeer("recv")

	// Allow the goroutine to exit.
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)

	after := runtime.NumGoroutine()
	if after >= before {
		t.Errorf("peerBatcher goroutine leaked after RemovePeer: before=%d after=%d", before, after)
	}
}

// ---- TestGRPC_MultiRaft_ManagerWiring ----------------------------------------

// TestGRPC_MultiRaft_ManagerWiring is the canonical integration test for the
// full gRPC + Manager + multi-raft stack.  It verifies end-to-end wiring that
// pure-transport tests and memtransport-based multi-raft tests cannot cover:
//
//   - SetGroupLookup(mgr.Lookup) routes inbound BatchHeartbeats entries to the
//     correct Node via the Manager.
//   - Every group independently elects a leader over real gRPC.
//   - Proposals commit on all groups.
//   - TransferLeadership succeeds (exercises the TimeoutNow GroupId fix: without
//     it the receiver returns codes.InvalidArgument and the transfer fails).
//   - HeartbeatSendBlocked stays zero under normal load, confirming no
//     hbChanSize saturation with 3 groups.
func TestGRPC_MultiRaft_ManagerWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in short mode")
	}

	const (
		numPhysical = 3
		numGroups   = 3
	)

	// ---- Phase 1: create transports and managers ----------------------------

	transports := make([]*grpctransport.GRPCTransport, numPhysical)
	addrs := make([]string, numPhysical)
	managers := make([]*raft.Manager, numPhysical)

	for p := range numPhysical {
		tr, err := grpctransport.Listen("127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen physical %d: %v", p, err)
		}
		transports[p] = tr
		addrs[p] = tr.Addr()
		managers[p] = raft.NewManager()
	}

	// ---- Phase 2: wire AddPeer + SetGroupLookup before nodes start ----------
	//
	// AddPeer must be called before any tick fires an RPC; SetGroupLookup must
	// be called before the first inbound BatchHeartbeats arrives.  Both happen
	// before AddAndStart below.

	for p := range numPhysical {
		for g := range numGroups {
			groupID := uint64(g + 1)
			for pp := range numPhysical {
				if pp == p {
					continue
				}
				// Multiple group NodeIDs on the same physical peer all map to
				// the same address; clientFor reuses the cached connection.
				peerID := raft.NodeID(fmt.Sprintf("g%d-p%d", groupID, pp))
				transports[p].AddPeer(peerID, addrs[pp])
			}
		}
		transports[p].SetGroupLookup(managers[p].Lookup)
	}

	// ---- Phase 3: create and start nodes ------------------------------------

	// nodes[g][p] is the Node for group g+1 on physical node p.
	nodes := make([][]*raft.Node, numGroups)
	for g := range numGroups {
		groupID := uint64(g + 1)
		nodes[g] = make([]*raft.Node, numPhysical)

		// Build the list of all NodeIDs in this group.
		allIDs := make([]raft.NodeID, numPhysical)
		for p := range numPhysical {
			allIDs[p] = raft.NodeID(fmt.Sprintf("g%d-p%d", groupID, p))
		}

		for p := range numPhysical {
			peers := make([]raft.NodeID, 0, numPhysical-1)
			for _, id := range allIDs {
				if id != allIDs[p] {
					peers = append(peers, id)
				}
			}
			sm := &kvSM{data: make(map[string]string)}
			cfg := raft.DefaultConfig()
			cfg.GroupID = groupID
			cfg.ID = allIDs[p]
			cfg.Peers = peers
			cfg.Storage = memstore.New()
			cfg.StateMachine = sm
			cfg.Transport = transports[p] // all groups on one physical node share one transport
			cfg.TickInterval = 10 * time.Millisecond

			node, err := raft.New(&cfg)
			if err != nil {
				t.Fatalf("raft.New g=%d p=%d: %v", g+1, p, err)
			}
			// AddAndStart calls node.Start(), which calls transport.Register.
			// In multi-raft mode groupLookup takes precedence over handlers,
			// so registering multiple NodeIDs on the same transport is harmless.
			if err := managers[p].AddAndStart(groupID, node); err != nil {
				t.Fatalf("managers[%d].AddAndStart(g=%d): %v", p, g+1, err)
			}
			nodes[g][p] = node
		}
	}

	t.Cleanup(func() {
		for _, mgr := range managers {
			mgr.StopAll()
		}
		for _, tr := range transports {
			tr.Close() //nolint:errcheck
		}
	})

	// ---- Phase 4: wait for all groups to elect a leader --------------------

	leaders := make([]*raft.Node, numGroups) // leaders[g] = leader node for group g+1
	leaderPhys := make([]int, numGroups)     // physical index of leader[g]
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		allFound := true
		for g := range numGroups {
			if leaders[g] != nil {
				continue
			}
			for p, n := range nodes[g] {
				if n.StateSnapshot() == raft.Leader {
					leaders[g] = n
					leaderPhys[g] = p
					break
				}
			}
			if leaders[g] == nil {
				allFound = false
			}
		}
		if allFound {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for g, l := range leaders {
		if l == nil {
			t.Fatalf("group %d never elected a leader within %s", g+1, electionTimeout)
		}
		t.Logf("group %d leader: %s (physical %d)", g+1, l.ID(), leaderPhys[g])
	}

	// ---- Phase 5: propose one entry per group --------------------------------

	for g, leader := range leaders {
		ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
		_, err := leader.Propose(ctx, []byte(fmt.Sprintf("key=g%d", g+1)))
		cancel()
		if err != nil {
			t.Errorf("group %d Propose: %v", g+1, err)
		}
	}

	// ---- Phase 6: TransferLeadership on group 1 (exercises TimeoutNow fix) --
	//
	// Without the TimeoutNow GroupId fix the receiver returns
	// codes.InvalidArgument and TransferLeadership returns an error.

	const transferGroup = 0 // group index (0-based) to test on
	leaderNode := leaders[transferGroup]
	leaderP := leaderPhys[transferGroup]

	// Pick a follower as transfer target.
	var transferTarget raft.NodeID
	for p, n := range nodes[transferGroup] {
		if p != leaderP {
			transferTarget = n.ID()
			break
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	if err := leaderNode.TransferLeadership(ctx, transferTarget); err != nil {
		t.Errorf("group 1 TransferLeadership to %s: %v", transferTarget, err)
	}

	// Verify that the target (or at least some other node) became leader.
	deadline = time.Now().Add(electionTimeout)
	transferred := false
	for time.Now().Before(deadline) {
		for p, n := range nodes[transferGroup] {
			if p != leaderP && n.StateSnapshot() == raft.Leader {
				transferred = true
				break
			}
		}
		if transferred {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !transferred {
		t.Errorf("group 1: leadership did not transfer away from physical %d within %s", leaderP, electionTimeout)
	}

	// ---- Phase 7: sanity-check transport metrics ----------------------------
	//
	// 3 groups is well under the default hbChanSize of 1024; the backpressure
	// counter must remain zero throughout the test.
	//
	// BatchHeartbeatsServed must be positive on every transport to confirm that
	// the batching code path (not just direct AppendEntries) was exercised.

	for p, tr := range transports {
		if n := tr.HeartbeatSendBlocked(); n != 0 {
			t.Errorf("physical %d: HeartbeatSendBlocked = %d, want 0 (hbChan saturated under light load)", p, n)
		}
		if n := tr.BatchHeartbeatsServed(); n == 0 {
			t.Errorf("physical %d: BatchHeartbeatsServed = 0, want >0 (batching path not exercised)", p)
		}
	}
}
