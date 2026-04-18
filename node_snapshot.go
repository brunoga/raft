package raft

import (
	"context"
	"fmt"
	"io"
)

// ---- Snapshotting -----------------------------------------------------------

// partialSnapshot holds state for an in-progress multi-chunk snapshot install.
// Chunks are streamed directly to storage via a goroutine rather than buffered
// in memory, preventing OOM when the snapshot is large.
type partialSnapshot struct {
	meta        SnapshotMeta
	installCh   chan []byte        // event loop sends raw chunks; closed after final chunk
	cancelFn    context.CancelFunc // cancels the runSnapshotInstall goroutine
	expectedOff int64              // expected byte offset of the next incoming chunk
}

// snapInstallResult is posted by runSnapshotInstall to the event loop when the
// streaming snapshot install from a leader completes (successfully or not).
type snapInstallResult struct {
	meta  SnapshotMeta
	table map[NodeID]clientEntry
	smR   io.ReadCloser // positioned past framing header, at SM data; nil on error
	err   error
}

// chunkReader implements io.Reader by draining a channel of byte slices.
// It is used by runSnapshotInstall to stream snapshot chunks directly to
// storage without accumulating the full snapshot in memory.
type chunkReader struct {
	ctx context.Context
	ch  <-chan []byte
	buf []byte // leftover bytes from the previous partial Read
}

func (r *chunkReader) Read(p []byte) (int, error) {
	// Serve from the tail of the previously-received chunk first.
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	select {
	case chunk, ok := <-r.ch:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, chunk)
		r.buf = chunk[n:]
		return n, nil
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
}

// drainChunks reads and discards all available items from ch until it is
// closed or empty. Used by runSnapshotInstall to release any buffered chunks
// after a storage error or context cancellation.
func drainChunks(ch <-chan []byte) {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		default:
			return
		}
	}
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
// Each call enqueues one chunk of snapshot data to a background goroutine that
// streams it directly to storage, preventing full-snapshot buffering in memory.
// The goroutine posts a snapInstallResult to rpcCh when the install completes.
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

	// Cancel any in-progress install that is for a different snapshot OR that
	// the sender is restarting from chunk 0 (retry after an error).
	if n.pendingSnap != nil {
		sameSnap := n.pendingSnap.meta.LastIncludedIndex == req.LastIncludedIndex
		if !sameSnap || req.Offset == 0 {
			n.pendingSnap.cancelFn()
			n.pendingSnap = nil
		}
	}

	// Start a new streaming install goroutine if needed.
	if n.pendingSnap == nil {
		if req.Offset != 0 {
			// Cannot resume an install without its beginning; wait for chunk 0.
			return resp, nil
		}
		meta := SnapshotMeta{
			LastIncludedIndex: req.LastIncludedIndex,
			LastIncludedTerm:  req.LastIncludedTerm,
		}
		installCtx, cancelFn := context.WithCancel(n.stopCtx)
		installCh := make(chan []byte, 8) // small buffer; sender rate-limited by RPC round-trips
		n.pendingSnap = &partialSnapshot{
			meta:      meta,
			installCh: installCh,
			cancelFn:  cancelFn,
		}
		n.snapshotInstallWg.Add(1)
		go func() {
			defer n.snapshotInstallWg.Done()
			n.runSnapshotInstall(installCtx, meta, installCh)
		}()
	}

	// Verify the expected byte offset to detect out-of-order delivery.
	if req.Offset != n.pendingSnap.expectedOff {
		return resp, nil
	}

	// Copy chunk data: the proto bytes backing req.Data may be reused by the
	// gRPC framing layer after this handler returns.
	chunk := make([]byte, len(req.Data))
	copy(chunk, req.Data)

	// Enqueue chunk for the install goroutine. The channel has a small buffer;
	// because the sender waits for our response before sending the next chunk,
	// the goroutine should consume each chunk well before the next one arrives.
	select {
	case n.pendingSnap.installCh <- chunk:
	default:
		// Channel full: disk I/O is lagging behind the RPC rate.  Cancel the
		// install goroutine so it exits cleanly (a blocked goroutine waiting
		// for chunks that never arrive would otherwise leak until node stop).
		// The leader will retry from Offset=0 when it detects stalled progress.
		n.logger.Warn("snapshot install: chunk channel full; cancelling install",
			"peer", req.LeaderID, "offset", req.Offset)
		n.pendingSnap.cancelFn()
		n.pendingSnap = nil
		return resp, nil
	}
	n.pendingSnap.expectedOff += int64(len(req.Data))

	if req.Done {
		// Closing the channel signals io.EOF to the chunkReader inside SaveSnapshot.
		close(n.pendingSnap.installCh)
		n.pendingSnap = nil // goroutine owns installCh from here; posts result via rpcCh
	}

	return resp, nil
}

// runSnapshotInstall is the background goroutine for a streaming snapshot
// install. It reads chunks from chunkCh (which is closed after the final
// chunk), writes them to storage via SaveSnapshot, then re-reads to extract
// the client table and get a reader positioned at the state-machine data.
// The result is posted to the event loop via rpcCh.
func (n *Node) runSnapshotInstall(ctx context.Context, meta SnapshotMeta, chunkCh <-chan []byte) {
	cr := &chunkReader{ctx: ctx, ch: chunkCh}
	saveErr := n.cfg.Storage.SaveSnapshot(ctx, meta, cr)

	// Drain remaining buffered chunks so no event-loop sends are left blocked.
	drainChunks(chunkCh)

	sendResult := func(r *snapInstallResult) {
		// Prefer ctx.Done() (installCtx) over n.stopCtx so that a goroutine
		// whose install was superseded by a newer one discards its result
		// instead of posting a stale result to the event loop.
		// installCtx is derived from stopCtx, so this also handles node stop.
		select {
		case <-ctx.Done():
			if r.smR != nil {
				_ = r.smR.Close()
			}
			return
		default:
		}
		select {
		case n.rpcCh <- rpcEnvelope{req: r}:
		case <-ctx.Done():
			if r.smR != nil {
				_ = r.smR.Close()
			}
		}
	}

	if saveErr != nil {
		if ctx.Err() != nil {
			// Install was deliberately cancelled (node stopped or new snapshot
			// started); silently discard without posting an error result.
			return
		}
		n.logger.Error("snapshot install: SaveSnapshot failed", "err", saveErr)
		sendResult(&snapInstallResult{meta: meta, err: saveErr})
		return
	}

	// Re-read the freshly saved snapshot to extract the client dedup table and
	// to obtain a reader positioned at the state-machine data for the apply
	// goroutine. This avoids holding the full snapshot in memory after saving.
	loadMeta, r, err := n.cfg.Storage.LoadSnapshot(ctx)
	if err != nil {
		sendResult(&snapInstallResult{meta: meta, err: fmt.Errorf("snapshot install: LoadSnapshot: %w", err)})
		return
	}
	if loadMeta.LastIncludedIndex != meta.LastIncludedIndex {
		_ = r.Close()
		sendResult(&snapInstallResult{meta: meta, err: fmt.Errorf(
			"snapshot install: loaded snapshot index %d, want %d",
			loadMeta.LastIncludedIndex, meta.LastIncludedIndex)})
		return
	}

	table, _, parseErr := readWrappedSnapshot(r)
	if parseErr != nil {
		_ = r.Close()
		sendResult(&snapInstallResult{meta: meta, err: fmt.Errorf("snapshot install: unwrap: %w", parseErr)})
		return
	}
	// r is now positioned past the framing header, at the SM data. The event
	// loop forwards it to the apply goroutine for StateMachine.Restore; the
	// apply goroutine closes r when done.
	sendResult(&snapInstallResult{meta: meta, table: table, smR: r})
}

// handleSnapInstallResult is called when runSnapshotInstall reports completion.
// It updates log state, the client dedup table, and signals the apply goroutine
// to restore the state machine from the newly installed snapshot.
func (n *Node) handleSnapInstallResult(r *snapInstallResult) {
	if r.err != nil {
		n.logger.Error("snapshot install failed", "err", r.err)
		// Clear the stale pendingSnap reference so that any chunks arriving
		// before the leader retries from Offset=0 are rejected immediately
		// rather than being buffered into a channel whose consumer has exited.
		if n.pendingSnap != nil && n.pendingSnap.meta.LastIncludedIndex == r.meta.LastIncludedIndex {
			n.pendingSnap = nil
		}
		return
	}

	// Skip stale results: a newer snapshot may have already been applied.
	if r.meta.LastIncludedIndex <= n.lastApplied {
		_ = r.smR.Close()
		return
	}

	n.clientTable.loadFrom(r.table)

	if err := n.log.truncatePrefix(n.stopCtx, r.meta.LastIncludedIndex+1); err != nil {
		n.logger.Error("snapshot install: truncatePrefix", "err", err)
		_ = r.smR.Close()
		return
	}
	n.log.snapMeta = r.meta

	// Signal applyLoop to restore the state machine.
	install := snapshotInstall{
		meta:        r.meta,
		r:           r.smR,
		clientTable: r.table,
	}
	select {
	case n.restoreSnapshotCh <- install:
	default:
		// Existing pending restore — overwrite with the newer result.
		// Close the displaced reader to prevent an fd leak.
		select {
		case old := <-n.restoreSnapshotCh:
			if old.r != nil {
				_ = old.r.Close()
			}
		default:
		}
		n.restoreSnapshotCh <- install
	}

	// Commit index must reflect at least the snapshot index.
	if n.commitIndex < r.meta.LastIncludedIndex {
		n.setCommitIndex(r.meta.LastIncludedIndex)
	}
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
	// Per-chunk RPC timeout: snapshot chunks can be large, so use 4× the
	// base RPC timeout (consistent with the RPCTimeout config documentation).
	chunkTimeout := 4 * n.rpcTimeout()

	go func(p NodeID, m SnapshotMeta, r io.ReadCloser) {
		defer func() { _ = r.Close() }()

		drop := func() {
			select {
			case n.rpcCh <- rpcEnvelope{
				req: &installSnapshotResult{peer: p, term: 0, meta: m, dropped: true},
			}:
			case <-n.stopCtx.Done():
			}
		}

		// Allocate the read buffer once and reuse it across chunks; gRPC
		// marshals req.Data synchronously before InstallSnapshot returns so
		// reuse is safe.
		buf := make([]byte, chunkSize)
		offset := int64(0)
		for {
			nRead, err := io.ReadFull(r, buf)
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				n.logger.Error("sendSnapshotToPeer: read", "peer", p, "err", err)
				drop()
				return
			}
			done := err == io.EOF || err == io.ErrUnexpectedEOF

			req := &InstallSnapshotRequest{
				GroupID:           n.cfg.GroupID,
				Term:              term,
				LeaderID:          leaderID,
				LastIncludedIndex: m.LastIncludedIndex,
				LastIncludedTerm:  m.LastIncludedTerm,
				Offset:            offset,
				Data:              buf[:nRead],
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
