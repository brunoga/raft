package raft

import "context"

// ---- Replication --------------------------------------------------------------

// handleAppendEntries processes one AppendEntries request and answers it on
// respCh, which may happen after this function has returned.
//
// The answer is the single most consequential message a follower sends. A
// leader that receives it counts this node towards the quorum that commits the
// entries, and a committed entry is applied and never revisited, so the answer
// means "these entries will survive my crash" and nothing weaker. That is why
// it waits for the write: the entries go into the log and become visible to
// everything this node does immediately, but the acknowledgement is held back
// until the storage write behind them has completed.
//
// A node that crashes in that window comes back without the entries and
// without having acknowledged them, which is indistinguishable from never
// having received the request -- the case Raft already handles by retrying.
func (n *Node) handleAppendEntries(req *AppendEntriesRequest, respCh chan rpcResponse) {
	resp := &AppendEntriesResponse{Term: n.currentTerm}
	// The response channel holds exactly one value and the caller reads it
	// once. Answering twice would block the event loop on the second send,
	// which is a far worse failure than the bug that caused it, so the guard
	// is here rather than in an argument about why it cannot happen.
	answered := false
	reply := func(err error) {
		if respCh == nil || answered {
			return
		}
		answered = true
		respCh <- rpcResponse{resp: resp, err: err}
	}
	// replyWhenDurable holds the answer back until everything this turn queued
	// is on disk. Every answer here goes through it, a rejection included: an
	// answer states this node's term whatever else it says.
	replyWhenDurable := func(writeSeq uint64) {
		n.afterWrite(n.sendGate(writeSeq), func() { reply(nil) }, reply)
	}

	if req.Term < n.currentTerm {
		replyWhenDurable(0)
		return
	}
	// Valid leader contact — reset election timer. This may step the node up
	// to the leader's term, which queues a hard-state write that every reply
	// below has to wait for: a rejection carries this node's term just as an
	// acknowledgement does.
	n.becomeFollower(req.Term, req.LeaderID)
	resp.Term = n.currentTerm

	// Verify prevLog matches.
	if req.PrevLogIndex > 0 {
		prevTerm, _ := n.log.termAt(req.PrevLogIndex)
		// prevTerm == 0 means the entry is not in our log; all valid Raft
		// terms are ≥ 1, so 0 reliably signals "not found" and the
		// mismatch check below handles both cases uniformly.
		if prevTerm != req.PrevLogTerm {
			if prevTerm == 0 {
				// Entry not in our log — give the leader a conflict hint.
				resp.ConflictIndex = n.log.lastLogIndex() + 1
			} else {
				// Term mismatch — help the leader back-track by term. The
				// useful answer is where the term this node disagreed in
				// begins, which the run index gives directly; walking back an
				// index at a time to find it meant a storage read per entry,
				// on the event loop, bounded only by the length of a term.
				resp.ConflictTerm = prevTerm
				resp.ConflictIndex = n.log.termRunStart(req.PrevLogIndex)
				if resp.ConflictIndex == 0 {
					// The index is the snapshot boundary rather than an entry
					// the log still holds, so the run is that one index. A
					// zero here would reach the leader as a hint to resume
					// from the very beginning, and it would ship its entire
					// state machine to a follower that needs almost none of it.
					resp.ConflictIndex = req.PrevLogIndex
				}
			}
			replyWhenDurable(0)
			return
		}
	}

	// A follower holds entries in memory until storage has written them, just
	// as a leader does, and needs the same bound. Its backlog is normally kept
	// small by what the leader will send before being acknowledged, which
	// MaxInflightRPCs and MaxBytesPerRPC limit -- but that is the leader's
	// restraint, not this node's, and a node should not be able to be driven
	// out of memory by a peer.
	//
	// Refusing outright rather than rejecting the append is deliberate: a
	// rejection is a statement about this node's log that would send the
	// leader hunting backwards through it for a disagreement that does not
	// exist. A failed RPC is retried from where it was.
	if limit := n.unstableLimit(); limit > 0 && len(req.Entries) > 0 &&
		n.log.unstableSize() >= limit {
		reply(ErrWriteBacklogFull)
		return
	}

	// Append new entries, truncating any conflicting suffix first. writeSeq is
	// the write this request queued, and stays 0 when it queued none: a
	// heartbeat, or a request whose entries this node already holds. What the
	// acknowledgement waits on is decided below, and is not always this.
	var writeSeq uint64
	for i, e := range req.Entries {
		existingTerm, err := n.log.termAt(e.Index)
		if err != nil {
			// Entry doesn't exist — append from here onward.
			writeSeq = n.log.append(req.Entries[i:])
			break
		}
		if existingTerm != e.Term {
			// Conflict: truncate and replace. Both operations are queued, and
			// the writer runs them in the order they were queued, so storage
			// can never end up with the new entries written behind the removal
			// of the old ones.
			if truncErr := n.log.truncateSuffix(n.stopCtx, e.Index); truncErr != nil {
				// The log may still hold entries the leader has overwritten.
				n.fail(truncErr, "truncate conflicting log suffix")
				reply(truncErr)
				return
			}
			// The membership in effect may have come from an entry that was
			// just discarded. Recompute it from the snapshot base and what is
			// left of the log before adopting anything new.
			if n.configIndex >= e.Index {
				if rebuildErr := n.rebuildMembership(n.stopCtx); rebuildErr != nil {
					reply(rebuildErr)
					return
				}
			}
			writeSeq = n.log.append(req.Entries[i:])
			break
		}
	}

	// Advance commitIndex, but never past the last index this request actually
	// covers. The leader's LeaderCommit refers to ITS log; it says nothing about
	// entries this follower holds beyond the range the request establishes as
	// matching. Clamping to our own last index instead would commit whatever
	// uncommitted suffix we still carry from a previous leader — entries the
	// current leader is about to overwrite — and applying those violates State
	// Machine Safety, permanently, because an applied index is never revisited.
	//
	// PrevLogIndex + len(Entries) is exactly the range the leader has vouched
	// for: the prefix matched the PrevLog check above, and the entries are the
	// leader's own.
	lastCovered := req.PrevLogIndex + Index(len(req.Entries))
	if newCommit := min(req.LeaderCommit, lastCovered); newCommit > n.commitIndex {
		n.setCommitIndex(newCommit)
		n.notifyApply()
	}

	resp.Success = true
	// Gated on whatever write will put the entries this request vouches for on
	// disk, and on the hard-state write when stepping up to this leader's term
	// produced one: an acknowledgement is a statement both that the entries
	// are on disk and that this node is at that term.
	//
	// That write is not always the one queued above. When a leader re-sends a
	// range it has not been acknowledged for -- after its own RPC timed out,
	// or because the entry cap makes the next batch identical to the last --
	// a follower still holding those entries unwritten finds every one of them
	// already in its log and appends nothing. The write to wait for is then
	// the one the first delivery queued, which is what writeSeqCovering finds.
	gate := writeSeq
	if len(req.Entries) > 0 {
		// Only for a request that actually carries entries. A heartbeat's
		// lastCovered is just its PrevLogIndex, which it makes no claim about
		// -- the leader never turns a heartbeat's acknowledgement into a match
		// index -- and after an election or a snapshot install that index can
		// sit in the part of the log still being written. Waiting for it would
		// put a follower's disk inside every heartbeat and every read barrier,
		// which is how a slow disk would come to cause the step-down that the
		// whole of this work exists to prevent.
		if covering := n.log.writeSeqCovering(lastCovered); covering > gate {
			gate = covering
		}
	}
	replyWhenDurable(gate)
}

// broadcastHeartbeat sends empty AppendEntries to all peers.
// All reads of node state happen in the event-loop goroutine before any
// goroutine is spawned, so there are no data races.
func (n *Node) broadcastHeartbeat() {
	for _, peer := range n.cfg.Peers {
		id := peer.ID
		// Build the request in the event-loop goroutine (safe: n is single-threaded here).
		prevIdx := n.nextIndex[id] - 1
		prevTerm, _ := n.log.termAt(prevIdx)
		req := &AppendEntriesRequest{
			GroupID:      n.cfg.GroupID,
			Term:         n.currentTerm,
			LeaderID:     n.cfg.ID,
			PrevLogIndex: prevIdx,
			PrevLogTerm:  prevTerm,
			LeaderCommit: n.commitIndex,
		}
		// Enqueue on the pump's size-1 channel. Non-blocking: if the pump is
		// still sending the previous heartbeat, the new one overwrites it (a
		// missed heartbeat only delays follower timer resets, not safety).
		ch := n.hbPumps[id]
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
		n.replicateToPeer(peer.ID)
	}
}

// replicateToPeer sends AppendEntries to a single peer. If the peer's
// nextIndex has fallen behind our snapshot boundary, we send a snapshot
// instead via sendSnapshotToPeer. Honours MaxInflightRPCs: if the peer
// already has the maximum number of in-flight RPCs, this is a no-op.
func (n *Node) replicateToPeer(peer NodeID) {
	nextIdx := n.nextIndex[peer]

	// The follower needs entries we no longer have — send the snapshot. The
	// test is what the log still holds, not where the snapshot boundary is:
	// with TrailingLogs set, entries below that boundary are often still
	// present and a snapshot would be wasted work.
	if !n.log.canDescribe(nextIdx - 1) {
		n.sendSnapshotToPeer(peer)
		return
	}

	// Backpressure: do not exceed MaxInflightRPCs per peer.
	if n.inflight[peer] >= n.cfg.MaxInflightRPCs {
		return
	}

	prevIdx := nextIdx - 1
	prevTerm, _ := n.log.termAt(prevIdx)

	var entries []LogEntry
	if n.log.lastLogIndex() >= nextIdx {
		var err error
		// Through the log, not straight from storage: the entries a leader
		// most wants to send are the ones it has just appended, which are the
		// ones least likely to be on disk yet.
		entries, err = n.log.entries(n.stopCtx, nextIdx, n.log.lastLogIndex()+1)
		if err != nil {
			n.logger.Error("replicateToPeer: read log entries", "peer", peer, "err", err)
			return
		}
		// Cap at MaxLogEntriesPerRPC.
		if len(entries) > n.cfg.MaxLogEntriesPerRPC {
			entries = entries[:n.cfg.MaxLogEntriesPerRPC]
		}
		entries = capByBytes(entries, n.cfg.MaxBytesPerRPC)
	}

	req := &AppendEntriesRequest{
		GroupID:      n.cfg.GroupID,
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

// capByBytes trims entries so their payloads fit within budget, always keeping
// at least one. An entry larger than the whole budget travels on its own:
// refusing to send it would stall replication for good, and the transport may
// still accept it.
func capByBytes(entries []LogEntry, budget uint64) []LogEntry {
	if budget == 0 || len(entries) == 0 {
		return entries
	}
	var total uint64
	for i, e := range entries {
		total += uint64(len(e.Command))
		if total > budget {
			if i == 0 {
				return entries[:1]
			}
			return entries[:i]
		}
	}
	return entries
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
		// A rejection with no conflict hint at all is not a log mismatch: a
		// follower that genuinely disagrees always reports where. An empty hint
		// means the response was synthesised somewhere between the peer and
		// here — a transport that reported a routing or handler failure as a
		// failed append, for instance. Treating it as a mismatch would drive
		// nextIndex to 1, which is at or below the snapshot boundary on any
		// leader that has ever compacted, so the leader would ship its entire
		// state machine to a follower that may be perfectly up to date. Leave
		// the peer's progress alone and let the next heartbeat retry.
		if r.conflictIndex == 0 && r.conflictTerm == 0 {
			n.logger.Warn("append rejected without a conflict hint; ignoring",
				"peer", r.peer, "term", r.term)
			return
		}

		// Back-track nextIndex using conflict hints.
		if r.conflictTerm != 0 {
			// Resume just past the last entry this leader holds in the term
			// the follower disagreed in, or at the follower's own hint when
			// the leader has nothing in that term at all. This used to walk
			// down from the end of the log reading each entry back from
			// storage, on the event loop, for as far as the follower had
			// fallen behind.
			newNext := r.conflictIndex
			if idx, ok := n.log.lastIndexOfTerm(r.conflictTerm); ok {
				newNext = idx + 1
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
			confirmed = hasMajorityAck(n.readBatchAcks, n.cfg.Peers, true, n.cfg.Voter)
		} else {
			confirmed = hasMajorityAck(n.readBatchAcks, n.jointOld, true, n.jointSelfVoterOld) &&
				hasMajorityAck(n.readBatchAcks, n.jointNew, n.jointIncludeSelf, n.jointSelfVoter)
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
func hasMajorityAck(acks map[NodeID]bool, members []PeerConfig, includeSelf, selfVoter bool) bool {
	count := 0
	if includeSelf && selfVoter {
		count = 1
	}
	for _, m := range members {
		if m.Voter && acks[m.ID] {
			count++
		}
	}
	total := 0
	for _, m := range members {
		if m.Voter {
			total++
		}
	}
	if includeSelf && selfVoter {
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
func (n *Node) replicatedOnMajority(idx Index, members []PeerConfig, includeSelf, selfVoter bool) bool {
	count := 0
	// Self counts only as far as its own storage has got. A leader's log is
	// one of the replicas the quorum is drawn from, and it is no more entitled
	// than a follower is to vouch for an entry it has not written: committing
	// on a quorum that includes an entry living only in this leader's memory
	// would apply it everywhere, and then lose it here if the machine died
	// before the write landed.
	if includeSelf && selfVoter && idx <= n.log.stableIndex() {
		count = 1
	}
	for _, p := range members {
		if p.Voter && n.matchIndex[p.ID] >= idx {
			count++
		}
	}
	total := 0
	for _, p := range members {
		if p.Voter {
			total++
		}
	}
	if includeSelf && selfVoter {
		total++
	}
	return count > total/2
}

// maybeAdvanceCommit checks whether a new index can be committed (replicated
// on a majority) and advances commitIndex if so. During joint consensus a
// commit requires a majority of both C_old and C_new independently.
func (n *Node) maybeAdvanceCommit() {
	if n.state != Leader || n.termStartIndex == 0 {
		return
	}

	// Find the highest N such that the required quorum have matchIndex >= N and
	// log[N].term == currentTerm.
	//
	// The scan stops at the first index of this leader's term rather than
	// walking down to commitIndex: an entry from an earlier term can never be
	// committed by replica count (Raft 5.4.2), so there is nothing below that
	// point worth testing. Everything at or above it is this leader's own
	// entry, in the current term, which is what makes the term check
	// unnecessary -- and with it the storage read that used to be done for
	// every index on every acknowledgement.
	lo := n.commitIndex + 1
	if n.termStartIndex > lo {
		lo = n.termStartIndex
	}
	for idx := n.log.lastLogIndex(); idx >= lo; idx-- {
		var committed bool
		if n.jointOld == nil {
			// Normal single-config majority. Self is always a member.
			committed = n.replicatedOnMajority(idx, n.cfg.Peers, true, n.cfg.Voter)
		} else {
			// Joint consensus: both C_old and C_new must independently have a
			// majority (Raft §6). jointOld and jointNew never include self.
			// Self is always in C_old (it is the leader); for C_new it is
			// only counted when jointIncludeSelf is true (i.e. self is
			// retained in the new membership).
			committed = n.replicatedOnMajority(idx, n.jointOld, true, n.jointSelfVoterOld) &&
				n.replicatedOnMajority(idx, n.jointNew, n.jointIncludeSelf, n.jointSelfVoter)
		}
		if committed {
			n.setCommitIndex(idx)
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
					n.startReadBatch()
				}
			}
			break
		}
	}
}
