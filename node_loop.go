package raft

import (
	"fmt"
	"time"
)

// run is the single goroutine that exclusively owns all mutable Node state.
// Every other goroutine communicates with it only through channels.
func (n *Node) run() {
	defer close(n.doneCh)
	for {
		select {
		case <-n.stopCh:
			n.drainPending(ErrStopped)
			// Let storage finish what it has already been given, then
			// release whatever that completed and fail the rest. Anything
			// still waiting is answered either way, so no caller is left
			// holding a request that will never come back.
			n.writer.close()
			n.handleWriteCompletions()
			n.failDeferredWrites(ErrStopped)
			n.failSnapInstallAck(ErrStopped)
			// Drain any results that applyLoop already sent but the event loop
			// has not yet processed. Without this, the clientTable and
			// pendingConfigIndex updates for applied entries are lost, and
			// clients whose entries committed see ErrStopped even though their
			// command reached the state machine.
			// applyLoop exits on stopCh via its own send-selects; once
			// applyDoneCh is closed no further results will arrive.
			for {
				select {
				case ar := <-n.applyResultCh:
					n.handleApplyResult(&ar)
				case <-n.applyDoneCh:
					// applyLoop has fully exited; drain any final results it
					// buffered before seeing stopCh.
					for {
						select {
						case ar := <-n.applyResultCh:
							n.handleApplyResult(&ar)
						default:
							return
						}
					}
				}
			}

		case <-n.tickCh:
			n.tick()

		case env := <-n.rpcCh:
			n.handleRPCEnvelope(env)

		case prop := <-n.proposeCh:
			props := []proposeMsg{prop}
			// Greedily drain the channel to batch proposals. This amortizes the
			// cost of the durable storage write (fsync).
		batchDrain:
			for len(props) < 1024 {
				select {
				case p := <-n.proposeCh:
					props = append(props, p)
				default:
					break batchDrain
				}
			}
			n.handleProposals(props)

		case ri := <-n.readIndexCh:
			n.handleReadIndex(ri)

		case tm := <-n.transferCh:
			n.handleLeadershipTransfer(tm)

		case ar := <-n.applyResultCh:
			n.handleApplyResult(&ar)

		case sr := <-n.snapshotResultCh:
			n.handleSnapshotResult(&sr)

		case <-n.writer.completions():
			n.handleWriteCompletions()
		}

		// One announcement per turn, after the whole transition has been
		// applied. See announceLeadership.
		n.announceLeadership()
	}
}

// ---- Tick ------------------------------------------------------------------

// tick is called once per logical clock tick. It drives election and
// heartbeat timeouts.
func (n *Node) tick() {
	switch n.state {
	case Follower, Candidate, PreCandidate:
		if n.cfg.Voter {
			n.electionElapsed++
			if n.electionElapsed >= n.electionTimeout {
				n.resetElectionTimeout()
				n.triggerElection()
			}
		}

	case Leader:
		n.heartbeatElapsed++
		if n.heartbeatElapsed >= n.heartbeatTimeout {
			n.heartbeatElapsed = 0
			n.broadcastHeartbeat()
			// If there are pending ReadIndex requests, re-send the barrier so
			// that a dropped barrier heartbeat does not stall reads forever.
			// NOTE (false positive): this re-send is intentional. broadcastReadBarrier
			// increments readBatchGen each time only on the *first* request in a
			// batch; subsequent re-sends use the same gen so old ACKs still count.
			// Guard on leaderNopCommitted: reads queued before the nop commits
			// are dispatched by maybeAdvanceCommit, not the heartbeat tick.
			if n.leaderNopCommitted && len(n.pendingReads) > 0 {
				n.broadcastReadBarrier()
			}
		}
		// Leadership transfer timeout: if the target hasn't started an election
		// within one election timeout, abort the transfer.
		if n.transferTarget != "" {
			n.transferElapsed++
			if n.transferElapsed >= n.electionTimeout {
				n.logger.Warn("leadership transfer timed out", "target", n.transferTarget)
				n.transferTarget = ""
				n.transferElapsed = 0
			}
		}
		// Preferred-leader pinning: if another node is designated as the
		// preferred leader and we are currently leading, initiate a transfer.
		// Guard on leaderNopCommitted so we don't transfer before the cluster
		// is in a stable state, and on transferTarget == "" so we don't start
		// a second transfer while one is already in progress.
		if n.cfg.PreferredLeader != "" &&
			n.cfg.PreferredLeader != n.cfg.ID &&
			n.transferTarget == "" &&
			n.leaderNopCommitted {
			respCh := make(chan error, 1)
			n.handleLeadershipTransfer(leadershipTransferMsg{
				target: n.cfg.PreferredLeader,
				respCh: respCh,
			})
			<-respCh // always buffered; drain without blocking
		}

		// Check-quorum: step down if we have not heard from a majority of peers
		// within the last electionTimeout ticks. This prevents a partitioned
		// leader from continuing to reject client requests (or accepting proposals
		// that never commit) after losing contact with the rest of the cluster.
		// Single-node clusters are trivially their own quorum and are excluded.
		if n.cfg.CheckQuorum && len(n.cfg.Peers) > 0 {
			n.leaderQuorumElapsed++
			if n.leaderQuorumElapsed >= n.electionTimeout {
				// During joint consensus both configs must have a quorum of
				// recently-heard-from peers; lacking one means we may not be
				// the legitimate leader for that config group.
				var hasQuorum bool
				if n.jointOld == nil {
					hasQuorum = hasMajorityAck(n.quorumAcks, n.cfg.Peers, true, n.cfg.Voter)
				} else {
					hasQuorum = hasMajorityAck(n.quorumAcks, n.jointOld, true, n.jointSelfVoterOld) &&
						hasMajorityAck(n.quorumAcks, n.jointNew, n.jointIncludeSelf, n.jointSelfVoter)
				}
				if !hasQuorum {
					n.logger.Warn("check-quorum: no majority of peers responded; stepping down")
					n.becomeFollower(n.currentTerm, "")
					return
				}
				// Quorum confirmed — reset the window.
				n.quorumAcks = make(map[NodeID]bool)
				n.leaderQuorumElapsed = 0
			}
		}
	}
}

// ---- RPC dispatch ----------------------------------------------------------

// handleRPCEnvelope routes an inbound RPC to the appropriate handler and
// sends the response back on env.respCh.
func (n *Node) handleRPCEnvelope(env rpcEnvelope) {
	var resp rpcResponse
	switch req := env.req.(type) {
	case *RequestVoteRequest:
		n.handleRequestVote(req, env.respCh)
		return // a granted vote waits for the hard-state write
	case *AppendEntriesRequest:
		n.handleAppendEntries(req, env.respCh)
		return // the acknowledgement waits for the log write
	case *InstallSnapshotRequest:
		n.handleInstallSnapshot(req, env.respCh)
		return // the reply carries this node's term
	case *TimeoutNowRequest:
		n.handleTimeoutNow(req, env.respCh)
		return // the reply carries this node's term
	case *ReadIndexRequest:
		n.handleReadIndexRPC(req, env.respCh)
		return // response sent asynchronously
	case *voteResult:
		n.handleVoteResult(req)
	case *appendResult:
		n.handleAppendResult(req)
	case *peerReachability:
		if req.reachable {
			n.markPeerUp(req.peer)
		} else {
			n.markPeerDown(req.peer)
		}
	case *installSnapshotResult:
		n.handleInstallSnapshotResult(req)
	case *snapInstallResult:
		n.handleSnapInstallResult(req)
	case *progressRequest:
		resp = rpcResponse{resp: n.replicationProgress()}
	default:
		resp = rpcResponse{err: fmt.Errorf("raft: unknown RPC type %T", req)}
	}
	if env.respCh != nil {
		env.respCh <- resp
	}
}

// ---- Client proposals ------------------------------------------------------

// handlePropose is called when a client sends a command to the leader.
func (n *Node) handleProposals(props []proposeMsg) {
	if n.state != Leader {
		for _, prop := range props {
			p := promise[[]byte]{ch: prop.respCh}
			p.reject(&NotLeaderError{Leader: n.leaderID})
			n.reportProposal(prop.submitted, false)
		}
		return
	}
	if n.transferTarget != "" {
		for _, prop := range props {
			p := promise[[]byte]{ch: prop.respCh}
			p.reject(ErrLeadershipTransferInProgress)
			n.reportProposal(prop.submitted, false)
		}
		return
	}

	// The leader holds entries in memory until storage has written them.
	// Accepting more while that backlog is at its limit would turn a slow disk
	// into an unbounded one, so the budget is spent across this batch and
	// whatever is already waiting.
	limit := n.unstableLimit()
	backlog := n.log.unstableSize()

	entries := make([]LogEntry, 0, len(props)+1)

	// The group's bound on the exactly-once table has to be agreed before
	// the first entry that table records, or each replica would apply that
	// entry under its own configuration and evict differently from there on.
	// So a leader whose group has no agreed bound yet puts one in the log
	// ahead of the first ProposeOnce entry, from its own configuration; every
	// replica adopts it at apply time, before the entry that needs it.
	if !n.clientTableCapReplicated && n.capEntryPending == 0 {
		for _, prop := range props {
			if !isDedupCmd(prop.cmd) {
				continue
			}
			idx := n.log.lastLogIndex() + 1
			entries = append(entries, LogEntry{
				Index:   idx,
				Term:    n.currentTerm,
				Command: encodeClientTableCapEntry(n.cfg.MaxClientTableSize),
			})
			n.capEntryPending = idx
			// The log now knows of a bound. The table itself is unchanged:
			// the value is this node's own, and it takes effect in apply
			// order like every other cap entry.
			n.clientTableCapReplicated = true
			break
		}
	}

	for _, prop := range props {
		if isConfigEntry(prop.cmd) && n.pendingConfigIndex != 0 {
			p := promise[[]byte]{ch: prop.respCh}
			p.reject(ErrConfigChangeInProgress)
			n.reportProposal(prop.submitted, false)
			continue
		}

		// Dedup check for ProposeOnce commands.
		if isDedupCmd(prop.cmd) {
			clientID, seqNum, _, err := decodeDedupCmd(prop.cmd)
			if err == nil {
				if cached, ok := n.clientTable.get(clientID); ok {
					if seqNum < cached.seqNum {
						p := promise[[]byte]{ch: prop.respCh}
						p.reject(ErrObsoleteSeqNum)
						n.reportProposal(prop.submitted, false)
						continue
					}
					if seqNum == cached.seqNum {
						// Exact duplicate — return the cached result without re-appending.
						p := promise[[]byte]{ch: prop.respCh}
						p.resolve(cached.result)
						n.reportProposal(prop.submitted, true)
						continue
					}
					// seqNum > cached.seqNum — new request; fall through to normal propose.
				}
			}
		}

		// Checked last, so that a proposal answered from the dedup table or
		// refused for another reason is not turned away for a backlog it was
		// never going to add to. A backlog of nothing always accepts, whatever
		// the command's size: an oversized command is refused by
		// checkProposalSize, and refusing one here instead would be a stall
		// with no error to explain it.
		if limit > 0 && backlog > 0 && backlog+len(prop.cmd) > limit {
			p := promise[[]byte]{ch: prop.respCh}
			p.reject(ErrWriteBacklogFull)
			n.reportProposal(prop.submitted, false)
			continue
		}

		idx := n.log.lastLogIndex() + Index(len(entries)) + 1
		entry := LogEntry{Index: idx, Term: n.currentTerm, Command: prop.cmd}
		entries = append(entries, entry)
		n.pending[idx] = pendingProposal{
			promise:   promise[[]byte]{ch: prop.respCh},
			submitted: prop.submitted,
		}
		if isConfigEntry(prop.cmd) {
			n.pendingConfigIndex = idx
		}
		backlog += len(prop.cmd)
	}

	if len(entries) > 0 {
		// The entries are in the log now and go out to followers now. They are
		// not counted towards a commit quorum until the write lands, which is
		// what maybeAdvanceCommit consults the durable point for; so this
		// overlaps the leader's own fsync with the round trip to its followers
		// instead of placing one after the other.
		seq := n.log.append(entries)
		indices := make([]Index, len(entries))
		for i := range entries {
			indices[i] = entries[i].Index
		}
		n.afterWrite(seq, nil, func(err error) {
			// A leader that cannot write its own log cannot make progress, and
			// entries it believes it appended may or may not be there.
			for _, idx := range indices {
				if p, ok := n.pending[idx]; ok {
					p.promise.reject(fmt.Errorf("propose: append: %w", err))
					n.reportProposal(p.submitted, false)
					delete(n.pending, idx)
				}
				if n.pendingConfigIndex == idx {
					n.pendingConfigIndex = 0
				}
				if n.capEntryPending == idx {
					n.capEntryPending = 0
				}
			}
		})
		n.replicateToFollowers()
		// For single-node clusters (no peers) the entry is replicated on a
		// majority as soon as it is durable; nothing can be committed here
		// yet, but the call is harmless and keeps the path uniform.
		n.maybeAdvanceCommit()
	}
}

// ---- Apply results ---------------------------------------------------------

// handleApplyResult is called when the apply goroutine finishes applying an
// entry. It advances lastApplied, resolves any waiting client promise, and
// may trigger a snapshot if the log has grown past the configured threshold.
func (n *Node) handleApplyResult(ar *applyResult) {
	if ar.index > n.lastApplied {
		n.lastApplied = ar.index
		n.atomicLastApplied.Store(uint64(ar.index))
		select {
		case n.applyAdvancedCh <- struct{}{}:
		default:
		}
	}

	// Apply config changes to Raft's own peer list.
	if ar.configCmd != nil {
		n.applyConfigChange(ar.configCmd, ar.index)
		if n.pendingConfigIndex == ar.index {
			n.pendingConfigIndex = 0
		}
	}

	// Record the outcome of a ProposeOnce entry so a retry gets the same answer.
	//
	// Only a successful apply is recorded. Caching a failure as though it were
	// a result would answer the retry with a nil error and a nil result, so a
	// caller whose command the state machine rejected would be told it
	// succeeded. A failed command left unrecorded is simply re-run, which is
	// the correct outcome for a command that never took effect.
	if isDedupCmd(ar.cmd) && ar.err == nil {
		if clientID, seqNum, _, err := decodeDedupCmd(ar.cmd); err == nil {
			if cached, ok := n.clientTable.get(clientID); !ok || seqNum >= cached.seqNum {
				if forgotten, evicted := n.clientTable.put(clientID,
					clientEntry{seqNum: seqNum, result: ar.val}); evicted {
					n.reportClientForgotten(forgotten)
				}
				n.syncClientTableSize()
			}
		}
	}

	p, ok := n.pending[ar.index]
	if ok {
		if ar.err != nil {
			p.promise.reject(ar.err)
		} else {
			p.promise.resolve(ar.val)
		}
		n.reportProposal(p.submitted, ar.err == nil)
		delete(n.pending, ar.index)
	}
	n.maybeSnapshot()
}

// ---- Commit notification and proposal draining -----------------------------

// notifyApply sends the applicable commit index to the apply goroutine,
// dropping the send if the goroutine is busy (the channel is size-1;
// the goroutine will read the latest value when it next wakes).
//
// What is applicable is not the commit index but the part of it that is on
// disk. A follower learns its commit index from the leader and may hold the
// entries it covers in memory only; the apply loop reads entries back from
// storage, so being told about them early would have it apply whatever storage
// happens to hold at those indices -- nothing, or entries from a history this
// node has already discarded. The rest follows when the write lands, because
// every write completion calls this again.
func (n *Node) notifyApply() {
	applicable := n.commitIndex
	if stable := n.log.stableIndex(); stable < applicable {
		applicable = stable
	}
	if applicable == 0 {
		return
	}
	select {
	case n.commitNotifyCh <- applicable:
	default:
		// Channel already has a value; drain the stale index and replace it
		// with the latest so the apply goroutine always wakes to the current
		// commitIndex. The non-blocking drain handles the (rare) case where
		// the apply goroutine races to drain the channel between us hitting
		// the first default and arriving here. After the drain (regardless of
		// whether we drained or the apply goroutine did), the channel is empty
		// and the blocking send below always succeeds immediately.
		select {
		case <-n.commitNotifyCh:
		default:
		}
		n.commitNotifyCh <- applicable
	}
}

// drainPending rejects all in-flight proposals with the given error.
func (n *Node) drainPending(err error) {
	for idx, p := range n.pending {
		p.promise.reject(err)
		n.reportProposal(p.submitted, false)
		delete(n.pending, idx)
	}
	n.drainPendingReads(err)
}

// drainPendingReads rejects all outstanding ReadIndex futures, both those
// waiting on the round in flight and those waiting for the next one.
func (n *Node) drainPendingReads(err error) {
	for _, p := range n.pendingReads {
		p.reject(err)
	}
	clear(n.pendingReads)
	n.pendingReads = n.pendingReads[:0]

	for _, p := range n.waitingReads {
		p.reject(err)
	}
	clear(n.waitingReads)
	n.waitingReads = n.waitingReads[:0]

	n.readBatchAcks = nil
}

// reportClientForgotten announces that a client was dropped from the
// exactly-once table. Called from the event loop.
//
// This is not a warning about pressure. The table is the only record of which
// requests have already been carried out, so a client that is no longer in it
// will have its next retry executed a second time -- and that duplicate is
// undetectable after the fact: it applies cleanly, the log stays consistent,
// and every replica agrees, because every replica evicted the same entry. The
// eviction is the last moment anything can be said about it, so it is said
// three ways: to Metrics, to Events, and to the log for the many deployments
// that wire up neither.
//
// Only the event loop reports. The apply loop keeps its own copy of the table
// and evicts in lockstep with this one, by construction -- both are driven by
// the same entries in the same order -- so reporting there as well would
// double every count.
func (n *Node) reportClientForgotten(clientID NodeID) {
	n.clientsForgotten++

	if cm, ok := n.cfg.Metrics.(ClientTableMetrics); ok {
		cm.ClientForgotten(n.cfg.ID, clientID)
	}
	n.emit(&Event{Type: EventClientForgotten, Client: clientID})

	// The first one is always logged: it is the transition from a cluster that
	// keeps its exactly-once promise to one that does not, and an operator who
	// sees nothing else should see that. After it, a table one entry too small
	// evicts on every single proposal, so the rest are summarised.
	now := n.now()
	if n.clientsForgotten > 1 && now.Sub(n.lastForgetLog) < time.Minute {
		return
	}
	n.lastForgetLog = now
	n.logger.Warn("client dropped from the exactly-once table; a retry from it will run twice",
		"client", clientID,
		"maxClientTableSize", n.cfg.MaxClientTableSize,
		"forgottenTotal", n.clientsForgotten)
}
