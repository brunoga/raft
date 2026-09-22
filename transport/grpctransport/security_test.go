package grpctransport_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/transport/grpctransport"
)

// nodeCA issues certificates that name a node, so a test can hand each side of
// a connection an identity and check that the authorizer honours it.
type nodeCA struct {
	cert *x509.Certificate
	key  *rsa.PrivateKey
	pool *x509.CertPool
}

func newNodeCA(t *testing.T) *nodeCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Raft Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &nodeCA{cert: cert, key: key, pool: pool}
}

// tlsConfigFor issues a certificate whose Common Name is nodeID and returns a
// TLS config usable as both a server and a client config.
func (ca *nodeCA) tlsConfigFor(t *testing.T, nodeID string) *tls.Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth,
		},
		DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		RootCAs:      ca.pool,
		ClientCAs:    ca.pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS12,
	}
}

// TestPeerAuthorizer_RejectsMismatchedNodeID verifies that a peer which
// completes the TLS handshake but claims to be a different node is rejected.
//
// Authentication is not authorization. Every node in a cluster trusts the same
// CA, so without this check any node — or anything else holding a certificate
// from that CA — can send an AppendEntries with a huge term and force the
// whole cluster to step down, or send a TimeoutNow to trigger an election.
func TestPeerAuthorizer_RejectsMismatchedNodeID(t *testing.T) {
	ca := newNodeCA(t)

	srv, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithTLSConfig(ca.tlsConfigFor(t, "server")),
		grpctransport.WithPeerAuthorizer(grpctransport.MTLSPeerAuthorizer(nil)),
	)
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	srv.Register("srv", newRecordingHandler())

	// The client's certificate says "impostor", so only that node ID is
	// authorized for it.
	cli, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithTLSConfig(ca.tlsConfigFor(t, "impostor")))
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	defer func() { _ = cli.Close() }()
	cli.AddPeer("srv", srv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Claiming to be the legitimate leader must be refused.
	_, err = cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
		Term: 99, LeaderID: "leader",
		Entries: []raft.LogEntry{{Index: 1, Term: 99}},
	})
	if err == nil {
		t.Fatal("a peer impersonating another node was accepted")
	}
	if !errors.Is(err, grpctransport.ErrUnauthorizedPeer) {
		t.Errorf("error %v does not match ErrUnauthorizedPeer", err)
	}

	// TimeoutNow is just as dangerous and must be refused the same way.
	_, err = cli.TimeoutNow(ctx, "srv", &raft.TimeoutNowRequest{Term: 99, LeaderID: "leader"})
	if !errors.Is(err, grpctransport.ErrUnauthorizedPeer) {
		t.Errorf("TimeoutNow: error %v does not match ErrUnauthorizedPeer", err)
	}

	// RequestVote carries a CandidateID rather than a LeaderID.
	_, err = cli.RequestVote(ctx, "srv", &raft.RequestVoteRequest{Term: 99, CandidateID: "leader"})
	if !errors.Is(err, grpctransport.ErrUnauthorizedPeer) {
		t.Errorf("RequestVote: error %v does not match ErrUnauthorizedPeer", err)
	}
}

// TestPeerAuthorizer_AcceptsMatchingNodeID verifies the authorizer lets a peer
// through when the claimed node ID matches its verified certificate.
func TestPeerAuthorizer_AcceptsMatchingNodeID(t *testing.T) {
	ca := newNodeCA(t)

	srv, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithTLSConfig(ca.tlsConfigFor(t, "server")),
		grpctransport.WithPeerAuthorizer(grpctransport.MTLSPeerAuthorizer(nil)),
	)
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	h := newRecordingHandler()
	srv.Register("srv", h)

	cli, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithTLSConfig(ca.tlsConfigFor(t, "leader")))
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	defer func() { _ = cli.Close() }()
	cli.AddPeer("srv", srv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
		Term: 1, LeaderID: "leader",
		Entries: []raft.LogEntry{{Index: 1, Term: 1}},
	}); err != nil {
		t.Fatalf("AppendEntries from the genuine leader: %v", err)
	}
	select {
	case <-h.appends:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never saw the authorized request")
	}
}

// TestPeerAuthorizer_AppliesToBatchedHeartbeats verifies that batched
// heartbeats are authorized per entry, so the batching path cannot be used to
// bypass the check the unbatched path enforces.
func TestPeerAuthorizer_AppliesToBatchedHeartbeats(t *testing.T) {
	ca := newNodeCA(t)

	recv, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithTLSConfig(ca.tlsConfigFor(t, "server")),
		grpctransport.WithPeerAuthorizer(grpctransport.MTLSPeerAuthorizer(nil)),
	)
	if err != nil {
		t.Fatalf("Listen recv: %v", err)
	}
	defer func() { _ = recv.Close() }()
	h := newRecordingHandler()
	recv.SetGroupLookup(func(gid uint64) (raft.Handler, bool) { return h, gid == 1 })

	send, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithTLSConfig(ca.tlsConfigFor(t, "impostor")))
	if err != nil {
		t.Fatalf("Listen send: %v", err)
	}
	defer func() { _ = send.Close() }()
	send.AddPeer("recv", recv.Addr())
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
		GroupID: 1, Term: 99, LeaderID: "leader",
	})
	if err == nil {
		t.Fatal("an unauthorized batched heartbeat was accepted")
	}
	select {
	case req := <-h.appends:
		t.Errorf("the handler was reached by an unauthorized heartbeat: %+v", req)
	default:
	}
}

// TestMTLSPeerAuthorizer_RejectsPlaintextPeer verifies that the mTLS
// authorizer refuses a connection with no verified certificate, instead of
// accepting everything when TLS is accidentally left off.
func TestMTLSPeerAuthorizer_RejectsPlaintextPeer(t *testing.T) {
	srv, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithPeerAuthorizer(grpctransport.MTLSPeerAuthorizer(nil)), grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	srv.Register("srv", newRecordingHandler())

	cli, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	defer func() { _ = cli.Close() }()
	cli.AddPeer("srv", srv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
		Term: 1, LeaderID: "leader",
		Entries: []raft.LogEntry{{Index: 1, Term: 1}},
	})
	if !errors.Is(err, grpctransport.ErrUnauthorizedPeer) {
		t.Fatalf("error %v does not match ErrUnauthorizedPeer", err)
	}
}

// TestMTLSPeerAuthorizer_CustomIdentities verifies that the identities hook
// can derive node IDs from the certificate in a deployment-specific way.
func TestMTLSPeerAuthorizer_CustomIdentities(t *testing.T) {
	ca := newNodeCA(t)

	authorizer := grpctransport.MTLSPeerAuthorizer(func(c *x509.Certificate) []string {
		return []string{strings.TrimSuffix(c.Subject.CommonName, ".raft.example")}
	})
	srv, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithTLSConfig(ca.tlsConfigFor(t, "server")),
		grpctransport.WithPeerAuthorizer(authorizer),
	)
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	h := newRecordingHandler()
	srv.Register("srv", h)

	cli, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithTLSConfig(ca.tlsConfigFor(t, "n1.raft.example")))
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	defer func() { _ = cli.Close() }()
	cli.AddPeer("srv", srv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
		Term: 1, LeaderID: "n1",
		Entries: []raft.LogEntry{{Index: 1, Term: 1}},
	}); err != nil {
		t.Fatalf("AppendEntries with a derived identity: %v", err)
	}
}

// TestStrictRequestValidation_RejectsEmptyNodeID verifies that a request
// claiming no identity at all is refused when strict validation is on. An
// anonymous request cannot be authorized, so accepting one would leave a hole
// next to the authorizer.
func TestStrictRequestValidation_RejectsEmptyNodeID(t *testing.T) {
	cli, srv := pairedTransports(t, grpctransport.WithStrictRequestValidation())
	h := newRecordingHandler()
	srv.Register("srv", h)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
		Term:    1,
		Entries: []raft.LogEntry{{Index: 1, Term: 1}},
	}); err == nil {
		t.Error("AppendEntries with an empty LeaderID was accepted")
	}
	if _, err := cli.RequestVote(ctx, "srv", &raft.RequestVoteRequest{Term: 1}); err == nil {
		t.Error("RequestVote with an empty CandidateID was accepted")
	}

	// A well-formed request must still get through.
	if _, err := cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
		Term: 1, LeaderID: "leader",
		Entries: []raft.LogEntry{{Index: 1, Term: 1}},
	}); err != nil {
		t.Errorf("AppendEntries with a LeaderID: %v", err)
	}
}

// TestRequestValidation_RejectsOverlongNodeID verifies that an absurdly long
// node ID is rejected even with no authorizer configured. Node IDs are
// operator-assigned host names; a multi-kilobyte one is malformed under any
// configuration and should not reach a handler.
func TestRequestValidation_RejectsOverlongNodeID(t *testing.T) {
	cli, srv := pairedTransports(t)
	h := newRecordingHandler()
	srv.Register("srv", h)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
		Term:     1,
		LeaderID: raft.NodeID(strings.Repeat("x", 4096)),
		Entries:  []raft.LogEntry{{Index: 1, Term: 1}},
	})
	if err == nil {
		t.Fatal("an over-long LeaderID was accepted")
	}
	select {
	case req := <-h.appends:
		t.Errorf("the handler was reached by a malformed request: LeaderID is %d bytes", len(req.LeaderID))
	default:
	}
}

// TestPeerAuthorizer_HookReceivesClaimedNodeID verifies that a custom
// authorizer is handed the exact node ID the request claims, for every RPC.
func TestPeerAuthorizer_HookReceivesClaimedNodeID(t *testing.T) {
	seen := make(chan string, 8)
	authorizer := func(_ context.Context, claimed string) error {
		seen <- claimed
		return nil
	}

	srv, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithPeerAuthorizer(authorizer), grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	srv.Register("srv", newRecordingHandler())

	cli, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	defer func() { _ = cli.Close() }()
	cli.AddPeer("srv", srv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
		Term: 1, LeaderID: "the-leader",
		Entries: []raft.LogEntry{{Index: 1, Term: 1}},
	}); err != nil {
		t.Fatalf("AppendEntries: %v", err)
	}
	if got := <-seen; got != "the-leader" {
		t.Errorf("AppendEntries authorizer saw %q, want %q", got, "the-leader")
	}

	if _, err := cli.RequestVote(ctx, "srv", &raft.RequestVoteRequest{
		Term: 1, CandidateID: "the-candidate",
	}); err != nil {
		t.Fatalf("RequestVote: %v", err)
	}
	if got := <-seen; got != "the-candidate" {
		t.Errorf("RequestVote authorizer saw %q, want %q", got, "the-candidate")
	}

	if _, err := cli.InstallSnapshot(ctx, "srv", &raft.InstallSnapshotRequest{
		Term: 1, LeaderID: "the-leader", Done: true,
	}); err != nil {
		t.Fatalf("InstallSnapshot: %v", err)
	}
	if got := <-seen; got != "the-leader" {
		t.Errorf("InstallSnapshot authorizer saw %q, want %q", got, "the-leader")
	}
}
