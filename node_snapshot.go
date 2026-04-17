package raft

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"
)

// ---- Snapshotting -----------------------------------------------------------

// partialSnapshot holds chunks of an in-progress multi-chunk snapshot install.
type partialSnapshot struct {
	meta SnapshotMeta
	data []byte // buffers chunks until Done=true
}

// snapshotTrigger carries snapshot-trigger metadata from the event loop
// to applyLoop. applyLoop calls StateMachine.Snapshot() after completing
// the current Apply, guaranteeing that Snapshot and Apply never overlap.
// the same state machine.
type snapshotTrigger struct {
	meta SnapshotMeta
	// clientTable is a deep copy of n.clientTable at trigger time, consistent
	// with the SM state at meta.LastIncludedIndex.
	clientTable map[NodeID]clientEntry
}

// snapshotResult is delivered from applyLoop to the event loop once the
// snapshot has been taken and saved to storage.
type snapshotResult struct {
	meta SnapshotMeta
	err  error
}

// snapshotInstall is sent from the event loop to the apply goroutine when an
// InstallSnapshot RPC has been processed and the state machine must be
// restored from storage.
type snapshotInstall struct {
	meta        SnapshotMeta
	r           io.ReadCloser
	clientTable map[NodeID]clientEntry
}

// installSnapshotResult is delivered from sendSnapshotToPeer's background
// goroutine to the event loop.
type installSnapshotResult struct {
	peer    NodeID
	term    Term
	meta    SnapshotMeta
	dropped bool
}

// handleInstallSnapshot implements the receiver side of the InstallSnapshot RPC.
func (n *Node) handleInstallSnapshot(req *InstallSnapshotRequest) (*InstallSnapshotResponse, error) {
	resp := &InstallSnapshotResponse{Term: n.currentTerm}

	if req.Term < n.currentTerm {
		return resp, nil
	}
	if req.Term > n.currentTerm {
		n.becomeFollower(req.Term, req.LeaderID)
		resp.Term = n.currentTerm
	}

	// Raft §7: "The leader identifies its status by including its term and ID
	// in the InstallSnapshot request."
	n.leaderID = req.LeaderID
	n.resetElectionTimeout()

	// If the snapshot is old, discard it.
	if req.LastIncludedIndex <= n.lastApplied {
		return resp, nil
	}

	// Mult-chunk install.
	if n.pendingSnap == nil || n.pendingSnap.meta.LastIncludedIndex != req.LastIncludedIndex {
		// New snapshot install starting.
		n.pendingSnap = &partialSnapshot{
			meta: SnapshotMeta{
				LastIncludedIndex: req.LastIncludedIndex,
				LastIncludedTerm:  req.LastIncludedTerm,
			},
		}
	}

	// Verify offset.
	if int64(len(n.pendingSnap.data)) != req.Offset {
		// Out of order chunk; return current term and wait for re-send.
		return resp, nil
	}

	// Append chunk data.
	n.pendingSnap.data = append(n.pendingSnap.data, req.Data...)

	if !req.Done {
		return resp, nil
	}

	// Snapshot complete.
	fullData := n.pendingSnap.data
	meta := n.pendingSnap.meta
	n.pendingSnap = nil

	// Save to storage.
	if err := n.cfg.Storage.SaveSnapshot(n.stopCtx, meta, bytes.NewReader(fullData)); err != nil {
		return resp, fmt.Errorf("installSnapshot: SaveSnapshot: %w", err)
	}
	table, smDataReader, err := readWrappedSnapshot(bytes.NewReader(fullData))
	if err != nil {
		return resp, fmt.Errorf("installSnapshot: unwrap: %w", err)
	}
	n.clientTable.loadFrom(table)

	// Update in-memory log cache.
	if err := n.log.truncatePrefix(n.stopCtx, meta.LastIncludedIndex+1); err != nil {
		return resp, fmt.Errorf("installSnapshot: truncatePrefix: %w", err)
	}
	n.log.snapMeta = meta

	// Signal applyLoop to restore state machine.
	// Non-blocking send: if applyLoop is busy, it will catch the newest
	// snapshot on its next priority-select.
	select {
	case n.restoreSnapshotCh <- snapshotInstall{
		meta:        meta,
		r:           io.NopCloser(smDataReader),
		clientTable: table,
	}:
	default:
		// Existing pending restore. Overwrite it.
		select {
		case <-n.restoreSnapshotCh:
		default:
		}
		n.restoreSnapshotCh <- snapshotInstall{
			meta:        meta,
			r:           io.NopCloser(smDataReader),
			clientTable: table,
		}
	}

	// Commit index must be at least the snapshot index.
	if n.commitIndex < meta.LastIncludedIndex {
		n.setCommitIndex(meta.LastIncludedIndex)
	}
	// applied index is handled by applyLoop.

	return resp, nil
}

// maybeSnapshot checks if the log has grown past SnapshotThreshold and, if so,
// triggers a background snapshot in applyLoop.
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

	if err := n.log.truncatePrefix(n.stopCtx, sr.meta.LastIncludedIndex+1); err != nil {
		n.logger.Error("snapshot: truncatePrefix", "err", err)
		return
	}
	n.log.snapMeta = sr.meta
	n.logger.Info("snapshot saved", "index", sr.meta.LastIncludedIndex, "term", sr.meta.LastIncludedTerm)
	if n.cfg.Metrics != nil {
		n.cfg.Metrics.SnapshotTaken(n.cfg.ID, sr.meta.LastIncludedIndex, 0) // size unknown without more plumbing
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
	meta, r, err := n.cfg.Storage.LoadSnapshot(n.stopCtx)
	if err != nil {
		n.logger.Error("sendSnapshotToPeer: LoadSnapshot", "peer", peer, "err", err)
		return
	}
	n.snapshotInflight[peer] = true

	chunkSize := n.snapshotChunkSize()
	term := n.currentTerm
	leaderID := n.cfg.ID

	go func(p NodeID, m SnapshotMeta, r io.ReadCloser) {
		defer r.Close()

		// Give each chunk a generous timeout since individual chunks may still
		// be large and the network or disk may be slow.
		chunkTimeout := 2 * time.Second

		drop := func() {
			select {
			case n.rpcCh <- rpcEnvelope{
				req: &installSnapshotResult{peer: p, term: 0, meta: m, dropped: true},
			}:
			case <-n.stopCtx.Done():
			}
		}

		offset := int64(0)
		for {
			buf := make([]byte, chunkSize)
			nRead, err := io.ReadFull(r, buf)
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				n.logger.Error("sendSnapshotToPeer: read", "peer", p, "err", err)
				drop()
				return
			}
			done := err == io.EOF || err == io.ErrUnexpectedEOF
			if done {
				buf = buf[:nRead]
			}

			req := &InstallSnapshotRequest{
				GroupID:           n.cfg.GroupID,
				Term:              term,
				LeaderID:          leaderID,
				LastIncludedIndex: m.LastIncludedIndex,
				LastIncludedTerm:  m.LastIncludedTerm,
				Offset:            offset,
				Data:              buf,
				Done:              done,
			}

			ctx, cancel := context.WithTimeout(n.stopCtx, chunkTimeout)
			resp, err := n.cfg.Transport.InstallSnapshot(ctx, p, req)
			cancel()

			if err != nil || resp == nil {
				n.logger.Error("sendSnapshotToPeer: RPC", "peer", p, "err", err)
				drop()
				return
			}

			if resp.Term > term || done {
				select {
				case n.rpcCh <- rpcEnvelope{
					req: &installSnapshotResult{peer: p, term: resp.Term, meta: m},
				}:
				case <-n.stopCtx.Done():
				}
				return
			}
			offset += int64(nRead)
		}
	}(peer, meta, r)
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
	if r.meta.LastIncludedIndex >= n.matchIndex[r.peer] {
		n.nextIndex[r.peer] = r.meta.LastIncludedIndex + 1
		n.matchIndex[r.peer] = r.meta.LastIncludedIndex
	}
	n.maybeAdvanceCommit()
	// If there are new entries after the snapshot, replicate them.
	if n.nextIndex[r.peer] <= n.log.lastLogIndex() {
		n.replicateToPeer(r.peer)
	}
}
