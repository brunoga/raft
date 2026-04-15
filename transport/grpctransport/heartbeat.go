package grpctransport

import (
	"context"
	"sync"
	"time"

	"github.com/brunoga/raft"
	pb "github.com/brunoga/raft/transport/grpctransport/raftpb"
)

const (
	// defaultHBWindow is the default collection window for the heartbeat
	// batcher. One tick fires all G groups' heartbeats nearly simultaneously
	// (RunTicker fans out in parallel goroutines), so 1 ms is enough to collect
	// all of them before sending a single RPC. Tune with WithHeartbeatWindow.
	defaultHBWindow = time.Millisecond

	// hbChanSize is the per-peer channel depth. With G groups all enqueueing
	// concurrently, the channel must absorb a full tick's worth of entries
	// without blocking callers.
	hbChanSize = 1024
)

// hbCall is one pending heartbeat enqueued by a Node goroutine.
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
	peer raft.NodeID
	ch   chan hbCall
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

		// Collect remaining calls within hbWindow.
		pending := make([]hbCall, 0, 16)
		pending = append(pending, first)
		window := time.NewTimer(t.hbWindow)
	drain:
		for {
			select {
			case c := <-b.ch:
				pending = append(pending, c)
			case <-window.C:
				break drain
			case <-ctx.Done():
				window.Stop()
				for _, c := range pending {
					c.respCh <- hbResp{err: ctx.Err()}
				}
				return
			}
		}
		window.Stop()

		// Build and send the batched RPC.
		req := &pb.BatchedHeartbeatRequest{
			Entries: make([]*pb.HeartbeatEntry, len(pending)),
		}
		for i, c := range pending {
			req.Entries[i] = c.entry
		}

		client, err := t.clientFor(b.peer)
		if err != nil {
			for _, c := range pending {
				c.respCh <- hbResp{err: err}
			}
			continue
		}

		sendCtx, cancel := context.WithTimeout(ctx, t.heartbeatRPCTimeout())
		resp, err := client.BatchHeartbeats(sendCtx, req)
		cancel()

		if err != nil {
			for _, c := range pending {
				c.respCh <- hbResp{err: err}
			}
			continue
		}

		// Index results by group_id for O(1) lookup.
		results := make(map[uint64]*pb.HeartbeatResult, len(resp.Results))
		for _, r := range resp.Results {
			results[r.GroupId] = r
		}
		for _, c := range pending {
			if r, ok := results[c.entry.GroupId]; ok {
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
	batchers map[raft.NodeID]*peerBatcher
}

func newHeartbeatBatcher(t *GRPCTransport) *heartbeatBatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &heartbeatBatcher{
		t:        t,
		ctx:      ctx,
		cancel:   cancel,
		batchers: make(map[raft.NodeID]*peerBatcher),
	}
}

func (b *heartbeatBatcher) stop() { b.cancel() }

// peerBatcherFor lazily creates a peerBatcher for the given peer.
func (b *heartbeatBatcher) peerBatcherFor(peer raft.NodeID) *peerBatcher {
	b.mu.Lock()
	defer b.mu.Unlock()
	if existing, ok := b.batchers[peer]; ok {
		return existing
	}
	batcher := &peerBatcher{
		peer: peer,
		ch:   make(chan hbCall, hbChanSize),
	}
	b.batchers[peer] = batcher
	go batcher.run(b.ctx, b.t)
	return batcher
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
	respCh := make(chan hbResp, 1)
	call := hbCall{entry: entry, respCh: respCh}

	pb := b.peerBatcherFor(to)
	select {
	case pb.ch <- call:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case r := <-respCh:
		if r.err != nil {
			return nil, r.err
		}
		return &raft.AppendEntriesResponse{
			Term:    raft.Term(r.term),
			Success: r.success,
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
