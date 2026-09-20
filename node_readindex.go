package raft

import "context"

// ---- Linearizable read-only queries ------------------------------------------

// readIndexResolver is the interface for resolving a ReadIndex request.
type readIndexResolver interface {
	resolve(index Index)
	reject(err error)
}

// readIndexMsg carries a ReadIndex request from a caller into the event loop.
type readIndexMsg struct {
	resolver readIndexResolver
	useLease bool // if true, short-circuit on a valid clock-based read lease
}

// promiseIndexResolver implements readIndexResolver for a promise[Index].
type promiseIndexResolver struct {
	p promise[Index]
}

func (r promiseIndexResolver) resolve(index Index) { r.p.resolve(index) }
func (r promiseIndexResolver) reject(err error)    { r.p.reject(err) }

// rpcReadIndexResolver implements readIndexResolver for an RPC response.
type rpcReadIndexResolver struct {
	term   Term
	respCh chan rpcResponse
}

func (r rpcReadIndexResolver) resolve(index Index) {
	r.respCh <- rpcResponse{
		resp: &ReadIndexResponse{Term: r.term, Index: index},
	}
}

func (r rpcReadIndexResolver) reject(err error) {
	r.respCh <- rpcResponse{err: err}
}

// handleReadIndex processes a ReadIndex request. If this node is the leader,
// it captures the current commitIndex as the readIndex and starts a barrier
// heartbeat round to confirm leadership. The Future resolves once a quorum
// has responded to the barrier.
//
// If msg.useLease is true and a valid clock-based read lease is held, the
// request is resolved immediately without a network round-trip.
func (n *Node) handleReadIndex(msg readIndexMsg) {
	if n.state != Leader {
		msg.resolver.reject(&NotLeaderError{Leader: n.leaderID})
		return
	}

	// Single-node cluster: we are the only member, so we are trivially the
	// leader and there is no previous leader that could have committed entries
	// we have not yet accounted for; resolve immediately.
	if n.isSingleVoter() {
		msg.resolver.resolve(n.commitIndex)
		return
	}

	// Raft §8: a leader must not serve reads until it has committed at least
	// one entry in its own term (the no-op). Until then, commitIndex may not
	// reflect all entries committed by the previous leader. Queue the request;
	// it will be dispatched by maybeAdvanceCommit when the nop commits.
	if !n.leaderNopCommitted {
		n.pendingReads = append(n.pendingReads, msg.resolver)
		return
	}

	// Lease fast path: if the caller opted in, short-circuit without a network
	// round-trip. Cache n.now() once so both branches see the same instant.
	if msg.useLease {
		now := n.now()
		if !n.leaseExpiry.IsZero() && now.Before(n.leaseExpiry) {
			msg.resolver.resolve(n.commitIndex)
		} else {
			msg.resolver.reject(ErrLeaseExpired)
		}
		return
	}

	// A read may only be answered by a confirmation round that started after it
	// arrived. The replies to a round already in flight say the node was leader
	// when that round went out, which is a moment that had already passed when
	// this request was made; leadership may have moved in between, and
	// answering from those replies would return a commit index from a leader
	// that had already been replaced.
	//
	// So a request that arrives mid-round waits for the next one. Requests that
	// arrive together still share a round, which is what keeps ReadIndex cheap
	// under load: the cost is one round per round-trip, not one per read.
	if n.readBatchAcks != nil {
		n.waitingReads = append(n.waitingReads, msg.resolver)
		return
	}

	n.pendingReads = append(n.pendingReads, msg.resolver)
	n.startReadBatch()
}

// startReadBatch begins a leadership-confirmation round for the reads in
// pendingReads. Leader only.
func (n *Node) startReadBatch() {
	n.readBatchGen++
	n.readBatchAcks = make(map[NodeID]bool)
	n.readBatchIndex = n.commitIndex
	n.broadcastReadBarrier()
}

func (n *Node) handleReadIndexRPC(req *ReadIndexRequest, respCh chan rpcResponse) {
	// Per Raft §5.1: a higher term in any incoming message means this node's
	// term is stale — step down immediately before doing anything else.
	if req.Term > n.currentTerm {
		n.becomeFollower(req.Term, "")
		respCh <- rpcResponse{err: &NotLeaderError{Leader: n.leaderID}}
		return
	}

	if n.state != Leader {
		respCh <- rpcResponse{err: &NotLeaderError{Leader: n.leaderID}}
		return
	}

	// A lower req.Term means the follower is behind (hasn't received the latest
	// heartbeat yet). We are still the valid leader — serve the read normally.
	// The old code returned ReadIndexResponse{Index: 0} with nil error here,
	// which looked like a successful linearizable read at index 0, allowing
	// stale reads. We now ignore req.Term entirely for serving decisions: the
	// only safety invariant is "am I the leader?", confirmed by the barrier
	// heartbeat that handleReadIndex will send.
	n.handleReadIndex(readIndexMsg{
		resolver: rpcReadIndexResolver{term: n.currentTerm, respCh: respCh},
		useLease: false, // always use full barrier for RPCs
	})
}

// broadcastReadBarrier sends a tagged heartbeat to all peers to confirm that
// this node is still the leader. The ReadBarrier field lets handleAppendResult
// identify responses that belong to the current batch.
// The send time is captured here so that confirmReadBatch can base the lease
// expiry on when followers received the heartbeat (≈send time), not on when
// the leader receives their ACK (send time + one RTT).
func (n *Node) broadcastReadBarrier() {
	n.leaseSendTime = n.now()
	for _, peer := range n.cfg.Peers {
		id := peer.ID
		prevIdx := n.nextIndex[id] - 1
		prevTerm, _ := n.log.termAt(prevIdx)
		req := &AppendEntriesRequest{
			GroupID:      n.cfg.GroupID,
			Term:         n.currentTerm,
			LeaderID:     n.cfg.ID,
			PrevLogIndex: prevIdx,
			PrevLogTerm:  prevTerm,
			LeaderCommit: n.commitIndex,
			ReadBarrier:  n.readBatchGen,
		}
		go func(p NodeID, r *AppendEntriesRequest) {
			ctx, cancel := context.WithTimeout(n.stopCtx, n.rpcTimeout())
			defer cancel()
			finish := n.traceRPC(p, "AppendEntries")
			resp, err := n.cfg.Transport.AppendEntries(ctx, p, r)
			finish(err)
			if err != nil || resp == nil {
				return
			}

			select {
			case n.rpcCh <- rpcEnvelope{
				req: &appendResult{
					peer:          p,
					req:           r,
					success:       resp.Success,
					term:          resp.Term,
					conflictIndex: resp.ConflictIndex,
					conflictTerm:  resp.ConflictTerm,
				},
			}:
			case <-n.stopCh:
			}
		}(id, req)
	}
}

// confirmReadBatch resolves all pending read futures with the captured
// readBatchIndex and resets the batch state.
// It also extends the clock-based read lease, enabling future ReadIndexLease
// calls to skip the heartbeat round-trip.
func (n *Node) confirmReadBatch() {
	// Set the lease expiry from the heartbeat SEND time, not from now.
	//
	// Correctness: followers reset their election timers when they RECEIVE the
	// heartbeat (≈ leaseSendTime + one_way_latency). A new election can start
	// no earlier than leaseSendTime + ElectionTimeoutMin (ignoring one-way
	// latency, which is bounded by RPCTimeout << ElectionTimeoutMin in any
	// well-configured cluster). Using now() instead would add one RTT to the
	// lease duration — the leader would believe it holds the lease after a
	// follower's election timer has already expired.
	base := n.leaseSendTime
	if base.IsZero() {
		// Fallback for the initial heartbeat before leaseSendTime is set
		// (should not occur in normal operation).
		base = n.now()
	}
	n.leaseExpiry = base.Add(n.cfg.ElectionTimeoutMin)

	idx := n.readBatchIndex
	for _, p := range n.pendingReads {
		p.resolve(idx)
	}
	n.pendingReads = n.pendingReads[:0]
	n.readBatchAcks = nil

	// Reads that arrived while that round was in flight get a round of their
	// own, started now.
	if len(n.waitingReads) > 0 {
		n.pendingReads = append(n.pendingReads, n.waitingReads...)
		clear(n.waitingReads)
		n.waitingReads = n.waitingReads[:0]
		n.startReadBatch()
	}
}
