package raft

import (
	"context"
	"fmt"
)

// ---- Snapshot handling --------------------------------------------------------

func (n *Node) handleInstallSnapshot(req *InstallSnapshotRequest) (*InstallSnapshotResponse, error) {
	resp := &InstallSnapshotResponse{Term: n.currentTerm}

	if req.Term < n.currentTerm {
		return resp, nil
	}
	n.becomeFollower(req.Term, req.LeaderID)
	resp.Term = n.currentTerm

	// Ignore stale or duplicate snapshots (applies to all chunks).
	if req.LastIncludedIndex <= n.log.snapMeta.LastIncludedIndex {
		n.pendingSnap = nil // discard any partially buffered transfer for this index
		return resp, nil
	}

	// ---- Chunked reassembly ----
	// Chunks arrive in order (Offset monotonically increasing, Done on last).
	// We buffer them in n.pendingSnap until Done=true, then apply the full blob.

	if req.Offset == 0 {
		// First (or only) chunk: start a fresh partial buffer.
		n.pendingSnap = &partialSnapshot{
			meta: SnapshotMeta{
				LastIncludedIndex: req.LastIncludedIndex,
				LastIncludedTerm:  req.LastIncludedTerm,
			},
			data:           append([]byte(nil), req.Data...),
			expectedOffset: int64(len(req.Data)),
		}
	} else {
		// Continuation chunk: validate it belongs to the current transfer.
		if n.pendingSnap == nil ||
			n.pendingSnap.meta.LastIncludedIndex != req.LastIncludedIndex ||
			n.pendingSnap.expectedOffset != req.Offset {
			// Out-of-order or stale chunk — drop the partial buffer; the leader
			// will restart the transfer after it detects the missed ACK.
			n.pendingSnap = nil
			return resp, nil
		}
		n.pendingSnap.data = append(n.pendingSnap.data, req.Data...)
		n.pendingSnap.expectedOffset += int64(len(req.Data))
	}

	if !req.Done {
		// More chunks to come; nothing further to do this round.
		return resp, nil
	}

	// We have the complete snapshot blob in n.pendingSnap.data.
	fullData := n.pendingSnap.data
	meta := n.pendingSnap.meta
	n.pendingSnap = nil

	// Persist, then unwrap to restore the client table and SM data separately.
	if err := n.cfg.Storage.SaveSnapshot(n.stopCtx, meta, fullData); err != nil {
		return resp, fmt.Errorf("installSnapshot: SaveSnapshot: %w", err)
	}
	table, smData := unwrapSnapshot(fullData)
	n.clientTable.loadFrom(table)

	// Deep-copy the client table for the apply goroutine. After this point the
	// event loop may mutate n.clientTable (via handleApplyResult) while the
	// apply goroutine reads si.clientTable — sharing the same map causes a
	// data race.
	tableForApply := make(map[NodeID]clientEntry, len(table))
	for id, e := range table {
		tableForApply[id] = e
	}

	// Check if our log already contains the snapshot boundary consistently.
	consistent := false
	if n.log.lastLogIndex() >= req.LastIncludedIndex {
		t, err := n.log.termAt(n.stopCtx, req.LastIncludedIndex)
		if err == nil && t == req.LastIncludedTerm {
			consistent = true
		}
	}

	if consistent {
		// Keep log entries after the snapshot boundary.
		if err := n.log.truncatePrefix(n.stopCtx, req.LastIncludedIndex+1); err != nil {
			n.logger.Warn("installSnapshot: truncatePrefix", "err", err)
		}
	} else {
		// Discard the entire log.
		if n.log.first > 0 {
			if err := n.log.storage.TruncateSuffix(n.stopCtx, n.log.first); err != nil {
				n.logger.Warn("installSnapshot: discard log", "err", err)
			}
			n.log.first = 0
			n.log.last = 0
		}
	}
	n.log.snapMeta = meta
	n.log.lastTerm = meta.LastIncludedTerm

	if n.commitIndex < req.LastIncludedIndex {
		n.commitIndex = req.LastIncludedIndex
	}
	// Advance event-loop lastApplied to prevent the apply goroutine from
	// trying to apply entries that have been superseded by the snapshot.
	// NOTE: atomicLastApplied is NOT updated here — it is updated by the
	// apply goroutine after it actually restores the state machine, so that
	// LastApplied() only advances once the SM reflects the snapshot.
	n.lastApplied = req.LastIncludedIndex

	// Ask the apply goroutine to restore the state machine from the snapshot.
	// Send the unwrapped SM data (not the raw fullData which includes the
	// client table header). Also forward the client table so applyLoop can
	// reset its local dedup state consistently with the new SM state.
	// Use a non-blocking replace so the event loop never blocks.
	si := snapshotInstall{meta: meta, data: smData, clientTable: tableForApply}
	select {
	case n.restoreSnapshotCh <- si:
	default:
		select {
		case <-n.restoreSnapshotCh:
		default:
		}
		n.restoreSnapshotCh <- si
	}

	n.logger.Info("installed snapshot", "index", meta.LastIncludedIndex, "term", meta.LastIncludedTerm)
	return resp, nil
}

// snapshotTrigger is sent from the event loop to applyLoop to request a
// snapshot. applyLoop calls StateMachine.Snapshot() after completing the
// current Apply, ensuring Snapshot and Apply never execute concurrently on
// the same state machine.
type snapshotTrigger struct {
	meta SnapshotMeta
	// clientTable is a deep copy of n.clientTable at trigger time, consistent
	// with the SM state at meta.LastIncludedIndex.
	clientTable map[NodeID]clientEntry
}

// snapshotResult is delivered from applyLoop to the event loop once
// StateMachine.Snapshot() has completed.
type snapshotResult struct {
	meta SnapshotMeta
	data []byte
	err  error
	// clientTable is the table captured at trigger time, forwarded from the
	// snapshotTrigger so handleSnapshotResult can wrap the snapshot correctly.
	clientTable map[NodeID]clientEntry
}

// snapshotInstall is sent from the event loop to the apply goroutine when an
// InstallSnapshot RPC has been processed and the state machine must be
// restored.
type snapshotInstall struct {
	meta SnapshotMeta
	data []byte
	// clientTable is the dedup table consistent with the snapshot at meta.LastIncludedIndex.
	// applyLoop uses it to seed its local dedup state so that exactly-once
	// semantics are preserved across snapshot boundaries.
	clientTable map[NodeID]clientEntry
}

// partialSnapshot accumulates chunks of an in-progress multi-chunk snapshot
// install on a follower. It is created when the first chunk (Offset==0) of a
// snapshot with a higher LastIncludedIndex arrives and is nil'd once the final
// chunk (Done==true) has been processed or when becomeFollower clears it.
type partialSnapshot struct {
	meta           SnapshotMeta
	data           []byte
	expectedOffset int64
}

// installSnapshotResult carries the outcome of a leader-initiated
// InstallSnapshot RPC back to the event loop.
type installSnapshotResult struct {
	peer    NodeID
	term    Term
	meta    SnapshotMeta
	dropped bool // true if the RPC failed; only peer and meta are valid
}

// maybeSnapshot checks whether the accumulated log since the last snapshot
// exceeds SnapshotThreshold and, if so, signals applyLoop to take a snapshot.
//
// The snapshot is taken by applyLoop (not by a separate goroutine spawned here)
// so that StateMachine.Snapshot() and StateMachine.Apply() are never called
// concurrently. This removes the requirement for state machines to be
// internally thread-safe for Snapshot/Apply overlap.
func (n *Node) maybeSnapshot() {
	if n.snapshotting || n.cfg.SnapshotThreshold == 0 {
		return
	}
	if n.lastApplied < n.log.snapMeta.LastIncludedIndex+Index(n.cfg.SnapshotThreshold) {
		return
	}

	// Throttling: if a shared SnapshotSemaphore is configured, acquire a
	// permit before starting. If no permits are available, we defer the
	// snapshot until the next applyResult. This prevents correlated
	// "snapshot storms" in multi-raft deployments.
	if n.cfg.SnapshotSemaphore != nil {
		select {
		case n.cfg.SnapshotSemaphore <- struct{}{}:
			// Permit acquired.
		default:
			// No permits available; try again later.
			return
		}
	}

	snapAt := n.lastApplied
	snapTerm, err := n.log.termAt(n.stopCtx, snapAt)
	if err != nil {
		n.logger.Error("maybeSnapshot: termAt", "index", snapAt, "err", err)
		if n.cfg.SnapshotSemaphore != nil {
			<-n.cfg.SnapshotSemaphore
		}
		return
	}
	n.snapshotting = true
	meta := SnapshotMeta{LastIncludedIndex: snapAt, LastIncludedTerm: snapTerm}

	// Capture the client table at trigger time so that the snapshot wraps
	// table state consistent with the SM state at snapAt. applyLoop will
	// forward this table through snapshotResult to handleSnapshotResult.
	tableSnapshot := n.clientTable.toMap()

	// Signal applyLoop to take the snapshot. The channel is size-1 and
	// snapshotting prevents re-entry, so this send never blocks.
	n.snapshotTriggerCh <- snapshotTrigger{meta: meta, clientTable: tableSnapshot}
}

// handleSnapshotResult is called when the snapshot goroutine completes. It
// persists the snapshot and truncates the log prefix.
func (n *Node) handleSnapshotResult(sr snapshotResult) {
	defer func() {
		n.snapshotting = false
		if n.cfg.SnapshotSemaphore != nil {
			<-n.cfg.SnapshotSemaphore
		}
	}()

	if sr.err != nil {
		n.logger.Error("snapshot goroutine failed", "err", sr.err)
		return
	}

	// Wrap the SM data with the client table captured at trigger time.
	// Using sr.clientTable (not n.clientTable) ensures the table is consistent
	// with the SM state at sr.meta.LastIncludedIndex.
	wrapped := wrapSnapshot(sr.clientTable, sr.data)
	if err := n.cfg.Storage.SaveSnapshot(n.stopCtx, sr.meta, wrapped); err != nil {
		n.logger.Error("snapshot: SaveSnapshot", "err", err)
		return
	}
	if err := n.log.truncatePrefix(n.stopCtx, sr.meta.LastIncludedIndex+1); err != nil {
		n.logger.Error("snapshot: truncatePrefix", "err", err)
		return
	}
	n.log.snapMeta = sr.meta
	n.logger.Info("snapshot saved", "index", sr.meta.LastIncludedIndex, "term", sr.meta.LastIncludedTerm)
	if n.cfg.Metrics != nil {
		n.cfg.Metrics.SnapshotTaken(n.cfg.ID, sr.meta.LastIncludedIndex, len(sr.data))
	}
}

// sendSnapshotToPeer loads the latest snapshot and sends it to peer via
// InstallSnapshot RPC. The RPC is made in a background goroutine; the result
// is fed back to the event loop via rpcCh.
// Only one concurrent snapshot transfer per peer is allowed; duplicate calls
// while a transfer is in-flight are silently dropped.
func (n *Node) sendSnapshotToPeer(peer NodeID) {
	if n.snapshotInflight[peer] {
		return // already transferring to this peer
	}
	meta, data, err := n.cfg.Storage.LoadSnapshot(n.stopCtx)
	if err != nil {
		n.logger.Error("sendSnapshotToPeer: LoadSnapshot", "peer", peer, "err", err)
		return
	}
	n.snapshotInflight[peer] = true

	chunkSize := n.snapshotChunkSize()
	term := n.currentTerm
	leaderID := n.cfg.ID

	go func(p NodeID, m SnapshotMeta) {
		// Give each chunk a per-chunk timeout (4× normal RPC timeout) since
		// individual chunks may still be large.
		chunkTimeout := n.rpcTimeout() * 4

		drop := func() {
			select {
			case n.rpcCh <- rpcEnvelope{
				req: &installSnapshotResult{peer: p, term: 0, meta: m, dropped: true},
			}:
			case <-n.stopCtx.Done():
			}
		}

		if chunkSize <= 0 || len(data) <= chunkSize {
			// Single-RPC path: send everything at once.
			req := &InstallSnapshotRequest{
				GroupID:           n.cfg.GroupID,
				Term:              term,
				LeaderID:          leaderID,
				LastIncludedIndex: m.LastIncludedIndex,
				LastIncludedTerm:  m.LastIncludedTerm,
				Offset:            0,
				Data:              data,
				Done:              true,
			}
			ctx, cancel := context.WithTimeout(n.stopCtx, chunkTimeout)
			defer cancel()
			finish := n.traceRPC(p, "InstallSnapshot")
			resp, rerr := n.cfg.Transport.InstallSnapshot(ctx, p, req)
			finish(rerr)
			if rerr != nil || resp == nil {
				drop()
				return
			}
			select {
			case n.rpcCh <- rpcEnvelope{
				req: &installSnapshotResult{peer: p, term: resp.Term, meta: m},
			}:
			case <-n.stopCtx.Done():
			}
			return
		}

		// Multi-chunk path: send sequential RPCs, each carrying at most chunkSize bytes.
		var offset int64
		for offset < int64(len(data)) {
			end := offset + int64(chunkSize)
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			chunk := data[offset:end]
			done := end == int64(len(data))

			req := &InstallSnapshotRequest{
				GroupID:           n.cfg.GroupID,
				Term:              term,
				LeaderID:          leaderID,
				LastIncludedIndex: m.LastIncludedIndex,
				LastIncludedTerm:  m.LastIncludedTerm,
				Offset:            offset,
				Data:              chunk,
				Done:              done,
			}
			ctx, cancel := context.WithTimeout(n.stopCtx, chunkTimeout)
			finish := n.traceRPC(p, "InstallSnapshot")
			resp, rerr := n.cfg.Transport.InstallSnapshot(ctx, p, req)
			finish(rerr)
			cancel()
			if rerr != nil || resp == nil {
				drop()
				return
			}
			// If the follower has stepped up its term, report back and abort.
			if resp.Term > term {
				select {
				case n.rpcCh <- rpcEnvelope{
					req: &installSnapshotResult{peer: p, term: resp.Term, meta: m},
				}:
				case <-n.stopCtx.Done():
				}
				return
			}
			if done {
				select {
				case n.rpcCh <- rpcEnvelope{
					req: &installSnapshotResult{peer: p, term: resp.Term, meta: m},
				}:
				case <-n.stopCtx.Done():
				}
				return
			}
			offset = end
		}
	}(peer, meta)
}

// handleInstallSnapshotResult is called when an InstallSnapshot RPC we sent
// to a follower has completed. It advances the follower's tracking indices.
func (n *Node) handleInstallSnapshotResult(r *installSnapshotResult) {
	// Clear inflight flag regardless of outcome so the next heartbeat can retry.
	if n.snapshotInflight != nil {
		delete(n.snapshotInflight, r.peer)
	}
	if r.dropped {
		return // RPC failed; the next heartbeat will trigger a retry.
	}
	if r.term > n.currentTerm {
		n.becomeFollower(r.term, "")
		return
	}
	if n.state != Leader {
		return
	}
	if r.meta.LastIncludedIndex >= n.nextIndex[r.peer] {
		n.nextIndex[r.peer] = r.meta.LastIncludedIndex + 1
		n.matchIndex[r.peer] = r.meta.LastIncludedIndex
	}
	n.maybeAdvanceCommit()
	// If there are new entries after the snapshot, replicate them.
	if n.nextIndex[r.peer] <= n.log.lastLogIndex() {
		n.replicateToPeer(r.peer)
	}
}
