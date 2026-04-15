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

// ---- TestGRPC_HeartbeatBatching ---------------------------------------------

// TestGRPC_HeartbeatBatching verifies that when SetGroupLookup is installed,
// pure heartbeats from G groups to the same peer are collapsed into a single
// BatchHeartbeats RPC instead of G individual AppendEntries calls.
//
// Setup: two GRPCTransports (sender, receiver). The receiver has 3 groups
// registered via SetGroupLookup. We concurrently send one pure heartbeat per
// group from the sender and assert:
//   - All 3 group handlers on the receiver receive a heartbeat (correctness).
//   - BatchHeartbeatsServed() == 1 on the receiver (RPC count reduction).
func TestGRPC_HeartbeatBatching(t *testing.T) {
	const numGroups = 3

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

	// Concurrently send one pure heartbeat per group to the same peer.
	var wg sync.WaitGroup
	for g := range numGroups {
		wg.Add(1)
		go func(gid uint64) {
			defer wg.Done()
			if _, err := send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
				GroupID: gid,
				Term:    1,
			}); err != nil {
				t.Errorf("AppendEntries group %d: %v", gid, err)
			}
		}(uint64(g + 1))
	}
	wg.Wait()

	// Every group's handler must have been called.
	for gid, h := range handlers {
		select {
		case <-h.called:
		default:
			t.Errorf("group %d handler was not called", gid)
		}
	}

	// The 3 concurrent calls should have been batched into 1 (or at most 2 if
	// goroutine scheduling split them). The key invariant is O(P) << O(G×P).
	served := recv.BatchHeartbeatsServed()
	if served == 0 {
		t.Error("BatchHeartbeats was never called on receiver")
	}
	if served >= int64(numGroups) {
		t.Errorf("BatchHeartbeats called %d times for %d groups — batching not effective", served, numGroups)
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
