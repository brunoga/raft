package raft

import "slices"

// ---- Membership changes -----------------------------------------------------

// storePeers snapshots the current cfg.Peers slice into atomicPeers so that
// callers outside the event loop (e.g. ReconfigureCluster) can read the peer
// list without a data race. Must be called from the event-loop goroutine after
// every mutation to n.cfg.Peers.
func (n *Node) storePeers() {
	snap := make([]NodeID, len(n.cfg.Peers))
	copy(snap, n.cfg.Peers)
	n.atomicPeers.Store(snap)
}

// applyConfigChange updates the in-memory peer list when a committed
// config-change log entry is applied. It is called for every node (leader and
// follower) once the entry is committed.
func (n *Node) applyConfigChange(configCmd []byte) {
	op, id, ok := decodeConfigEntry(configCmd)
	if !ok {
		return
	}
	switch op {
	case configOpAdd:
		if id == n.cfg.ID || slices.Contains(n.cfg.Peers, id) {
			return // self or already present
		}
		n.cfg.Peers = append(n.cfg.Peers, id)
		n.storePeers()
		if n.state == Leader {
			n.nextIndex[id] = n.log.lastLogIndex() + 1
			n.matchIndex[id] = 0
			// inflight and snapshotInflight are zero/false by map default.
			// Start a heartbeat pump so the newly added peer receives ongoing
			// heartbeats immediately, without waiting for the next proposal.
			n.startHBPumpFor(id)
		}
		n.logger.Info("config change: added peer", "id", id)

	case configOpRemove:
		for i, p := range n.cfg.Peers {
			if p == id {
				n.cfg.Peers = append(n.cfg.Peers[:i], n.cfg.Peers[i+1:]...)
				break
			}
		}
		n.storePeers()
		if n.state == Leader {
			n.stopHBPumpFor(id)
			delete(n.nextIndex, id)
			delete(n.matchIndex, id)
			delete(n.inflight, id)
			delete(n.snapshotInflight, id)
			// We may now have quorum with one fewer peer; re-check.
			n.maybeAdvanceCommit()
		}
		n.logger.Info("config change: removed peer", "id", id)
		// If we removed ourselves, step down.
		if id == n.cfg.ID {
			n.becomeFollower(n.currentTerm, "")
		}

	case configOpJoint:
		old, new_, ok2 := decodeJointConfigEntry(configCmd)
		if !ok2 {
			return
		}
		n.jointOld = old
		// new_ may include self's ID when self is retained in the new cluster.
		// Filter self out for peer tracking; remember the inclusion flag so
		// appendFinaliseEntry knows whether to include self in the finalise entry.
		n.jointIncludeSelf = false
		newPeers := make([]NodeID, 0, len(new_))
		for _, m := range new_ {
			if m == n.cfg.ID {
				n.jointIncludeSelf = true
			} else {
				newPeers = append(newPeers, m)
			}
		}
		n.jointNew = newPeers

		// cfg.Peers becomes the union of old and new peers, excluding self.
		union := peerUnion(old, newPeers, n.cfg.ID)
		// Init tracking state on the leader for any brand-new peers.
		if n.state == Leader {
			nextIdx := n.log.lastLogIndex() + 1
			for _, p := range union {
				if _, exists := n.nextIndex[p]; !exists {
					n.nextIndex[p] = nextIdx
					n.matchIndex[p] = 0
				}
			}
		}
		n.cfg.Peers = union
		n.storePeers()
		n.logger.Info("config change: entered joint consensus",
			"old", old, "new", new_)

		// The leader auto-appends the finalise entry to drive the second phase.
		if n.state == Leader {
			n.appendFinaliseEntry(n.jointNew, n.jointIncludeSelf)
		}

	case configOpFinalise:
		members, ok2 := decodeFinaliseConfigEntry(configCmd)
		if !ok2 {
			return
		}
		oldPeers := n.cfg.Peers

		// cfg.Peers becomes the finalised new membership, excluding self.
		newPeers := make([]NodeID, 0, len(members))
		selfInNew := false
		for _, m := range members {
			if m == n.cfg.ID {
				selfInNew = true
			} else {
				newPeers = append(newPeers, m)
			}
		}
		// Self is always implicitly in the new config (ReconfigureCluster callers
		// pass newMembers that exclude self, so self is always retained).
		// If somehow self is not present, we still clean up and step down.
		_ = selfInNew

		// Clean up leader tracking for peers that are no longer in the cluster.
		if n.state == Leader {
			for _, p := range oldPeers {
				if !slices.Contains(newPeers, p) {
					delete(n.nextIndex, p)
					delete(n.matchIndex, p)
					delete(n.inflight, p)
					delete(n.snapshotInflight, p)
				}
			}
		}

		n.cfg.Peers = newPeers
		n.storePeers()
		n.jointOld = nil
		n.jointNew = nil
		n.jointIncludeSelf = false
		n.logger.Info("config change: finalised new membership", "peers", newPeers)

		if n.state == Leader {
			// Quorum size has changed; re-check whether anything can now commit.
			n.maybeAdvanceCommit()
		}
		// Step down if self is not in the new config.
		if !selfInNew {
			n.logger.Info("config change: self removed, stepping down")
			n.becomeFollower(n.currentTerm, "")
		}
	}
}

// appendFinaliseEntry appends a configOpFinalise entry that commits the new
// cluster membership. Called by the leader after applying a joint config entry
// to drive the second phase of joint consensus.
//
// newPeers is the list of peers (excluding self). includeSelf controls whether
// self's ID is included in the encoded membership: true means self stays in
// the cluster; false means self is removed and will step down when the
// finalise entry is applied.
func (n *Node) appendFinaliseEntry(newPeers []NodeID, includeSelf bool) {
	// Build the complete new membership.
	var allNew []NodeID
	if includeSelf {
		allNew = make([]NodeID, 0, len(newPeers)+1)
		allNew = append(allNew, n.cfg.ID)
		allNew = append(allNew, newPeers...)
	} else {
		allNew = newPeers
	}

	idx := n.log.lastLogIndex() + 1
	entry := LogEntry{
		Index:   idx,
		Term:    n.currentTerm,
		Command: encodeFinaliseConfigEntry(allNew),
	}
	if err := n.log.appendOne(n.stopCtx, entry); err != nil {
		n.logger.Error("appendFinaliseEntry: append", "err", err)
		return
	}
	// NOTE (false positive — intentional): pendingConfigIndex is set only after
	// a successful append. If the append fails, we must not block future config
	// changes with a stale pendingConfigIndex that refers to an entry that was
	// never written. The caller (applyConfigChange) will retry on the next
	// becomeLeader invocation if the node wins a subsequent election.
	n.pendingConfigIndex = idx
	n.replicateToFollowers()
	n.maybeAdvanceCommit()
}

// peerUnion returns the union of two peer lists, excluding the local node ID.
func peerUnion(a, b []NodeID, self NodeID) []NodeID {
	seen := make(map[NodeID]bool, len(a)+len(b))
	result := make([]NodeID, 0, len(a)+len(b))
	for _, p := range a {
		if p != self && !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	for _, p := range b {
		if p != self && !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	return result
}
