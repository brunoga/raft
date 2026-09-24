package grpctransport_test

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/transport/grpctransport"
)

// freePort reserves and releases a local TCP port so the same address can be
// bound again later in the test.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestReconnect_RestartedPeerIsReachedQuickly restarts a stopped peer at the
// same address and checks that the sender reaches it again promptly, rather
// than waiting out a reconnect backoff that has grown during the outage.
//
// No test previously restarted a stopped peer at all, so nothing covered the
// path a node takes when it rejoins after a crash or a rolling restart.
func TestReconnect_RestartedPeerIsReachedQuickly(t *testing.T) {
	addr := freePort(t)

	srv, err := grpctransport.Listen(addr, grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	srv.Register("srv", newRecordingHandler())

	cli, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	cli.AddPeer("srv", addr)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := &raft.AppendEntriesRequest{Term: 1, LeaderID: "leader",
		Entries: []raft.LogEntry{{Index: 1, Term: 1}}}

	if _, e := cli.AppendEntries(ctx, "srv", req); e != nil {
		t.Fatalf("initial AppendEntries: %v", e)
	}

	// Take the peer down and keep it down long enough that a backoff with a
	// multiplier has had several failed attempts to grow on.
	if e := srv.Close(); e != nil {
		t.Fatalf("Close server: %v", e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 200*time.Millisecond)
		_, attemptErr := cli.AppendEntries(attemptCtx, "srv", req)
		attemptCancel()
		if attemptErr == nil {
			t.Fatal("the peer answered after it was closed")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Bring the peer back at the same address.
	restarted, err := grpctransport.Listen(addr, grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("restart server: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.Register("srv", newRecordingHandler())

	// The sender must notice within a Raft election timeout, not within
	// gRPC's default two-minute backoff ceiling.
	const budget = 10 * time.Second
	start := time.Now()
	for {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, time.Second)
		_, attemptErr := cli.AppendEntries(attemptCtx, "srv", req)
		attemptCancel()
		if attemptErr == nil {
			t.Logf("reconnected after %v", time.Since(start))
			return
		}
		if time.Since(start) > budget {
			t.Fatalf("the restarted peer was still unreachable after %v: %v", budget, attemptErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestReconnect_ConnectionSurvivesBeyondThirtySeconds is a guard against
// re-introducing a server-side MaxConnectionAge.
//
// Recycling every peer link on a fixed schedule buys nothing in a Raft mesh,
// where the peer set is fixed and every link is long-lived: it only adds a
// GOAWAY, a redial and a latency spike on every link, at whatever interval is
// configured. The test keeps a link idle across that former 30 s boundary and
// then checks it still carries traffic.
func TestReconnect_ConnectionSurvivesBeyondThirtySeconds(t *testing.T) {
	// Idling past thirty seconds used to mean waiting thirty-five of them,
	// behind a testing.Short guard that skipped it. In a bubble the clock is
	// fake and advances only when every goroutine is blocked, so the idle
	// period costs nothing and the test always runs. gRPC is unchanged
	// underneath -- the same HTTP/2, the same keepalive timers, which see the
	// thirty-five seconds pass exactly as they would have -- and only the
	// connection beneath it is a pipe rather than a socket.
	//
	// The other tests in this file keep their sockets: they turn on a server
	// being closed and rebound at the same address, and what that does to a
	// connection is the thing under test.
	synctest.Test(t, func(t *testing.T) {
		cli, srv := pairedMemTransports(t)
		srv.Register("srv", newRecordingHandler())

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		req := &raft.AppendEntriesRequest{Term: 1, LeaderID: "leader",
			Entries: []raft.LogEntry{{Index: 1, Term: 1}}}

		if _, err := cli.AppendEntries(ctx, "srv", req); err != nil {
			t.Fatalf("initial AppendEntries: %v", err)
		}

		// Idle well past the boundary at which the connection used to be
		// recycled.
		time.Sleep(35 * time.Second)

		sendCtx, sendCancel := context.WithTimeout(ctx, 5*time.Second)
		defer sendCancel()
		if _, err := cli.AppendEntries(sendCtx, "srv", req); err != nil {
			t.Fatalf("AppendEntries after an idle period: %v", err)
		}
	})
}

// TestReconnect_BackoffIsConfigured verifies that the transport governs its own
// reconnect backoff instead of inheriting gRPC's.
//
// gRPC's stock backoff starts at 1 s and grows towards a 120 s ceiling, so a
// peer that has been down for a while can go uncontacted for up to two minutes
// after it returns — far longer than any Raft election timeout, and long enough
// for the cluster to keep electing around a node that is actually healthy. The
// test pins the behaviour by configuring an extreme backoff and showing it is
// honoured: a transport that ignored the setting would fall back to gRPC's much
// shorter default and reconnect anyway.
func TestReconnect_BackoffIsConfigured(t *testing.T) {
	addr := freePort(t)

	srv, err := grpctransport.Listen(addr, grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	srv.Register("srv", newRecordingHandler())

	// A backoff far longer than the test's patience. Every reconnect attempt
	// after the first failure must wait it out.
	cli, err := grpctransport.Listen("127.0.0.1:0",
		grpctransport.WithReconnectBackoff(90*time.Second, 120*time.Second), grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	cli.AddPeer("srv", addr)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := &raft.AppendEntriesRequest{Term: 1, LeaderID: "leader",
		Entries: []raft.LogEntry{{Index: 1, Term: 1}}}

	if _, callErr := cli.AppendEntries(ctx, "srv", req); callErr != nil {
		t.Fatalf("initial AppendEntries: %v", callErr)
	}

	// Take the peer down and fail one send against it, so the connection
	// enters backoff.
	if callErr := srv.Close(); callErr != nil {
		t.Fatalf("Close server: %v", callErr)
	}
	failCtx, failCancel := context.WithTimeout(ctx, 2*time.Second)
	if _, callErr := cli.AppendEntries(failCtx, "srv", req); callErr == nil {
		failCancel()
		t.Fatal("the peer answered after it was closed")
	}
	failCancel()

	// Bring it straight back at the same address.
	restarted, err := grpctransport.Listen(addr, grpctransport.WithInsecure())
	if err != nil {
		t.Fatalf("restart server: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.Register("srv", newRecordingHandler())

	// With the configured backoff the client must still be waiting. Reaching
	// the peer within a few seconds means the setting was ignored and gRPC's
	// own, much shorter, backoff applied instead.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, err := cli.AppendEntries(attemptCtx, "srv", req)
		attemptCancel()
		if err == nil {
			t.Fatal("reconnected despite a 90s configured backoff; the transport is not setting its own connect parameters")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
