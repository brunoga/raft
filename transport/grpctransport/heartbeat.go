package grpctransport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/brunoga/raft"
	pb "github.com/brunoga/raft/transport/grpctransport/raftpb"
)

// errBatcherStopped is returned by peerBatcherFor when the heartbeatBatcher
// has already been stopped.
var errBatcherStopped = errors.New("heartbeat batcher stopped")

const (
	// defaultHBWindow is the default collection window for the heartbeat
	// batcher. One tick fires all G groups' heartbeats nearly simultaneously
	// (RunTicker fans out in parallel goroutines), so 1 ms is enough to collect
	// all of them before sending a single RPC. Tune with WithHeartbeatWindow.
	defaultHBWindow = time.Millisecond

	// hbChanSizeDefault is the default per-peer channel depth. With G groups all
	// enqueueing concurrently, the channel must absorb a full tick's worth of
	// entries without blocking callers. Tune with WithHeartbeatChannelSize.
	hbChanSizeDefault = 1024

	// hbMinRPCTimeout floors the deadline derived from the callers' own
	// deadlines, so a caller that is already nearly out of time cannot shrink
	// the batch RPC to a deadline no round trip could ever meet.
	hbMinRPCTimeout = 10 * time.Millisecond
)

// hbCall is one pending heartbeat enqueued by a Node goroutine.
type hbCall struct {
	entry  *pb.HeartbeatEntry
	ctx    context.Context // the calling Node's context; governs this entry's lifetime
	respCh chan hbResp     // buffered(1); always written before read
}

// hbResp carries the result back to the waiting Node goroutine.
type hbResp struct {
	term          uint64
	success       bool
	conflictIndex uint64
	conflictTerm  uint64
	err           error
}

// peerBatcher collects heartbeat calls destined for one peer and periodically
// flushes them as a single BatchHeartbeats RPC.
//
// A flush runs on its own goroutine, bounded by inflight, so that the run loop
// can open the next collection window immediately. Serialising the flushes
// would mean a peer whose event loop is wedged stalls every group's heartbeats
// to that peer for as long as the stuck RPC lasts: the channel fills with
// entries whose callers have long since timed out, and the next flush ships a
// batch of heartbeats that are already stale.
type peerBatcher struct {
	peer     raft.NodeID
	ch       chan hbCall
	ctx      context.Context    // per-peer context; Done when removePeer cancels it
	cancel   context.CancelFunc // cancels this batcher's context (used by removePeer)
	pending  []hbCall           // reused across collection cycles; owned by run goroutine only
	inflight chan struct{}      // bounds concurrent BatchHeartbeats RPCs to this peer
	sends    sync.WaitGroup     // tracks in-flight flush goroutines
}

// run is the batcher goroutine. It blocks until the first call arrives, opens
// a short window to collect stragglers, then hands the batch to a flush
// goroutine and loops without waiting for the RPC to complete.
func (b *peerBatcher) run(ctx context.Context, t *GRPCTransport) {
	// A flush goroutine may still be waiting on an RPC when the loop exits; it
	// owns the response channels of the calls it took, so it must finish before
	// stop() can report the batcher as fully drained.
	defer b.sends.Wait()

	for {
		// Block until the first call (or shutdown).
		var first hbCall
		select {
		case first = <-b.ch:
		case <-ctx.Done():
			// Drain any calls that arrived before shutdown so that Send's drainer
			// goroutines (spawned when the caller ctx cancelled after enqueue) are
			// not left waiting on respCh forever.
			b.drainAll(ctx.Err())
			return
		}

		// Collect remaining calls within hbWindow. Reuse the slice across
		// cycles (clear only) to avoid a per-batch heap allocation.
		b.pending = b.pending[:0]
		b.pending = append(b.pending, first)
		window := time.NewTimer(t.hbWindow)
	drain:
		for {
			select {
			case c := <-b.ch:
				b.pending = append(b.pending, c)
			case <-window.C:
				break drain
			case <-ctx.Done():
				window.Stop()
				for _, c := range b.pending {
					c.respCh <- hbResp{err: ctx.Err()}
				}
				// Drain calls that arrived in the channel during the window so
				// that Send's drainer goroutines do not leak.
				b.drainAll(ctx.Err())
				return
			}
		}
		window.Stop()

		// Hand the batch off and immediately start collecting the next window.
		// b.pending is reused, so the flush gets a copy of its own.
		batch := make([]hbCall, len(b.pending))
		copy(batch, b.pending)
		b.flush(ctx, t, batch)
	}
}

// drainAll answers every call still queued on the channel with err. Called on
// shutdown so that no caller, and no Send drainer goroutine, waits forever.
func (b *peerBatcher) drainAll(err error) {
	for {
		select {
		case c := <-b.ch:
			c.respCh <- hbResp{err: err}
		default:
			return
		}
	}
}

// flush sends one batch. The RPC runs on a separate goroutine bounded by
// b.inflight; when every slot is taken it is sent inline, which applies
// backpressure to this peer alone.
func (b *peerBatcher) flush(ctx context.Context, t *GRPCTransport, batch []hbCall) {
	select {
	case b.inflight <- struct{}{}:
	default:
		// This peer already has the maximum number of batches in flight. Send
		// inline rather than growing the number of outstanding RPCs without
		// bound.
		b.send(ctx, t, batch)
		return
	}
	b.sends.Add(1)
	go func() {
		defer b.sends.Done()
		defer func() { <-b.inflight }()
		b.send(ctx, t, batch)
	}()
}

// send performs one BatchHeartbeats RPC and distributes the per-group results
// to the waiting callers. Every call in batch is answered exactly once.
func (b *peerBatcher) send(ctx context.Context, t *GRPCTransport, batch []hbCall) {
	// Drop entries whose caller has already given up. Sending them wastes
	// bandwidth and, worse, makes the receiving node process heartbeats on
	// behalf of a leader that stopped waiting for the answer several windows
	// ago.
	live := batch[:0]
	for _, c := range batch {
		if err := c.ctx.Err(); err != nil {
			c.respCh <- hbResp{err: err}
			continue
		}
		live = append(live, c)
	}
	batch = live
	if len(batch) == 0 {
		return
	}

	req := &pb.BatchedHeartbeatRequest{
		Entries: make([]*pb.HeartbeatEntry, len(batch)),
	}
	for i, c := range batch {
		req.Entries[i] = c.entry
	}

	client, err := t.clientFor(b.peer)
	if err != nil {
		for _, c := range batch {
			c.respCh <- hbResp{err: err}
		}
		return
	}

	sendCtx, cancel := context.WithTimeout(ctx, batchDeadline(batch, t.heartbeatRPCTimeout()))
	resp, err := client.BatchHeartbeats(outgoingContext(sendCtx, b.peer), req)
	cancel()

	if err != nil {
		err = sendErr("BatchHeartbeats", err)
		for _, c := range batch {
			c.respCh <- hbResp{err: err}
		}
		return
	}

	// Index results by group_id for O(1) lookup.
	results := make(map[uint64]*pb.HeartbeatResult, len(resp.Results))
	for _, r := range resp.Results {
		results[r.GroupId] = r
	}
	for _, c := range batch {
		r, ok := results[c.entry.GroupId]
		if !ok {
			// The receiver answered the batch but said nothing about this group.
			// That is a protocol violation, not a log mismatch, so it is
			// reported as an error: a synthetic Success:false would make the
			// leader rewind this follower's nextIndex for no reason.
			c.respCh <- hbResp{err: fmt.Errorf(
				"grpctransport: BatchHeartbeats returned no result for group %d",
				c.entry.GroupId)}
			continue
		}
		if code := codes.Code(r.ErrorCode); code != codes.OK {
			// The receiver could not dispatch this entry (unknown group, failed
			// handler, cancelled context). Surface it the way the unbatched
			// AppendEntries path would: as a Go error, so the core treats the
			// heartbeat as dropped rather than as a log mismatch.
			c.respCh <- hbResp{err: sendErr("BatchHeartbeats",
				status.Error(code, r.ErrorMessage))}
			continue
		}
		c.respCh <- hbResp{
			term:          r.Term,
			success:       r.Success,
			conflictIndex: r.ConflictIndex,
			conflictTerm:  r.ConflictTerm,
		}
	}
}

// batchDeadline returns the timeout to apply to one BatchHeartbeats RPC: the
// shortest deadline among the batch's callers, clamped to
// [hbMinRPCTimeout, max].
//
// Applying the configured maximum unconditionally decouples the RPC from the
// callers it serves, so a wedged peer keeps a connection and a goroutine busy
// for the full timeout even when every caller gave up milliseconds in.
func batchDeadline(batch []hbCall, maxWait time.Duration) time.Duration {
	now := time.Now()
	timeout := maxWait
	for _, c := range batch {
		dl, ok := c.ctx.Deadline()
		if !ok {
			continue
		}
		if remaining := dl.Sub(now); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout < hbMinRPCTimeout {
		return hbMinRPCTimeout
	}
	return timeout
}

// heartbeatBatcher manages one peerBatcher goroutine per remote peer.
type heartbeatBatcher struct {
	t      *GRPCTransport
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	stopped  bool // set true by stop(); checked by peerBatcherFor
	batchers map[raft.NodeID]*peerBatcher
	wg       sync.WaitGroup // tracks all live peerBatcher goroutines

	// respChanPool reuses buffered channels used to carry heartbeat responses
	// back to callers of Send. Per-instance (rather than package-level) so that
	// pools from different transports are isolated and channels from one instance
	// are never recycled into another.
	respChanPool sync.Pool
}

func newHeartbeatBatcher(t *GRPCTransport) *heartbeatBatcher {
	ctx, cancel := context.WithCancel(context.Background())
	b := &heartbeatBatcher{
		t:        t,
		ctx:      ctx,
		cancel:   cancel,
		batchers: make(map[raft.NodeID]*peerBatcher),
	}
	b.respChanPool.New = func() any { return make(chan hbResp, 1) }
	return b
}

// stop cancels the shared context and blocks until all peerBatcher goroutines,
// and any batch RPCs they still have in flight, have finished. Callers (e.g.
// GRPCTransport.Close) must call stop before releasing any resources the
// goroutines depend on.
func (b *heartbeatBatcher) stop() {
	b.mu.Lock()
	b.stopped = true
	b.mu.Unlock()
	b.cancel()
	b.wg.Wait()
}

// removePeer stops the peerBatcher goroutine for the given peer (if any) and
// removes it from the registry. Subsequent sends to that peer will create a
// fresh batcher. This is called from GRPCTransport.RemovePeer so that stale
// batcher goroutines don't accumulate when peers leave the cluster.
//
// removePeer is fire-and-forget: it cancels the batcher's context and returns
// without waiting for the goroutine to exit. The goroutine will exit promptly
// on its next ctx.Done check, but there is a brief window where both the old
// goroutine (draining or mid-RPC) and a new goroutine (created for the same
// peer by a concurrent peerBatcherFor call) are alive simultaneously. This is
// safe: the old goroutine owns only already-enqueued calls; new calls go to
// the new goroutine.
func (b *heartbeatBatcher) removePeer(peer raft.NodeID) {
	b.mu.Lock()
	batcher, ok := b.batchers[peer]
	if ok {
		delete(b.batchers, peer)
	}
	b.mu.Unlock()
	if ok {
		batcher.cancel()
	}
}

// peerBatcherFor lazily creates a peerBatcher for the given peer.
// Returns errBatcherStopped if stop() has already been called.
func (b *heartbeatBatcher) peerBatcherFor(peer raft.NodeID) (*peerBatcher, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return nil, errBatcherStopped
	}
	if existing, ok := b.batchers[peer]; ok {
		return existing, nil
	}
	// Each peerBatcher gets its own cancellable child context so that
	// removePeer can stop just that goroutine without affecting others.
	peerCtx, peerCancel := context.WithCancel(b.ctx)
	inflight := b.t.hbInflight
	if inflight < 1 {
		inflight = 1
	}
	batcher := &peerBatcher{
		peer:     peer,
		ch:       make(chan hbCall, b.t.hbChanSize),
		ctx:      peerCtx,
		cancel:   peerCancel,
		inflight: make(chan struct{}, inflight),
	}
	b.batchers[peer] = batcher
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		batcher.run(peerCtx, b.t)
	}()
	return batcher, nil
}

// Send enqueues a pure-heartbeat AppendEntries call and blocks until the
// batcher receives the result from the remote peer.
//
// The response mirrors what the unbatched AppendEntries path would return,
// conflict hints included. A failure to reach the peer, or to dispatch on it,
// is returned as a non-nil error rather than as a Success:false response, so
// the core can tell a dropped heartbeat from a log mismatch.
func (b *heartbeatBatcher) Send(ctx context.Context, to raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	entry := &pb.HeartbeatEntry{
		GroupId:      req.GroupID,
		Term:         uint64(req.Term),
		LeaderId:     string(req.LeaderID),
		PrevLogIndex: uint64(req.PrevLogIndex),
		PrevLogTerm:  uint64(req.PrevLogTerm),
		LeaderCommit: uint64(req.LeaderCommit),
		ReadBarrier:  req.ReadBarrier,
	}

	// Get a buffered response channel from the pool to avoid per-call allocation.
	respCh := b.respChanPool.Get().(chan hbResp)
	call := hbCall{entry: entry, ctx: ctx, respCh: respCh}

	batcher, err := b.peerBatcherFor(to)
	if err != nil {
		b.respChanPool.Put(respCh)
		return nil, err
	}
	// Capture the batcher's context before any concurrent removePeer can cancel
	// it. Used below to detect the window where the goroutine exits after
	// peerBatcherFor returns but before we finish waiting for the response.
	batcherCtx := batcher.ctx

	// Fast path: try to enqueue without blocking. If the channel is full, fall
	// through to the blocking path and increment the backpressure counter so
	// operators can detect hbChanSize saturation via HeartbeatSendBlocked.
	select {
	case batcher.ch <- call:
		// call enqueued without blocking
	default:
		b.t.hbSendBlocked.Add(1)
		select {
		case batcher.ch <- call:
			// call enqueued after waiting; batcher WILL write to respCh exactly once.
		case <-ctx.Done():
			// call NOT enqueued; batcher will never write to respCh — safe to pool.
			b.respChanPool.Put(respCh)
			return nil, ctx.Err()
		case <-batcherCtx.Done():
			// Batcher goroutine stopped while we were waiting to enqueue.
			// The call was never enqueued so respCh will never be written.
			b.respChanPool.Put(respCh)
			return nil, errBatcherStopped
		}
	}

	select {
	case r := <-respCh:
		// Normal path: response received; channel is now empty — safe to pool.
		b.respChanPool.Put(respCh)
		if r.err != nil {
			return nil, r.err
		}
		return &raft.AppendEntriesResponse{
			Term:          raft.Term(r.term),
			Success:       r.success,
			ConflictIndex: raft.Index(r.conflictIndex),
			ConflictTerm:  raft.Term(r.conflictTerm),
		}, nil
	case <-ctx.Done():
		// call was enqueued but we abandoned the wait. The batcher will still
		// write to respCh (channel is buffered-1 so its write is non-blocking).
		// We must not return respCh to the pool yet; a drainer goroutine waits
		// for the write and then recycles the channel.
		go func() {
			<-respCh
			b.respChanPool.Put(respCh)
		}()
		return nil, ctx.Err()
	case <-batcherCtx.Done():
		// The batcher was stopped after we enqueued the call. The call may
		// already have been taken by a flush that is still waiting on its RPC,
		// in which case a write to respCh is still to come; or the batcher may
		// have exited without ever reading it, in which case none is. Neither
		// draining nor waiting is safe, so the channel is dropped rather than
		// recycled: returning it to the pool could hand a later caller a
		// channel that already holds a stale response.
		return nil, errBatcherStopped
	}
}
