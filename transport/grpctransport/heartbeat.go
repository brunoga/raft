package grpctransport

import (
	"context"
	"errors"
	"sync"
	"time"

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
)

// hbCall is one b.pending heartbeat enqueued by a Node goroutine.
type hbCall struct {
	entry  *pb.HeartbeatEntry
	respCh chan hbResp // buffered(1); always written before read
}

// hbResp carries the result back to the waiting Node goroutine.
type hbResp struct {
	term    uint64
	success bool
	err     error
}

// peerBatcher collects heartbeat calls destined for one peer and periodically
// flushes them as a single BatchHeartbeats RPC.
type peerBatcher struct {
	peer    raft.NodeID
	ch      chan hbCall
	cancel  context.CancelFunc             // cancels this batcher's context (used by removePeer)
	pending []hbCall                         // reused across flush cycles; owned by run goroutine only
	results map[uint64]*pb.HeartbeatResult // reused across RPC cycles; owned by run goroutine only
}

// run is the batcher goroutine. It blocks until the first call arrives,
// opens a short window to collect stragglers, then sends one RPC and
// distributes the results before looping.
func (b *peerBatcher) run(ctx context.Context, t *GRPCTransport) {
	for {
		// Block until the first call (or shutdown).
		var first hbCall
		select {
		case first = <-b.ch:
		case <-ctx.Done():
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
				return
			}
		}
		window.Stop()

		// Build and send the batched RPC.
		req := &pb.BatchedHeartbeatRequest{
			Entries: make([]*pb.HeartbeatEntry, len(b.pending)),
		}
		for i, c := range b.pending {
			req.Entries[i] = c.entry
		}

		client, err := t.clientFor(b.peer)
		if err != nil {
			for _, c := range b.pending {
				c.respCh <- hbResp{err: err}
			}
			continue
		}

		sendCtx, cancel := context.WithTimeout(ctx, t.heartbeatRPCTimeout())
		resp, err := client.BatchHeartbeats(sendCtx, req)
		cancel()

		if err != nil {
			for _, c := range b.pending {
				c.respCh <- hbResp{err: err}
			}
			continue
		}

		// Index results by group_id for O(1) lookup.
		// Reuse the map across cycles: clear entries (keeps underlying memory) rather
		// than allocating a new map on every RPC.
		for k := range b.results {
			delete(b.results, k)
		}
		for _, r := range resp.Results {
			b.results[r.GroupId] = r
		}
		for _, c := range b.pending {
			if r, ok := b.results[c.entry.GroupId]; ok {
				c.respCh <- hbResp{term: r.Term, success: r.Success}
			} else {
				// No result for this group (e.g. receiver doesn't know it yet).
				// Return a best-effort non-success so the Node retries next tick.
				c.respCh <- hbResp{success: false}
			}
		}
	}
}

// heartbeatBatcher manages one peerBatcher goroutine per remote peer.
type heartbeatBatcher struct {
	t      *GRPCTransport
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	stopped  bool                          // set true by stop(); checked by peerBatcherFor
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

// stop cancels the shared context and blocks until all peerBatcher goroutines
// have exited. Callers (e.g. GRPCTransport.Close) must call stop before
// releasing any resources the goroutines depend on.
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
	batcher := &peerBatcher{
		peer:    peer,
		ch:      make(chan hbCall, b.t.hbChanSize),
		cancel:  peerCancel,
		results: make(map[uint64]*pb.HeartbeatResult),
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
	call := hbCall{entry: entry, respCh: respCh}

	batcher, err := b.peerBatcherFor(to)
	if err != nil {
		b.respChanPool.Put(respCh)
		return nil, err
	}

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
			Term:    raft.Term(r.term),
			Success: r.success,
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
	}
}
