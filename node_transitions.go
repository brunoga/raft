package raft

import "time"

// ---- State transitions -----------------------------------------------------

// becomeFollower transitions this node to Follower for the given term.
// If the term is greater than currentTerm the voted-for is cleared and the
// new term is persisted. Use applyFollowerTransition when saveTerm has
// already been called by the caller (e.g. handleRequestVote).
func (n *Node) becomeFollower(term Term, leaderID NodeID) {
	if term > n.currentTerm {
		if err := n.saveTerm(term, ""); err != nil {
			n.logger.Error("becomeFollower: saveTerm", "err", err)
		}
	}
	n.applyFollowerTransition(leaderID)
}

// applyFollowerTransition performs the in-memory follower state transition
// without persisting the term. The caller must have already called saveTerm
// for any term change before invoking this.
func (n *Node) applyFollowerTransition(leaderID NodeID) {
	changed := n.state != Follower || n.leaderID != leaderID
	prev := n.state
	n.setState(Follower)
	n.setLeaderID(leaderID)
	n.nextIndex = nil
	n.matchIndex = nil
	n.inflight = nil
	n.snapshotInflight = nil
	n.transferTarget = ""
	n.transferElapsed = 0
	n.pendingConfigIndex = 0
	n.leaderNopCommitted = false
	// Reject all in-flight client proposals. If an uncommitted entry is later
	// overwritten by a new leader at the same log index, the apply goroutine
	// would resolve the old promise with the new entry's result — giving the
	// client a response for a command it did not submit. Draining here gives
	// callers a clean ErrNotLeader they can retry against the new leader.
	// Committed entries are already resolved by handleApplyResult before
	// step-down triggers, so draining here only affects uncommitted proposals.
	for idx, p := range n.pending {
		p.reject(&NotLeaderError{Leader: n.leaderID})
		delete(n.pending, idx)
	}
	n.drainPendingReads(&NotLeaderError{Leader: n.leaderID})
	n.readBatchAcks = nil
	n.leaseExpiry = time.Time{}   // invalidate the read lease on step-down
	n.leaseSendTime = time.Time{} // clear so the next term doesn't reuse an old send time
	n.quorumAcks = nil
	n.leaderQuorumElapsed = 0
	if n.hbPumps != nil {
		n.stopHBPumps() // signal pump goroutines to exit
	}
	if changed {
		// Cancel any in-progress streaming snapshot install: it came from a
		// different leader (or a leader in a lower term) and can never be
		// completed. cancelFn is safe to call even after the goroutine has
		// already exited.
		if n.pendingSnap != nil {
			n.pendingSnap.cancelFn()
			n.pendingSnap = nil
		}
	}
	n.resetElectionTimeout()
	if changed {
		n.logger.Info("became follower", "term", n.currentTerm, "leader", leaderID)
		if n.cfg.Metrics != nil {
			n.cfg.Metrics.StateChange(n.cfg.ID, prev, Follower, n.currentTerm)
		}
	}
}

// becomePreCandidate starts a pre-vote round without incrementing the term.
// If a majority responds positively, the node proceeds to becomeCandidate.
func (n *Node) becomePreCandidate() {
	prev := n.state
	n.setState(PreCandidate)
	n.setLeaderID("")
	n.nextIndex = nil
	n.matchIndex = nil
	n.receivedVoteSet = make(map[NodeID]bool)
	n.resetElectionTimeout()
	if prev != PreCandidate {
		n.logger.Info("became pre-candidate", "term", n.currentTerm)
		if n.cfg.Metrics != nil {
			n.cfg.Metrics.StateChange(n.cfg.ID, prev, PreCandidate, n.currentTerm)
		}
	}

	// A single-node cluster has nothing to pre-vote on.
	if n.isSingleVoter() {
		n.becomeCandidate()
		return
	}

	n.broadcastRequestVote(true)
}

// becomeCandidate transitions this node to Candidate, increments the term,
// votes for itself, and broadcasts RequestVote RPCs.
func (n *Node) becomeCandidate() {
	prev := n.state
	newTerm := n.currentTerm + 1
	if err := n.saveTerm(newTerm, n.cfg.ID); err != nil {
		n.logger.Error("becomeCandidate: saveTerm", "err", err)
		return
	}
	n.setState(Candidate)
	n.setLeaderID("")
	n.nextIndex = nil
	n.matchIndex = nil
	n.receivedVoteSet = make(map[NodeID]bool)
	n.resetElectionTimeout()
	n.logger.Info("became candidate", "term", n.currentTerm)
	if n.cfg.Metrics != nil {
		n.cfg.Metrics.StateChange(n.cfg.ID, prev, Candidate, n.currentTerm)
	}

	// A single-node cluster wins immediately.
	if n.isSingleVoter() {
		n.becomeLeader()
		return
	}

	n.broadcastRequestVote(false)
}

// becomeLeader transitions this node to Leader, initialises per-peer tracking
// state, and immediately sends a heartbeat to assert leadership.
func (n *Node) becomeLeader() {
	prev := n.state
	n.setState(Leader)
	n.setLeaderID(n.cfg.ID)
	n.heartbeatElapsed = 0
	n.leaderNopCommitted = false
	n.quorumAcks = make(map[NodeID]bool, len(n.cfg.Peers))
	n.leaderQuorumElapsed = 0

	// Initialise nextIndex to leader's last log index + 1.
	nextIdx := n.log.lastLogIndex() + 1
	n.nextIndex = make(map[NodeID]Index, len(n.cfg.Peers))
	n.matchIndex = make(map[NodeID]Index, len(n.cfg.Peers))
	n.inflight = make(map[NodeID]int, len(n.cfg.Peers))
	n.snapshotInflight = make(map[NodeID]bool, len(n.cfg.Peers))
	for _, peer := range n.cfg.Peers {
		n.nextIndex[peer.ID] = nextIdx
		n.matchIndex[peer.ID] = 0
	}

	n.logger.Info("became leader", "term", n.currentTerm)
	if n.cfg.Metrics != nil {
		n.cfg.Metrics.StateChange(n.cfg.ID, prev, Leader, n.currentTerm)
	}

	// Start persistent per-peer heartbeat pumps before sending the first
	// heartbeat, so broadcastHeartbeat can use them immediately.
	n.startHBPumps()

	// Append a no-op entry to commit any previous-term entries (§8).
	noop := LogEntry{
		Index:   n.log.lastLogIndex() + 1,
		Term:    n.currentTerm,
		Command: nil,
	}
	if err := n.log.appendOne(n.stopCtx, noop); err != nil {
		// If we can't write to storage, we cannot commit anything as leader
		// and leaderNopCommitted would never become true, permanently stalling
		// all reads and proposals. Step down so another node can be elected.
		n.logger.Error("becomeLeader: append noop failed, stepping down", "err", err)
		n.applyFollowerTransition("")
		return
	}

	n.broadcastHeartbeat()
	// For single-node clusters the noop commits immediately (quorum = 1).
	// For multi-node clusters this advances the commit index once the noop
	// (or any existing same-term entry) has been replicated to a majority.
	// Calling this here ensures that a restarted single-node leader
	// re-applies previously committed entries without requiring a new Propose.
	n.maybeAdvanceCommit()

	// If we became leader while a joint-consensus reconfiguration was in
	// progress (jointOld != nil), the previous leader may not have appended
	// the finalise entry yet. Re-append it to ensure the second phase
	// completes. Duplicates are harmless — applyConfigChange is idempotent.
	if n.jointOld != nil {
		n.appendFinaliseEntry(n.jointNew, n.jointIncludeSelf, n.jointSelfVoter)
	}
}
