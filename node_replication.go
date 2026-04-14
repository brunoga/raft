package raft

import "context"

// ---- Replication --------------------------------------------------------------

func (n *Node) handleAppendEntries(req *AppendEntriesRequest) (*AppendEntriesResponse, error) {
	resp := &AppendEntriesResponse{Term: n.currentTerm}

	if req.Term < n.currentTerm {
		return resp, nil
	}
	// Valid leader contact — reset election timer.
	n.becomeFollower(req.Term, req.LeaderID)
	resp.Term = n.currentTerm

	// Verify prevLog matches.
	if req.PrevLogIndex > 0 {
		prevTerm, _ := n.log.termAt(n.stopCtx, req.PrevLogIndex)
		// prevTerm == 0 means the entry is not in our log; all valid Raft
		// terms are ≥ 1, so 0 reliably signals "not found" and the
		// mismatch check below handles both cases uniformly.
		if prevTerm != req.PrevLogTerm {
			if prevTerm == 0 {
				// Entry not in our log — give the leader a conflict hint.
				resp.ConflictIndex = n.log.lastLogIndex() + 1
			} else {
				// Term mismatch — help the leader back-track by term.
				resp.ConflictTerm = prevTerm
				resp.ConflictIndex = req.PrevLogIndex
				for resp.ConflictIndex > n.log.first {
					t, _ := n.log.termAt(n.stopCtx, resp.ConflictIndex-1)
					if t != prevTerm {
						break
					}
					resp.ConflictIndex--
				}
			}
			return resp, nil
		}
	}

	// Append new entries, truncating any conflicting suffix first.
	for i, e := range req.Entries {
		existingTerm, err := n.log.termAt(n.stopCtx, e.Index)
		if err != nil {
			// Entry doesn't exist — append from here onward.
			if appendErr := n.log.append(n.stopCtx, req.Entries[i:]); appendErr != nil {
				return resp, appendErr
			}
			break
		}
		if existingTerm != e.Term {
			// Conflict: truncate and replace.
			if truncErr := n.log.truncateSuffix(n.stopCtx, e.Index); truncErr != nil {
				return resp, truncErr
			}
			if appendErr := n.log.append(n.stopCtx, req.Entries[i:]); appendErr != nil {
				return resp, appendErr
			}
			break
		}
	}

	// Advance commitIndex.
	if req.LeaderCommit > n.commitIndex {
		n.commitIndex = min(req.LeaderCommit, n.log.lastLogIndex())
		n.notifyApply()
	}

	resp.Success = true
	return resp, nil
}

// broadcastHeartbeat sends empty AppendEntries to all peers.
// All reads of node state happen in the event-loop goroutine before any
// goroutine is spawned, so there are no data races.
func (n *Node) broadcastHeartbeat() {
	for _, peer := range n.cfg.Peers {
		// Build the request in the event-loop goroutine (safe: n is single-threaded here).
		prevIdx := n.nextIndex[peer] - 1
		prevTerm, _ := n.log.termAt(n.stopCtx, prevIdx)
		req := &AppendEntriesRequest{
			Term:         n.currentTerm,
			LeaderID:     n.cfg.ID,
			PrevLogIndex: prevIdx,
			PrevLogTerm:  prevTerm,
			LeaderCommit: n.commitIndex,
		}
		// Enqueue on the pump's size-1 channel. Non-blocking: if the pump is
		// still sending the previous heartbeat, the new one overwrites it (a
		// missed heartbeat only delays follower timer resets, not safety).
		ch := n.hbPumps[peer]
		select {
		case ch <- req:
		default:
			// Pump busy; drain the stale heartbeat and replace it.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- req:
			default:
			}
		}
	}
}

// replicateToFollowers sends AppendEntries with pending entries to all peers.
func (n *Node) replicateToFollowers() {
	for _, peer := range n.cfg.Peers {
		n.replicateToPeer(peer)
	}
}

// replicateToPeer sends AppendEntries to a single peer. If the peer's
// nextIndex has fallen behind our snapshot boundary, we send a snapshot
// instead via sendSnapshotToPeer. Honours MaxInflightRPCs: if the peer
// already has the maximum number of in-flight RPCs, this is a no-op.
func (n *Node) replicateToPeer(peer NodeID) {
	nextIdx := n.nextIndex[peer]

	// The follower needs entries we no longer have — send the snapshot.
	if nextIdx <= n.log.snapMeta.LastIncludedIndex {
		n.sendSnapshotToPeer(peer)
		return
	}

	// Backpressure: do not exceed MaxInflightRPCs per peer.
	if n.inflight[peer] >= n.cfg.MaxInflightRPCs {
		return
	}

	prevIdx := nextIdx - 1
	prevTerm, _ := n.log.termAt(n.stopCtx, prevIdx)

	var entries []LogEntry
	if n.log.lastLogIndex() >= nextIdx {
		var err error
		entries, err = n.cfg.Storage.GetLogEntries(n.stopCtx, nextIdx, n.log.lastLogIndex()+1)
		if err != nil {
			n.logger.Error("replicateToPeer: GetLogEntries", "peer", peer, "err", err)
			return
		}
		// Cap at MaxLogEntriesPerRPC.
		if len(entries) > n.cfg.MaxLogEntriesPerRPC {
			entries = entries[:n.cfg.MaxLogEntriesPerRPC]
		}
	}

	req := &AppendEntriesRequest{
		Term:         n.currentTerm,
		LeaderID:     n.cfg.ID,
		PrevLogIndex: prevIdx,
		PrevLogTerm:  prevTerm,
		Entries:      entries,
		LeaderCommit: n.commitIndex,
	}

	n.inflight[peer]++
	go func(p NodeID, r *AppendEntriesRequest) {
		ctx, cancel := context.WithTimeout(n.stopCtx, n.rpcTimeout())
		defer cancel()
		finish := n.traceRPC(p, "AppendEntries")
		resp, err := n.cfg.Transport.AppendEntries(ctx, p, r)
		finish(err)
		if err != nil || resp == nil {
			// Decrement on error so the next heartbeat can retry.
			select {
			case n.rpcCh <- rpcEnvelope{
				req: &appendResult{peer: p, term: 0, success: false, req: r, dropped: true},
			}:
			case <-n.stopCtx.Done():
			}
			return
		}
		select {
		case n.rpcCh <- rpcEnvelope{
			req: &appendResult{
				peer:          p,
				term:          resp.Term,
				success:       resp.Success,
				req:           r,
				conflictIndex: resp.ConflictIndex,
				conflictTerm:  resp.ConflictTerm,
			},
		}:
		case <-n.stopCtx.Done():
		}
	}(peer, req)
}

// appendResult carries the outcome of an AppendEntries RPC back to the event loop.
type appendResult struct {
	peer          NodeID
	term          Term
	success       bool
	req           *AppendEntriesRequest
	conflictIndex Index
	conflictTerm  Term
	// dropped is true when the RPC failed entirely (network error, timeout).
	// In that case all other fields except peer and req are meaningless;
	// the handler just decrements the inflight counter.
	dropped bool
}

func (n *Node) handleAppendResult(r *appendResult) {
	// Always decrement inflight regardless of outcome so the counter stays
	// consistent with the number of in-flight RPCs.
	// NOTE (false positive — no double-decrement): replicateToPeer increments
	// n.inflight[peer] exactly once before spawning a goroutine, and that
	// goroutine always sends exactly one appendResult (success or dropped).
	// Both paths are serialised through the event-loop channel, so this
	// decrement executes exactly once per increment.
	if n.inflight != nil {
		if n.inflight[r.peer] > 0 {
			n.inflight[r.peer]--
		}
	}
	if r.dropped {
		return
	}
	if r.term > n.currentTerm {
		n.becomeFollower(r.term, "")
		return
	}
	if n.state != Leader {
		return
	}
	if !r.success {
		// Back-track nextIndex using conflict hints.
		if r.conflictTerm != 0 {
			// Find last entry with conflictTerm in our log.
			newNext := r.conflictIndex
			for i := n.log.lastLogIndex(); i >= n.log.first; i-- {
				t, err := n.log.termAt(n.stopCtx, i)
				if err != nil {
					break
				}
				if t == r.conflictTerm {
					newNext = i + 1
					break
				}
			}
			n.nextIndex[r.peer] = newNext
		} else {
			n.nextIndex[r.peer] = r.conflictIndex
		}
		if n.nextIndex[r.peer] < 1 {
			n.nextIndex[r.peer] = 1
		}
		// Retry immediately.
		n.replicateToPeer(r.peer)
		return
	}

	// Record this peer as "recently heard from" for check-quorum.
	if n.quorumAcks != nil {
		n.quorumAcks[r.peer] = true
	}

	// Update peer progress.
	if len(r.req.Entries) > 0 {
		last := r.req.Entries[len(r.req.Entries)-1].Index
		if last > n.matchIndex[r.peer] {
			n.matchIndex[r.peer] = last
			n.nextIndex[r.peer] = last + 1
		}
	}
	n.maybeAdvanceCommit()

	// Leadership transfer: once the target has caught up, send TimeoutNow.
	if n.transferTarget == r.peer &&
		n.matchIndex[r.peer] >= n.log.lastLogIndex() {
		n.sendTimeoutNow(n.transferTarget)
		n.transferTarget = "" // wait for step-down via becomeFollower
		n.transferElapsed = 0
	}

	// Read-barrier: if this is a heartbeat ACK for the current barrier batch,
	// count it. Resolve all pending reads when a quorum has responded.
	// During joint consensus both C_old and C_new must independently have a
	// majority (same requirement as commit); otherwise a stale leader in a
	// partitioned minority config could satisfy a union-quorum and serve reads.
	if r.req.ReadBarrier != 0 && r.req.ReadBarrier == n.readBatchGen &&
		r.success && r.term == n.currentTerm {
		if n.readBatchAcks == nil {
			n.readBatchAcks = make(map[NodeID]bool)
		}
		n.readBatchAcks[r.peer] = true
		var confirmed bool
		if n.jointOld == nil {
			confirmed = hasMajorityAck(n.readBatchAcks, n.cfg.Peers, true)
		} else {
			confirmed = hasMajorityAck(n.readBatchAcks, n.jointOld, true) &&
				hasMajorityAck(n.readBatchAcks, n.jointNew, n.jointIncludeSelf)
		}
		if confirmed {
			n.confirmReadBatch()
		}
	}

	// Pipeline: if the peer is still behind, send the next batch immediately
	// rather than waiting for the next heartbeat. This is the key mechanism
	// for a lagging follower to catch up quickly.
	if n.nextIndex[r.peer] <= n.log.lastLogIndex() {
		n.replicateToPeer(r.peer)
	}
}

// hasMajorityAck reports whether acks (a set of peer IDs that responded
// positively) together with self (when includeSelf is true) form a strict
// majority of the group described by members.
//
// This is used for both election vote counting and read-barrier / check-quorum
// tracking; the same quorum formula applies to all three.
func hasMajorityAck(acks map[NodeID]bool, members []NodeID, includeSelf bool) bool {
	count := 0
	if includeSelf {
		count = 1
	}
	for _, m := range members {
		if acks[m] {
			count++
		}
	}
	total := len(members)
	if includeSelf {
		total++
	}
	return count > total/2
}

// replicatedOnMajority reports whether idx has been replicated on a majority
// of the group described by members plus self when includeSelf is true.
//
// members is a peer list for one config group (e.g. jointOld or jointNew) and
// never includes the local node. includeSelf must be false when the leader is
// removing itself and checking C_new — it is not a member of that group.
//
// Quorum requires count > total/2 (integer division), which is a strict
// majority for all cluster sizes:
//
//	N=1 (no peers, self):   total/2 = 0, count > 0 means count >= 1  ✓
//	N=3 (2 peers, self):    total/2 = 1, count > 1 means count >= 2  ✓
//	N=4 (3 peers, self):    total/2 = 2, count > 2 means count >= 3  ✓
//	N=5 (4 peers, self):    total/2 = 2, count > 2 means count >= 3  ✓
//	N=2 (2 peers, no self): total/2 = 1, count > 1 means count >= 2  ✓
func (n *Node) replicatedOnMajority(idx Index, members []NodeID, includeSelf bool) bool {
	count := 0
	if includeSelf {
		count = 1
	}
	for _, p := range members {
		if n.matchIndex[p] >= idx {
			count++
		}
	}
	total := len(members)
	if includeSelf {
		total++
	}
	return count > total/2
}

// maybeAdvanceCommit checks whether a new index can be committed (replicated
// on a majority) and advances commitIndex if so. During joint consensus a
// commit requires a majority of both C_old and C_new independently.
func (n *Node) maybeAdvanceCommit() {
	// Find the highest N such that the required quorum have matchIndex >= N and
	// log[N].term == currentTerm.
	for idx := n.log.lastLogIndex(); idx > n.commitIndex; idx-- {
		t, err := n.log.termAt(n.stopCtx, idx)
		if err != nil || t != n.currentTerm {
			continue
		}
		var committed bool
		if n.jointOld == nil {
			// Normal single-config majority. Self is always a member.
			committed = n.replicatedOnMajority(idx, n.cfg.Peers, true)
		} else {
			// Joint consensus: both C_old and C_new must independently have a
			// majority (Raft §6). jointOld and jointNew never include self.
			// Self is always in C_old (it is the leader); for C_new it is
			// only counted when jointIncludeSelf is true (i.e. self is
			// retained in the new membership).
			committed = n.replicatedOnMajority(idx, n.jointOld, true) &&
				n.replicatedOnMajority(idx, n.jointNew, n.jointIncludeSelf)
		}
		if committed {
			n.commitIndex = idx
			n.notifyApply()
			if n.cfg.Metrics != nil {
				n.cfg.Metrics.CommitAdvanced(n.cfg.ID, idx)
			}
			// First current-term commit: the leader's commitIndex is now
			// accurate (all previous-term entries are committed too). Kick
			// off any ReadIndex requests that were deferred waiting for this.
			if !n.leaderNopCommitted {
				n.leaderNopCommitted = true
				if len(n.pendingReads) > 0 {
					n.readBatchGen++
					n.readBatchAcks = make(map[NodeID]bool)
					n.readBatchIndex = n.commitIndex
					n.broadcastReadBarrier()
				}
			}
			break
		}
	}
}
