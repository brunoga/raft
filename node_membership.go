package raft

import (
	"context"
	"fmt"
)

// ---- Membership changes -----------------------------------------------------

// storePeers snapshots the current cfg.Peers slice into atomicPeers so that
// callers outside the event loop (e.g. ReconfigureCluster) can read the peer
// list without a data race. Must be called from the event-loop goroutine after
// every mutation to n.cfg.Peers.
func (n *Node) storePeers() {
	snap := make([]PeerConfig, len(n.cfg.Peers))
	copy(snap, n.cfg.Peers)
	n.atomicPeers.Store(snap)
}

// applyConfigChange updates the in-memory peer list when a committed
// config-change log entry is applied. It is called for every node (leader and
// follower) once the entry is committed.
func (n *Node) applyConfigChange(configCmd []byte) {
	op, peer, ok := decodeConfigEntry(configCmd)
	if !ok {
		return
	}
	switch op {
	case configOpAdd:
		if peer.ID == n.cfg.ID {
			n.cfg.Voter = peer.Voter
			return
		}
		if i := indexOfPeer(n.cfg.Peers, peer.ID); i >= 0 {
			// Already a member. The entry still carries a role, so this is how
			// a learner is promoted to a voter, or a voter demoted: applying it
			// as a no-op would silently ignore the change.
			if n.cfg.Peers[i].Voter != peer.Voter {
				n.cfg.Peers[i].Voter = peer.Voter
				n.storePeers()
				n.logger.Info("config change: changed peer role",
					"id", peer.ID, "voter", peer.Voter)
				if n.state == Leader {
					// The set of voters changed, so the quorum may have too.
					n.maybeAdvanceCommit()
				}
			}
			return
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
		n.jointSelfVoter = false
		newPeers := make([]PeerConfig, 0, len(new_))
		for _, m := range new_ {
			if m.ID == n.cfg.ID {
				n.jointIncludeSelf = true
				n.jointSelfVoter = m.Voter
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
				if _, exists := n.nextIndex[p.ID]; !exists {
					n.nextIndex[p.ID] = nextIdx
					n.matchIndex[p.ID] = 0
					n.startHBPumpFor(p.ID)
				}
			}
		}
		n.cfg.Peers = union
		n.storePeers()
		n.logger.Info("config change: entered joint consensus",
			"old", old, "new", new_)

		// The leader auto-appends the finalise entry to drive the second phase.
		if n.state == Leader {
			n.appendFinaliseEntry(n.jointNew, n.jointIncludeSelf, n.jointSelfVoter)
		}

	case configOpFinalise:
		members, ok2 := decodeFinaliseConfigEntry(configCmd)
		if !ok2 {
			return
		}
		oldPeers := n.cfg.Peers

		// cfg.Peers becomes the finalised new membership, excluding self.
		newPeers := make([]PeerConfig, 0, len(members))
		selfInNew := false
		selfVoter := false
		for _, m := range members {
			if m.ID == n.cfg.ID {
				selfInNew = true
				selfVoter = m.Voter
			} else {
				newPeers = append(newPeers, m)
			}
		}

		// Clean up leader tracking for peers that are no longer in the cluster.
		if n.state == Leader {
			for _, p := range oldPeers {
				if !containsPeer(newPeers, p.ID) {
					delete(n.nextIndex, p.ID)
					delete(n.matchIndex, p.ID)
					delete(n.inflight, p.ID)
					delete(n.snapshotInflight, p.ID)
				}
			}
		}

		n.cfg.Peers = newPeers
		n.cfg.Voter = selfVoter
		n.storePeers()
		n.jointOld = nil
		n.jointNew = nil
		n.jointIncludeSelf = false
		n.jointSelfVoter = false
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

func containsPeer(peers []PeerConfig, id NodeID) bool {
	return indexOfPeer(peers, id) >= 0
}

// indexOfPeer returns the position of id in peers, or -1.
func indexOfPeer(peers []PeerConfig, id NodeID) int {
	for i, p := range peers {
		if p.ID == id {
			return i
		}
	}
	return -1
}

// appendFinaliseEntry appends a configOpFinalise entry that commits the new
// cluster membership. Called by the leader after applying a joint config entry
// to drive the second phase of joint consensus.
//
// newPeers is the list of peers (excluding self). includeSelf controls whether
// self's ID is included in the encoded membership: true means self stays in
// the cluster; false means self is removed and will step down when the
// finalise entry is applied.
func (n *Node) appendFinaliseEntry(newPeers []PeerConfig, includeSelf, selfVoter bool) {
	// Build the complete new membership.
	var allNew []PeerConfig
	if includeSelf {
		allNew = make([]PeerConfig, 0, len(newPeers)+1)
		allNew = append(allNew, PeerConfig{ID: n.cfg.ID, Voter: selfVoter})
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

// ---- Replication progress and promotion -------------------------------------

// PeerProgress is the leader's view of how far one peer has kept up.
type PeerProgress struct {
	// ID identifies the peer.
	ID NodeID `json:"id"`
	// Voter reports whether the peer votes and counts towards quorums.
	Voter bool `json:"voter"`
	// MatchIndex is the highest log index the leader knows this peer has
	// stored. Zero means the leader has not yet confirmed anything with it.
	MatchIndex Index `json:"match_index"`
	// NextIndex is the index the leader will send next.
	NextIndex Index `json:"next_index"`
	// LeaderLastIndex is the leader's own last log index, so that a caller can
	// read the lag straight off without a second call that might see a
	// different moment.
	LeaderLastIndex Index `json:"leader_last_index"`
	// SendingSnapshot reports whether a snapshot transfer to this peer is in
	// progress, which is why a peer far behind may show no forward movement.
	SendingSnapshot bool `json:"sending_snapshot"`
}

// Lag returns how many entries this peer is behind the leader.
func (p PeerProgress) Lag() Index {
	if p.MatchIndex >= p.LeaderLastIndex {
		return 0
	}
	return p.LeaderLastIndex - p.MatchIndex
}

// progressRequest asks the event loop for the leader's per-peer progress.
type progressRequest struct{}

// replicationProgress builds the per-peer view. Event-loop only.
func (n *Node) replicationProgress() []PeerProgress {
	if n.state != Leader {
		return nil
	}
	last := n.log.lastLogIndex()
	out := make([]PeerProgress, 0, len(n.cfg.Peers))
	for _, p := range n.cfg.Peers {
		out = append(out, PeerProgress{
			ID:              p.ID,
			Voter:           p.Voter,
			MatchIndex:      n.matchIndex[p.ID],
			NextIndex:       n.nextIndex[p.ID],
			LeaderLastIndex: last,
			SendingSnapshot: n.snapshotInflight[p.ID],
		})
	}
	return out
}

// ReplicationProgress returns the leader's view of how far each peer has kept
// up. It returns ErrNotLeader on any other node, since only the leader tracks
// this.
//
// Use it to decide whether a cluster is healthy enough for a change that
// depends on replicas keeping up: promoting a learner, removing a member,
// handing over leadership, or taking a node out for maintenance.
func (n *Node) ReplicationProgress(ctx context.Context) ([]PeerProgress, error) {
	if n.State() != Leader {
		return nil, &NotLeaderError{Leader: n.Leader()}
	}
	progress, err := dispatchRPC[[]PeerProgress](ctx, n, &progressRequest{})
	if err != nil {
		return nil, err
	}
	if progress == nil {
		// Leadership was lost between the check above and the event loop
		// answering.
		return nil, &NotLeaderError{Leader: n.Leader()}
	}
	return progress, nil
}

// PromoteMember makes an existing non-voting member a voter, but only once it
// has caught up to within maxLag entries of the leader.
//
// Adding a voter changes the quorum immediately. A member promoted while it is
// still far behind counts towards that larger quorum without being able to
// help satisfy it: a three-node cluster that promotes a fourth, empty member
// goes from tolerating one failure to tolerating none until that member
// catches up. The usual safe sequence is to add a member as a non-voter, let it
// replicate, and promote it once it is close — which is what this method exists
// to make easy to do correctly.
//
// maxLag of zero means the member must have replicated everything the leader
// has. Returns ErrMemberNotCaughtUp when the member is further behind than
// that, ErrNotMember when it is not part of the cluster, and ErrNotLeader when
// called on a node that is not the leader.
func (n *Node) PromoteMember(ctx context.Context, id NodeID, maxLag Index) error {
	progress, err := n.ReplicationProgress(ctx)
	if err != nil {
		return err
	}

	for _, p := range progress {
		if p.ID != id {
			continue
		}
		if p.Voter {
			return nil // already a voter; nothing to do
		}
		if p.Lag() > maxLag {
			return fmt.Errorf("%w: %s is %d entries behind, limit is %d",
				ErrMemberNotCaughtUp, id, p.Lag(), maxLag)
		}
		return n.AddServer(ctx, PeerConfig{ID: id, Voter: true})
	}
	return fmt.Errorf("%w: %s", ErrNotMember, id)
}
