package grpctransport_test

import (
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/transport/grpctransport"
)

// TestListen_RefusesPlaintextByDefault pins that a transport with no TLS
// configuration and no explicit request for plaintext does not start. A Raft
// peer is fully trusted, so an open port is an open cluster, and running that
// way has to be a choice rather than an omission.
func TestListen_RefusesPlaintextByDefault(t *testing.T) {
	tr, err := grpctransport.Listen("127.0.0.1:0")
	if err == nil {
		_ = tr.Close()
		t.Fatal("Listen with no security options succeeded; want ErrNoTransportSecurity")
	}
	if !errors.Is(err, grpctransport.ErrNoTransportSecurity) {
		t.Fatalf("Listen returned %v; want ErrNoTransportSecurity", err)
	}
}

// TestListen_WithInsecureStarts pins that the explicit request is honoured.
func TestListen_WithInsecureStarts(t *testing.T) {
	tr, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen with WithInsecure: %v", err)
	}
	_ = tr.Close()
}

// TestListen_WithCustomCredentialsAddsNone checks that a caller supplying
// credentials through the raw grpc options is neither refused nor given a
// second set of credentials on top of their own, which grpc would reject.
func TestListen_WithCustomCredentialsAddsNone(t *testing.T) {
	tr, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithCustomCredentials(),
		grpctransport.WithServerOptions(grpc.Creds(insecure.NewCredentials())),
		grpctransport.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("Listen with WithCustomCredentials: %v", err)
	}
	defer func() { _ = tr.Close() }()

	// A round trip proves the dial options carried usable credentials.
	peer, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	peer.Register("srv", &recordingHandler{})
	tr.AddPeer("srv", peer.Addr())
	if _, err := tr.RequestVote(t.Context(), "srv", &raft.RequestVoteRequest{Term: 1, CandidateID: "cli"}); err != nil {
		t.Fatalf("RequestVote through custom credentials: %v", err)
	}
}
