package raft

import "fmt"

// run is the single goroutine that exclusively owns all mutable Node state.
// Every other goroutine communicates with it only through channels.
func (n *Node) run() {
	defer close(n.doneCh)
	for {
		select {
		case <-n.stopCh:
			n.drainPending(ErrStopped)
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
			for len(props) < 1024 {
				select {
				case p := <-n.proposeCh:
					props = append(props, p)
				default:
					goto process
				}
			}
		process:
			n.handleProposals(props)

		case ri := <-n.readIndexCh:
			n.handleReadIndex(ri)

		case tm := <-n.transferCh:
			n.handleLeadershipTransfer(tm)

		case ar := <-n.applyResultCh:
			n.handleApplyResult(&ar)

		case sr := <-n.snapshotResultCh:
			n.handleSnapshotResult(sr)
		}
	}
}

// ---- Tick ------------------------------------------------------------------

// tick is called once per logical clock tick. It drives election and
// heartbeat timeouts.
func (n *Node) tick() {
	switch n.state {
	case Follower, Candidate, PreCandidate:
		n.electionElapsed++
		if n.electionElapsed >= n.electionTimeout {
			n.resetElectionTimeout()
			n.triggerElection()
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
					hasQuorum = hasMajorityAck(n.quorumAcks, n.cfg.Peers, true)
				} else {
					hasQuorum = hasMajorityAck(n.quorumAcks, n.jointOld, true) &&
						hasMajorityAck(n.quorumAcks, n.jointNew, n.jointIncludeSelf)
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
		r, err := n.handleRequestVote(req)
		resp = rpcResponse{resp: r, err: err}
	case *AppendEntriesRequest:
		r, err := n.handleAppendEntries(req)
		resp = rpcResponse{resp: r, err: err}
	case *InstallSnapshotRequest:
		r, err := n.handleInstallSnapshot(req)
		resp = rpcResponse{resp: r, err: err}
	case *TimeoutNowRequest:
		r, err := n.handleTimeoutNow(req)
		resp = rpcResponse{resp: r, err: err}
	case *ReadIndexRequest:
		n.handleReadIndexRPC(req, env.respCh)
		return // response sent asynchronously
	case *voteResult:
		n.handleVoteResult(req)
	case *appendResult:
		n.handleAppendResult(req)
	case *installSnapshotResult:
		n.handleInstallSnapshotResult(req)
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
		}
		return
	}
	if n.transferTarget != "" {
		for _, prop := range props {
			p := promise[[]byte]{ch: prop.respCh}
			p.reject(ErrLeadershipTransferInProgress)
		}
		return
	}

	entries := make([]LogEntry, 0, len(props))
	for _, prop := range props {
		if isConfigEntry(prop.cmd) && n.pendingConfigIndex != 0 {
			p := promise[[]byte]{ch: prop.respCh}
			p.reject(ErrConfigChangeInProgress)
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
						continue
					}
					if seqNum == cached.seqNum {
						// Exact duplicate — return the cached result without re-appending.
						p := promise[[]byte]{ch: prop.respCh}
						p.resolve(cached.result)
						continue
					}
					// seqNum > cached.seqNum — new request; fall through to normal propose.
				}
			}
		}

		idx := n.log.lastLogIndex() + Index(len(entries)) + 1
		entry := LogEntry{Index: idx, Term: n.currentTerm, Command: prop.cmd}
		entries = append(entries, entry)
		n.pending[idx] = promise[[]byte]{ch: prop.respCh}
		if isConfigEntry(prop.cmd) {
			n.pendingConfigIndex = idx
		}
	}

	if len(entries) > 0 {
		if err := n.log.append(n.stopCtx, entries); err != nil {
			// Fail all in-flight entries in this batch.
			for _, entry := range entries {
				if p, ok := n.pending[entry.Index]; ok {
					p.reject(fmt.Errorf("propose: append: %w", err))
					delete(n.pending, entry.Index)
				}
				if n.pendingConfigIndex == entry.Index {
					n.pendingConfigIndex = 0
				}
			}
			return
		}
		n.replicateToFollowers()
		// For single-node clusters (no peers) the entry is immediately replicated
		// on a majority (self), so try to advance commitIndex right away.
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
		n.applyConfigChange(ar.configCmd)
		if n.pendingConfigIndex == ar.index {
			n.pendingConfigIndex = 0
		}
	}

	// Update the client dedup table for ProposeOnce entries. The LRU evicts
	// the least-recently-used client automatically on put() when over cap.
	if isDedupCmd(ar.cmd) {
		if clientID, seqNum, _, err := decodeDedupCmd(ar.cmd); err == nil {
			if cached, ok := n.clientTable.get(clientID); !ok || seqNum >= cached.seqNum {
				n.clientTable.put(clientID, clientEntry{seqNum: seqNum, result: ar.val})
			}
		}
	}

	p, ok := n.pending[ar.index]
	if ok {
		if ar.err != nil {
			p.reject(ar.err)
		} else {
			p.resolve(ar.val)
		}
		delete(n.pending, ar.index)
	}
	n.maybeSnapshot()
}

// ---- Commit notification and proposal draining -----------------------------

// notifyApply sends the current commitIndex to the apply goroutine,
// dropping the send if the goroutine is busy (the channel is size-1;
// the goroutine will read the latest value when it next wakes).
func (n *Node) notifyApply() {
	select {
	case n.commitNotifyCh <- n.commitIndex:
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
		n.commitNotifyCh <- n.commitIndex
	}
}

// drainPending rejects all in-flight proposals with the given error.
func (n *Node) drainPending(err error) {
	for idx, p := range n.pending {
		p.reject(err)
		delete(n.pending, idx)
	}
	n.drainPendingReads(err)
}

// drainPendingReads rejects all pending ReadIndex futures.
func (n *Node) drainPendingReads(err error) {
	for _, p := range n.pendingReads {
		p.reject(err)
	}
	n.pendingReads = n.pendingReads[:0]
	n.readBatchAcks = nil
}
