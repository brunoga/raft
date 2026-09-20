package grpctransport_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/transport/grpctransport"
)

// batchingPair returns a (sender, receiver) pair in multi-Raft mode: the
// receiver routes by the group IDs in groups, and the sender has heartbeat
// batching enabled. Both are closed when the test finishes.
func batchingPair(t *testing.T, groups map[uint64]raft.Handler, opts ...grpctransport.Option) (send, recv *grpctransport.GRPCTransport) {
	t.Helper()

	recv, err := grpctransport.Listen("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("Listen recv: %v", err)
	}
	t.Cleanup(func() { _ = recv.Close() })
	recv.SetGroupLookup(func(gid uint64) (raft.Handler, bool) {
		h, ok := groups[gid]
		return h, ok
	})

	send, err = grpctransport.Listen("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("Listen send: %v", err)
	}
	t.Cleanup(func() { _ = send.Close() })
	send.AddPeer("recv", recv.Addr())
	// SetGroupLookup on the sender is what enables heartbeat batching.
	send.SetGroupLookup(func(uint64) (raft.Handler, bool) { return nil, false })

	return send, recv
}

// TestBatchedHeartbeat_DeliversConflictHints verifies that a log mismatch
// answered through the batching path reaches the leader with its
// ConflictIndex and ConflictTerm intact.
//
// The hints drive the fast-backup optimisation. Dropping them leaves the
// leader with nothing but Success:false, which makes it rewind nextIndex to
// the start of the log and ship an entire snapshot to a follower that was only
// a few entries behind.
func TestBatchedHeartbeat_DeliversConflictHints(t *testing.T) {
	const (
		groupID       = 7
		wantTerm      = 5
		wantConflictI = 42
		wantConflictT = 3
	)

	h := newRecordingHandler()
	h.appendResp = &raft.AppendEntriesResponse{
		Term:          wantTerm,
		Success:       false,
		ConflictIndex: wantConflictI,
		ConflictTerm:  wantConflictT,
	}
	send, _ := batchingPair(t, map[uint64]raft.Handler{groupID: h})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
		GroupID:  groupID,
		Term:     wantTerm,
		LeaderID: "leader",
	})
	if err != nil {
		t.Fatalf("batched heartbeat: %v", err)
	}
	if resp.Success {
		t.Fatal("Success = true, want false (the handler reported a log mismatch)")
	}
	if resp.Term != wantTerm {
		t.Errorf("Term = %d, want %d", resp.Term, wantTerm)
	}
	if resp.ConflictIndex != wantConflictI {
		t.Errorf("ConflictIndex = %d, want %d", resp.ConflictIndex, wantConflictI)
	}
	if resp.ConflictTerm != wantConflictT {
		t.Errorf("ConflictTerm = %d, want %d", resp.ConflictTerm, wantConflictT)
	}
}

// TestBatchedHeartbeat_UnknownGroupReturnsError verifies that a heartbeat for
// a group the receiver does not host is reported as a transport error, not as
// a successful RPC that happens to carry Success:false.
//
// The two mean opposite things to the core: an error is a dropped RPC and
// leaves leader state untouched, while Success:false asserts that the
// follower's log diverges and triggers a nextIndex rewind. The unbatched
// AppendEntries path returns an error here, and the batched path must agree.
func TestBatchedHeartbeat_UnknownGroupReturnsError(t *testing.T) {
	send, _ := batchingPair(t, map[uint64]raft.Handler{1: newRecordingHandler()})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
		GroupID:  99, // not registered on the receiver
		Term:     1,
		LeaderID: "leader",
	})
	if err == nil {
		t.Fatalf("expected an error for an unregistered group, got response %+v", resp)
	}
}

// TestBatchedHeartbeat_HandlerErrorReturnsError verifies that a handler which
// fails — a stopped node, say — is reported as a transport error rather than
// being flattened into a log mismatch.
func TestBatchedHeartbeat_HandlerErrorReturnsError(t *testing.T) {
	h := newRecordingHandler()
	h.appendErr = errors.New("node is stopped")
	send, _ := batchingPair(t, map[uint64]raft.Handler{4: h})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
		GroupID:  4,
		Term:     1,
		LeaderID: "leader",
	})
	if err == nil {
		t.Fatalf("expected an error when the handler fails, got response %+v", resp)
	}
}

// TestBatchedHeartbeat_MatchesDirectPathOnSuccess checks that a successful
// batched heartbeat carries the same fields the unbatched path would return,
// so switching to multi-Raft mode cannot change what the leader observes.
func TestBatchedHeartbeat_MatchesDirectPathOnSuccess(t *testing.T) {
	h := newRecordingHandler()
	h.appendResp = &raft.AppendEntriesResponse{Term: 9, Success: true}
	send, _ := batchingPair(t, map[uint64]raft.Handler{2: h})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
		GroupID:      2,
		Term:         9,
		LeaderID:     "leader",
		LeaderCommit: 11,
		ReadBarrier:  77,
	})
	if err != nil {
		t.Fatalf("batched heartbeat: %v", err)
	}
	if !resp.Success || resp.Term != 9 {
		t.Errorf("got Term=%d Success=%v, want Term=9 Success=true", resp.Term, resp.Success)
	}

	// The ReadBarrier must survive the batching round trip, or the leader can
	// never match the response to its pending ReadIndex future.
	select {
	case got := <-h.appends:
		if got.ReadBarrier != 77 {
			t.Errorf("ReadBarrier = %d, want 77", got.ReadBarrier)
		}
		if got.LeaderCommit != 11 {
			t.Errorf("LeaderCommit = %d, want 11", got.LeaderCommit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never saw the heartbeat")
	}
}

// blockingHandler parks every HandleAppendEntries call until release is closed,
// and signals entered each time one arrives.
type blockingHandler struct {
	release     chan struct{}
	releaseOnce sync.Once
	entered     chan struct{} // signalled (non-blocking) on every entry
}

func newBlockingHandler() *blockingHandler {
	return &blockingHandler{
		release: make(chan struct{}),
		entered: make(chan struct{}, 1024),
	}
}

// releaseAll unblocks every parked call and every call that arrives later. It
// is safe to call more than once, so tests can release early and still defer
// it for the failure paths.
func (h *blockingHandler) releaseAll() {
	h.releaseOnce.Do(func() { close(h.release) })
}

func (h *blockingHandler) HandleRequestVote(_ context.Context, _ *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	return &raft.RequestVoteResponse{}, nil
}

func (h *blockingHandler) HandleAppendEntries(ctx context.Context, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &raft.AppendEntriesResponse{Term: req.Term, Success: true}, nil
}

func (h *blockingHandler) HandleInstallSnapshot(_ context.Context, _ *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	return &raft.InstallSnapshotResponse{}, nil
}

func (h *blockingHandler) HandleTimeoutNow(_ context.Context, _ *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	return &raft.TimeoutNowResponse{}, nil
}

func (h *blockingHandler) HandleReadIndex(_ context.Context, _ *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	return &raft.ReadIndexResponse{}, nil
}

// TestHeartbeatBatcher_WedgedPeerDoesNotDelayLaterHeartbeats verifies that a
// batch RPC still waiting on a wedged receiver does not hold up the heartbeats
// collected in the following windows.
//
// With a single serialised RPC per peer, the batcher cannot start the next
// batch until the stuck one returns — up to the full heartbeat RPC timeout.
// Every group sharing that peer is stalled for that whole period even though
// only one group is actually unhealthy.
func TestHeartbeatBatcher_WedgedPeerDoesNotDelayLaterHeartbeats(t *testing.T) {
	const (
		wedgedGroup  = 1
		healthyGroup = 2
	)

	wedged := newBlockingHandler()
	healthy := newRecordingHandler()
	send, _ := batchingPair(t, map[uint64]raft.Handler{
		wedgedGroup:  wedged,
		healthyGroup: healthy,
	}, grpctransport.WithHeartbeatRPCTimeout(30*time.Second))
	defer wedged.releaseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Park a heartbeat for the wedged group. Its batch RPC will not return
	// until the handler is released at the end of the test.
	stuckDone := make(chan struct{})
	go func() {
		defer close(stuckDone)
		//nolint:errcheck // the wedged call's outcome is not what this test asserts.
		send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
			GroupID: wedgedGroup, Term: 1, LeaderID: "leader",
		})
	}()

	// Wait until the receiver is actually inside the wedged handler, so the
	// first batch RPC is definitively in flight.
	select {
	case <-wedged.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the wedged handler was never reached")
	}

	// A heartbeat for the healthy group now lands in a later collection
	// window. It must not queue behind the stuck RPC.
	start := time.Now()
	hbCtx, hbCancel := context.WithTimeout(ctx, 5*time.Second)
	defer hbCancel()
	if _, err := send.AppendEntries(hbCtx, "recv", &raft.AppendEntriesRequest{
		GroupID: healthyGroup, Term: 1, LeaderID: "leader",
	}); err != nil {
		t.Fatalf("healthy group heartbeat: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("healthy group heartbeat took %v; it is queued behind the wedged peer's RPC", elapsed)
	}

	wedged.releaseAll()
	<-stuckDone
}

// TestHeartbeatBatcher_DropsEntriesWhoseCallerGaveUp verifies that a heartbeat
// whose caller has already timed out is discarded instead of being sent.
//
// A batcher that ships them anyway makes the receiver do work for a leader
// that stopped waiting several windows ago, and the entries it sends describe
// a log position that may since have moved.
func TestHeartbeatBatcher_DropsEntriesWhoseCallerGaveUp(t *testing.T) {
	h := newRecordingHandler()
	// A long collection window keeps the entry pending well past its caller's
	// deadline, so the batcher must decide whether to send it after the caller
	// is gone.
	send, recv := batchingPair(t, map[uint64]raft.Handler{1: h},
		grpctransport.WithHeartbeatWindow(400*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
		GroupID: 1, Term: 1, LeaderID: "leader",
	})
	if err == nil {
		t.Fatal("expected the caller's deadline to fire, got a response")
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("the caller waited %v, well past its own 100ms deadline", elapsed)
	}

	// Give the batcher time to close its window and decide what to flush.
	time.Sleep(time.Second)

	if got := recv.HeartbeatStats().EntriesServed; got != 0 {
		t.Errorf("receiver processed %d heartbeat entries; a stale entry was sent anyway", got)
	}
	select {
	case req := <-h.appends:
		t.Errorf("handler was called for a heartbeat whose caller had given up: %+v", req)
	default:
	}
}

// TestBatchHeartbeats_SlowGroupDoesNotBlockOthers verifies that groups whose
// event loops are busy do not stall the dispatch of unrelated groups travelling
// in the same batch.
//
// Server-side dispatch is bounded so that a huge batch cannot spawn unbounded
// goroutines, but the bound must never be enforced by making entries wait in
// line: with enough stuck groups, every healthy group behind them in the batch
// would see heartbeat timeouts caused purely by co-tenancy on one node.
func TestBatchHeartbeats_SlowGroupDoesNotBlockOthers(t *testing.T) {
	// More slow groups than the dispatcher's worker bound, so the bound is
	// saturated by the time the healthy group's entry is reached.
	numSlow := runtime.GOMAXPROCS(0)*4 + 2

	groups := make(map[uint64]raft.Handler, numSlow+1)
	slow := newBlockingHandler()
	for g := 1; g <= numSlow; g++ {
		groups[uint64(g)] = slow
	}
	fastID := uint64(numSlow + 1)
	fast := newRecordingHandler()
	groups[fastID] = fast

	// A long collection window so every heartbeat below travels in one batch.
	send, _ := batchingPair(t, groups, grpctransport.WithHeartbeatWindow(2*time.Second))
	defer slow.releaseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Enqueue the slow groups first so they occupy the front of the batch.
	var wg sync.WaitGroup
	for g := 1; g <= numSlow; g++ {
		wg.Add(1)
		go func(gid uint64) {
			defer wg.Done()
			//nolint:errcheck // the slow groups' outcome is not what this test asserts.
			send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
				GroupID: gid, Term: 1, LeaderID: "leader",
			})
		}(uint64(g))
	}

	// Give the slow entries time to queue, then add the healthy group. It is
	// still well inside the collection window, so it shares their batch but
	// sits behind every one of them.
	time.Sleep(300 * time.Millisecond)

	fastDone := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := send.AppendEntries(ctx, "recv", &raft.AppendEntriesRequest{
			GroupID: fastID, Term: 1, LeaderID: "leader",
		})
		fastDone <- err
	}()

	// The healthy group's handler must be reached while every slow group is
	// still parked inside its own handler.
	select {
	case req := <-fast.appends:
		if req.GroupID != fastID {
			t.Errorf("the healthy handler saw group %d, want %d", req.GroupID, fastID)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the healthy group was never dispatched; it is queued behind %d busy groups", numSlow)
	}

	slow.releaseAll()
	if err := <-fastDone; err != nil {
		t.Errorf("healthy group heartbeat: %v", err)
	}
	wg.Wait()
}
