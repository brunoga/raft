package raft

import "context"

// ---- Membership changes -----------------------------------------------------
//
// A node's membership is not configuration: it is state the cluster agreed on
// through the log, and every node must be able to recover it from its own
// durable storage. The rule, following the Raft dissertation section 4.1, is
// that a node uses the latest configuration in its log, whether or not that
// entry has committed. That gives a single invariant:
//
//	membership = baseMembership (from the snapshot) + every config entry in the log
//
// which is established in exactly three places: at startup, when config entries
// are appended, and when a conflicting suffix is truncated away. Config.Peers
// is only a bootstrap value, used when there is no snapshot and no config entry
// in the log.
//
// Committing a config entry still matters, but only for lifecycle decisions —
// starting the second phase of a joint reconfiguration, stepping down after
// being removed, and releasing the one-change-at-a-time gate. Those stay on the
// apply path, in applyConfigChange.

// storePeers snapshots the current cfg.Peers slice into atomicPeers so that
// callers outside the event loop (e.g. ReconfigureCluster) can read the peer
// list without a data race. Must be called from the event-loop goroutine after
// every mutation to n.cfg.Peers.
func (n *Node) storePeers() {
	snap := make([]PeerConfig, len(n.cfg.Peers))
	copy(snap, n.cfg.Peers)
	n.atomicPeers.Store(snap)
}

// currentMembership returns the membership in effect in a form that does not
// depend on which node holds it: every list includes the local node. This is
// what gets written into a snapshot.
func (n *Node) currentMembership() membershipState {
	if n.jointOld != nil {
		return membershipState{
			joint: true,
			old:   withSelf(n.jointOld, n.cfg.ID, true, n.jointSelfVoterOld),
			new:   withSelf(n.jointNew, n.cfg.ID, n.jointIncludeSelf, n.jointSelfVoter),
		}
	}
	return membershipState{
		members: withSelf(n.cfg.Peers, n.cfg.ID, true, n.cfg.Voter),
	}
}

// restoreMembership installs ms as the membership in effect, replacing whatever
// was there. Event-loop only.
func (n *Node) restoreMembership(ms membershipState) {
	if !ms.joint {
		peers, present, voter := splitSelf(ms.members, n.cfg.ID)
		n.cfg.Peers = peers
		n.cfg.Voter = present && voter
		n.jointOld, n.jointNew = nil, nil
		n.jointIncludeSelf, n.jointSelfVoter, n.jointSelfVoterOld = false, false, false
		n.storePeers()
		return
	}

	oldPeers, _, oldVoter := splitSelf(ms.old, n.cfg.ID)
	newPeers, inNew, newVoter := splitSelf(ms.new, n.cfg.ID)
	n.jointOld = oldPeers
	n.jointNew = newPeers
	n.jointSelfVoterOld = oldVoter
	n.jointIncludeSelf = inNew
	n.jointSelfVoter = newVoter
	// While joint, this node's own role is still the one from C_old; it only
	// changes when the finalise entry is adopted.
	n.cfg.Voter = oldVoter
	n.cfg.Peers = peerUnion(oldPeers, newPeers, n.cfg.ID)
	n.storePeers()
}

// withSelf returns peers plus the local node when present is true. The result
// is a fresh slice; peers is never aliased.
func withSelf(peers []PeerConfig, self NodeID, present, voter bool) []PeerConfig {
	out := make([]PeerConfig, 0, len(peers)+1)
	if present {
		out = append(out, PeerConfig{ID: self, Voter: voter})
	}
	return append(out, peers...)
}

// splitSelf separates the local node out of a membership list, reporting
// whether it was there and whether it votes.
func splitSelf(members []PeerConfig, self NodeID) (peers []PeerConfig, present, voter bool) {
	peers = make([]PeerConfig, 0, len(members))
	for _, m := range members {
		if m.ID == self {
			present, voter = true, m.Voter
			continue
		}
		peers = append(peers, m)
	}
	return peers, present, voter
}

// rebuildMembership recomputes the membership in effect from the snapshot base
// plus every config entry currently in the log. Used at startup and whenever a
// truncation removes the entry the current membership came from.
func (n *Node) rebuildMembership(ctx context.Context) error {
	n.restoreMembership(n.baseMembership)
	n.configIndex = n.log.snapMeta.LastIncludedIndex

	first, last := n.log.first, n.log.last
	if first == 0 || last < first {
		return nil
	}

	// Read in batches so a long log neither allocates one huge slice nor makes
	// one storage call per entry.
	const batch = 1024
	for lo := first; lo <= last; lo += batch {
		hi := min(lo+batch, last+1)
		entries, err := n.cfg.Storage.GetLogEntries(ctx, lo, hi)
		if err != nil {
			return err
		}
		for i := range entries {
			if isConfigEntry(entries[i].Command) {
				n.adoptConfigEntry(entries[i].Command, entries[i].Index)
			}
		}
	}
	return nil
}

// adoptConfigEntry puts a single config entry into effect, changing the
// membership only. applyConfigChange calls it on the apply path; rebuildMembership
// calls it for each entry when recovering.
func (n *Node) adoptConfigEntry(configCmd []byte, index Index) {
	op, peer, ok := decodeConfigEntry(configCmd)
	if !ok {
		return
	}
	n.configIndex = index

	switch op {
	case configOpAdd:
		if peer.ID == n.cfg.ID {
			n.cfg.Voter = peer.Voter
			return
		}
		if containsPeer(n.cfg.Peers, peer.ID) {
			return // already present
		}
		n.cfg.Peers = append(n.cfg.Peers, peer)
		n.storePeers()
		if n.state == Leader {
			n.nextIndex[peer.ID] = n.log.lastLogIndex() + 1
			n.matchIndex[peer.ID] = 0
			// inflight and snapshotInflight are zero/false by map default.
			// Start a heartbeat pump so the newly added peer receives ongoing
			// heartbeats immediately, without waiting for the next proposal.
			n.startHBPumpFor(peer.ID)
		}
		n.logger.Info("config change: added peer", "id", peer.ID, "voter", peer.Voter)

	case configOpRemove:
		id := peer.ID
		if id == n.cfg.ID {
			// Self-removal takes effect for quorum purposes immediately, but
			// stepping down waits until the entry commits (applyConfigChange).
			n.cfg.Voter = false
			n.storePeers()
			n.logger.Info("config change: self removed from membership")
			return
		}
		for i, p := range n.cfg.Peers {
			if p.ID == id {
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

	case configOpJoint:
		old, new_, ok2 := decodeJointConfigEntry(configCmd)
		if !ok2 {
			return
		}
		// The joint entry carries C_old as peers only (self excluded) and C_new
		// as a full membership that may or may not include self.
		n.restoreMembership(membershipState{
			joint: true,
			old:   withSelf(old, n.cfg.ID, true, n.cfg.Voter),
			new:   new_,
		})
		if n.state == Leader {
			nextIdx := n.log.lastLogIndex() + 1
			for _, p := range n.cfg.Peers {
				if _, exists := n.nextIndex[p.ID]; !exists {
					n.nextIndex[p.ID] = nextIdx
					n.matchIndex[p.ID] = 0
					n.startHBPumpFor(p.ID)
				}
			}
		}
		n.logger.Info("config change: entered joint consensus", "old", old, "new", new_)

	case configOpFinalise:
		members, ok2 := decodeFinaliseConfigEntry(configCmd)
		if !ok2 {
			return
		}
		oldPeers := n.cfg.Peers
		n.restoreMembership(membershipState{members: members})

		// Clean up leader tracking for peers that left the cluster.
		if n.state == Leader {
			for _, p := range oldPeers {
				if !containsPeer(n.cfg.Peers, p.ID) {
					n.stopHBPumpFor(p.ID)
					delete(n.nextIndex, p.ID)
					delete(n.matchIndex, p.ID)
					delete(n.inflight, p.ID)
					delete(n.snapshotInflight, p.ID)
				}
			}
			// Quorum size has changed; re-check whether anything can commit.
			n.maybeAdvanceCommit()
		}
		n.logger.Info("config change: finalised new membership", "peers", n.cfg.Peers)
	}
}

// applyConfigChange puts a committed config entry into effect and handles the
// consequences of it having committed. Membership changes here rather than at
// append time so that a change cannot enlarge the quorum before the cluster has
// agreed to it.
func (n *Node) applyConfigChange(configCmd []byte, index Index) {
	op, peer, ok := decodeConfigEntry(configCmd)
	if !ok {
		return
	}
	n.adoptConfigEntry(configCmd, index)

	switch op {
	case configOpRemove:
		// A node removed from the cluster stops being a leader or candidate for
		// it. Waiting for the commit matters: a removal that never commits must
		// not take a healthy leader down.
		if peer.ID == n.cfg.ID {
			n.logger.Info("config change: self removed, stepping down")
			n.becomeFollower(n.currentTerm, "")
		}

	case configOpJoint:
		// Second phase of joint consensus. C_new may only be appended once
		// C_old,new is committed, which is exactly now. If this node already
		// appended the finalise entry, the joint config is no longer in effect
		// and there is nothing to do.
		if n.state == Leader && n.jointOld != nil {
			n.appendFinaliseEntry(n.jointNew, n.jointIncludeSelf, n.jointSelfVoter)
		}

	case configOpFinalise:
		members, ok2 := decodeFinaliseConfigEntry(configCmd)
		if !ok2 {
			return
		}
		if !containsPeer(members, n.cfg.ID) {
			n.logger.Info("config change: self removed, stepping down")
			n.becomeFollower(n.currentTerm, "")
		}
	}
}

func containsPeer(peers []PeerConfig, id NodeID) bool {
	for _, p := range peers {
		if p.ID == id {
			return true
		}
	}
	return false
}

// appendFinaliseEntry appends a configOpFinalise entry that commits the new
// cluster membership. Called by the leader once the joint config entry has
// committed, to drive the second phase of joint consensus.
//
// newPeers is the list of peers (excluding self). includeSelf controls whether
// self's ID is included in the encoded membership: true means self stays in
// the cluster; false means self is removed and will step down when the
// finalise entry commits.
func (n *Node) appendFinaliseEntry(newPeers []PeerConfig, includeSelf, selfVoter bool) {
	allNew := withSelf(newPeers, n.cfg.ID, includeSelf, selfVoter)

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
	// never written. The caller will retry on the next becomeLeader invocation
	// if the node wins a subsequent election.
	n.pendingConfigIndex = idx
	n.replicateToFollowers()
	n.maybeAdvanceCommit()
}

// peerUnion returns the union of two peer lists, excluding the local node ID.
func peerUnion(a, b []PeerConfig, self NodeID) []PeerConfig {
	seen := make(map[NodeID]bool, len(a)+len(b))
	result := make([]PeerConfig, 0, len(a)+len(b))
	for _, p := range a {
		if p.ID != self && !seen[p.ID] {
			seen[p.ID] = true
			result = append(result, p)
		}
	}
	for _, p := range b {
		if p.ID != self && !seen[p.ID] {
			seen[p.ID] = true
			result = append(result, p)
		}
	}
	return result
}
