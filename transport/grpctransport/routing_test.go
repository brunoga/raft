package grpctransport_test

import (
	"context"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/transport/grpctransport"
)

// TestTimeoutNow_PreservesGroupID verifies that a leadership transfer keeps the
// GroupID it was addressed to.
//
// The request is routed by GroupID, so the field is present on the wire; it was
// simply dropped when the Go request was rebuilt on the server. A handler
// shared by several groups — the normal multi-Raft arrangement — then sees a
// transfer for group 0 and cannot tell which of its groups is meant.
func TestTimeoutNow_PreservesGroupID(t *testing.T) {
	const groupID = 12

	h := newRecordingHandler()
	recv, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen recv: %v", err)
	}
	defer func() { _ = recv.Close() }()
	recv.SetGroupLookup(func(gid uint64) (raft.Handler, bool) { return h, gid == groupID })

	send, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen send: %v", err)
	}
	defer func() { _ = send.Close() }()
	send.AddPeer("recv", recv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := send.TimeoutNow(ctx, "recv", &raft.TimeoutNowRequest{
		GroupID:  groupID,
		Term:     4,
		LeaderID: "leader",
	}); err != nil {
		t.Fatalf("TimeoutNow: %v", err)
	}

	select {
	case got := <-h.timeouts:
		if got.GroupID != groupID {
			t.Errorf("GroupID = %d, want %d", got.GroupID, groupID)
		}
		if got.Term != 4 {
			t.Errorf("Term = %d, want 4", got.Term)
		}
		if got.LeaderID != "leader" {
			t.Errorf("LeaderID = %q, want %q", got.LeaderID, "leader")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never saw the TimeoutNow")
	}
}

// TestNodeIDHeader_RoutesToTheAddressedHandler verifies that a transport
// hosting several nodes delivers each RPC to the node it was addressed to.
//
// Routing falls back to the x-raft-node-id metadata header when no group
// lookup is installed, but the client never stamped the header, so the header
// path could only ever work for a transport with exactly one handler — the
// single case that does not need it. With two handlers registered, every
// inbound RPC failed outright.
func TestNodeIDHeader_RoutesToTheAddressedHandler(t *testing.T) {
	srv, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	defer func() { _ = srv.Close() }()

	h1, h2 := newRecordingHandler(), newRecordingHandler()
	srv.Register("n1", h1)
	srv.Register("n2", h2)

	cli, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	defer func() { _ = cli.Close() }()
	// Both node IDs live behind the same address, as co-located nodes do.
	cli.AddPeer("n1", srv.Addr())
	cli.AddPeer("n2", srv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := func(term raft.Term) *raft.AppendEntriesRequest {
		return &raft.AppendEntriesRequest{
			Term: term, LeaderID: "leader",
			Entries: []raft.LogEntry{{Index: 1, Term: term}},
		}
	}

	if _, err := cli.AppendEntries(ctx, "n1", req(1)); err != nil {
		t.Fatalf("AppendEntries to n1: %v", err)
	}
	select {
	case got := <-h1.appends:
		if got.Term != 1 {
			t.Errorf("n1 saw Term %d, want 1", got.Term)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("n1's handler was not called")
	}
	select {
	case <-h2.appends:
		t.Fatal("n2's handler was called for a request addressed to n1")
	default:
	}

	if _, err := cli.AppendEntries(ctx, "n2", req(2)); err != nil {
		t.Fatalf("AppendEntries to n2: %v", err)
	}
	select {
	case got := <-h2.appends:
		if got.Term != 2 {
			t.Errorf("n2 saw Term %d, want 2", got.Term)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("n2's handler was not called")
	}
}

// TestNodeIDHeader_RoutesEveryRPC checks that the addressing header is stamped
// on all five RPCs, not only the one that happened to be exercised.
func TestNodeIDHeader_RoutesEveryRPC(t *testing.T) {
	srv, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	defer func() { _ = srv.Close() }()

	target := newRecordingHandler()
	srv.Register("n1", newRecordingHandler())
	srv.Register("n2", target)

	cli, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	defer func() { _ = cli.Close() }()
	cli.AddPeer("n1", srv.Addr())
	cli.AddPeer("n2", srv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := cli.RequestVote(ctx, "n2", &raft.RequestVoteRequest{Term: 1, CandidateID: "c"}); err != nil {
		t.Errorf("RequestVote: %v", err)
	}
	if _, err := cli.InstallSnapshot(ctx, "n2", &raft.InstallSnapshotRequest{Term: 1, LeaderID: "l", Done: true}); err != nil {
		t.Errorf("InstallSnapshot: %v", err)
	}
	if _, err := cli.TimeoutNow(ctx, "n2", &raft.TimeoutNowRequest{Term: 1, LeaderID: "l"}); err != nil {
		t.Errorf("TimeoutNow: %v", err)
	}
	if _, err := cli.ReadIndex(ctx, "n2", &raft.ReadIndexRequest{Term: 1}); err != nil {
		t.Errorf("ReadIndex: %v", err)
	}

	if len(target.votes) != 1 {
		t.Errorf("n2 received %d RequestVote calls, want 1", len(target.votes))
	}
	if len(target.snapshots) != 1 {
		t.Errorf("n2 received %d InstallSnapshot calls, want 1", len(target.snapshots))
	}
	if len(target.timeouts) != 1 {
		t.Errorf("n2 received %d TimeoutNow calls, want 1", len(target.timeouts))
	}
}
