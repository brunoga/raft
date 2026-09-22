package grpctransport

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
)

// clientConnCount reports how many client connections the transport is
// currently holding. Tests use it to prove that a connection is never
// created, or is created and then reclaimed, on paths that would otherwise
// leak it silently.
func (t *GRPCTransport) clientConnCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.clients)
}

// TestClose_SendAfterCloseLeaksNoConnection verifies that a send issued after
// Close fails instead of dialling a connection that nothing will ever close.
//
// Close drains the connection map, so a send that dialled afterwards would
// store a live *grpc.ClientConn in a map that is never swept again: the socket,
// its keepalive timer and its resolver goroutines would stay alive for the rest
// of the process.
func TestClose_SendAfterCloseLeaksNoConnection(t *testing.T) {
	tr, err := Listen("127.0.0.1:0", WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	tr.AddPeer("peer", "127.0.0.1:1")

	if closeErr := tr.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sends := map[string]func() error{
		"RequestVote": func() error {
			_, err := tr.RequestVote(ctx, "peer", &raft.RequestVoteRequest{Term: 1, CandidateID: "c"})
			return err
		},
		"AppendEntries": func() error {
			_, err := tr.AppendEntries(ctx, "peer", &raft.AppendEntriesRequest{Term: 1, LeaderID: "l"})
			return err
		},
		"AppendEntriesWithEntries": func() error {
			_, err := tr.AppendEntries(ctx, "peer", &raft.AppendEntriesRequest{
				Term: 1, LeaderID: "l",
				Entries: []raft.LogEntry{{Index: 1, Term: 1}},
			})
			return err
		},
		"InstallSnapshot": func() error {
			_, err := tr.InstallSnapshot(ctx, "peer", &raft.InstallSnapshotRequest{Term: 1, LeaderID: "l"})
			return err
		},
		"TimeoutNow": func() error {
			_, err := tr.TimeoutNow(ctx, "peer", &raft.TimeoutNowRequest{Term: 1, LeaderID: "l"})
			return err
		},
		"ReadIndex": func() error {
			_, err := tr.ReadIndex(ctx, "peer", &raft.ReadIndexRequest{Term: 1})
			return err
		},
	}
	for name, send := range sends {
		if err := send(); !errors.Is(err, ErrTransportClosed) {
			t.Errorf("%s after Close returned %v, want ErrTransportClosed", name, err)
		}
	}

	if n := tr.clientConnCount(); n != 0 {
		t.Errorf("transport holds %d connections after Close; sends dialled anyway", n)
	}
}

// TestClose_EmptyAppendEntriesAfterCloseReturnsError covers the batched
// heartbeat path specifically. Close clears the batcher, and without a closed
// check an empty AppendEntries would quietly fall through to the unbatched
// path and dial a fresh connection instead of reporting that the transport is
// gone.
func TestClose_EmptyAppendEntriesAfterCloseReturnsError(t *testing.T) {
	tr, err := Listen("127.0.0.1:0", WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	tr.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })
	tr.AddPeer("peer", "127.0.0.1:1")

	if closeErr := tr.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = tr.AppendEntries(ctx, "peer", &raft.AppendEntriesRequest{
		GroupID: 1, Term: 1, LeaderID: "l",
	})
	if !errors.Is(err, ErrTransportClosed) {
		t.Errorf("empty AppendEntries after Close returned %v, want ErrTransportClosed", err)
	}
	if n := tr.clientConnCount(); n != 0 {
		t.Errorf("transport holds %d connections after Close", n)
	}
}

// TestClose_IsIdempotent verifies that closing twice is harmless, since
// deferred cleanup commonly runs alongside an explicit shutdown.
func TestClose_IsIdempotent(t *testing.T) {
	tr, err := Listen("127.0.0.1:0", WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestClose_ConcurrentWithInFlightSends runs Close against a burst of
// concurrent sends. Every send must either complete or report that the
// transport closed, and none may leave a connection behind.
func TestClose_ConcurrentWithInFlightSends(t *testing.T) {
	srv, err := Listen("127.0.0.1:0", WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	srv.Register("srv", stubHandler{})

	tr, err := Listen("127.0.0.1:0", WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	tr.AddPeer("srv", srv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 8 {
				//nolint:errcheck // any outcome is acceptable; the assertions are below.
				tr.RequestVote(ctx, "srv", &raft.RequestVoteRequest{Term: 1, CandidateID: "c"})
			}
		}()
	}

	// Close while the sends are in flight.
	time.Sleep(5 * time.Millisecond)
	if closeErr := tr.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	wg.Wait()

	if n := tr.clientConnCount(); n != 0 {
		t.Errorf("transport holds %d connections after a concurrent Close", n)
	}
}

// TestRemovePeer_RacingClientForLeaksNoConnection verifies that a peer removed
// while a send is dialling does not leave the new connection stranded.
//
// clientFor dials outside the lock, so RemovePeer can run in between. Storing
// the dial result unconditionally would put it in the connection map under an
// address the reference counter no longer tracks, and nothing would ever close
// it: the socket, its keepalive timer and its resolver goroutines would
// outlive the peer for the rest of the process.
func TestRemovePeer_RacingClientForLeaksNoConnection(t *testing.T) {
	tr, err := Listen("127.0.0.1:0", WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()

	// Hold the dial open until RemovePeer has run, so the interleaving under
	// test happens every time rather than only when the scheduler obliges.
	removed := make(chan struct{})
	tr.afterDial = func() { <-removed }

	tr.AddPeer("churn", "127.0.0.1:1")

	dialed := make(chan error, 1)
	go func() {
		_, err := tr.clientFor("churn")
		dialed <- err
	}()

	// Give the dial time to reach the hook, then remove the peer underneath it.
	time.Sleep(50 * time.Millisecond)
	tr.RemovePeer("churn")
	close(removed)

	if err := <-dialed; err == nil {
		t.Error("clientFor returned a client for a peer that was removed mid-dial")
	}
	if n := tr.clientConnCount(); n != 0 {
		t.Errorf("%d connections left after RemovePeer raced the dial", n)
	}
}

// TestClose_RacingClientForLeaksNoConnection is the shutdown counterpart: a
// Close that lands while a send is dialling must not leave the fresh
// connection behind in a map Close has already drained.
func TestClose_RacingClientForLeaksNoConnection(t *testing.T) {
	tr, err := Listen("127.0.0.1:0", WithInsecure())
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan struct{})
	tr.afterDial = func() { <-closed }

	tr.AddPeer("peer", "127.0.0.1:1")

	dialed := make(chan error, 1)
	go func() {
		_, err := tr.clientFor("peer")
		dialed <- err
	}()

	time.Sleep(50 * time.Millisecond)
	if closeErr := tr.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	close(closed)

	if err := <-dialed; !errors.Is(err, ErrTransportClosed) {
		t.Errorf("clientFor returned %v, want ErrTransportClosed", err)
	}
	if n := tr.clientConnCount(); n != 0 {
		t.Errorf("%d connections left after Close raced the dial", n)
	}
}

// TestClose_BoundedWhenHandlerIsWedged verifies that Close returns within its
// configured timeout even when an RPC handler never does.
//
// A handler blocks on its node's event loop. If that loop is wedged, an
// unbounded GracefulStop waits for it forever and shutdown never completes.
func TestClose_BoundedWhenHandlerIsWedged(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	srv, err := Listen("127.0.0.1:0", WithCloseTimeout(250*time.Millisecond), WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	srv.Register("srv", stubHandler{
		appendEntries: func(ctx context.Context, _ *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return &raft.AppendEntriesResponse{}, nil
		},
	})

	cli, err := Listen("127.0.0.1:0", WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	cli.AddPeer("srv", srv.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		//nolint:errcheck // the call is abandoned when the server is forced down.
		cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
			Term: 1, LeaderID: "l",
			Entries: []raft.LogEntry{{Index: 1, Term: 1}},
		})
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler was never reached")
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- srv.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("Close took %v; it is not bounded by the close timeout", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close never returned: it is waiting on a wedged handler")
	}
}

// stubHandler is a raft.Handler whose methods return zero values unless a
// per-method override is supplied.
type stubHandler struct {
	appendEntries func(context.Context, *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error)
}

func (h stubHandler) HandleRequestVote(_ context.Context, _ *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	return &raft.RequestVoteResponse{}, nil
}

func (h stubHandler) HandleAppendEntries(ctx context.Context, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	if h.appendEntries != nil {
		return h.appendEntries(ctx, req)
	}
	return &raft.AppendEntriesResponse{}, nil
}

func (h stubHandler) HandleInstallSnapshot(_ context.Context, _ *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	return &raft.InstallSnapshotResponse{}, nil
}

func (h stubHandler) HandleTimeoutNow(_ context.Context, _ *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	return &raft.TimeoutNowResponse{}, nil
}

func (h stubHandler) HandleReadIndex(_ context.Context, _ *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	return &raft.ReadIndexResponse{}, nil
}
