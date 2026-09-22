package raft

import (
	"context"
	"errors"
	"fmt"
	"time"
)

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

// storeMembership snapshots the membership held in cfg -- the peer list and
// this node's own role -- into the atomic mirrors, so that callers outside the
// event loop (Members, Status, ReconfigureCluster) can read it without a data
// race. Must be called from the event-loop goroutine after every mutation to
// n.cfg.Peers or n.cfg.Voter.
func (n *Node) storeMembership() {
	snap := make([]PeerConfig, len(n.cfg.Peers))
	copy(snap, n.cfg.Peers)
	n.atomicPeers.Store(snap)
	n.atomicVoter.Store(n.cfg.Voter)
}

// currentMembership returns the membership in effect in a form that does not
// depend on which node holds it: every list includes the local node. This is
// what gets written into a snapshot.
func (n *Node) currentMembership() membershipState {
	if n.jointOld != nil {
		return membershipState{
			joint:        true,
			old:          withSelf(n.jointOld, n.cfg.ID, true, n.jointSelfVoterOld),
			new:          withSelf(n.jointNew, n.cfg.ID, n.jointIncludeSelf, n.jointSelfVoter),
			commitQuorum: n.commitQuorumApplied,
		}
	}
	return membershipState{
		members:      withSelf(n.cfg.Peers, n.cfg.ID, true, n.cfg.Voter),
		commitQuorum: n.commitQuorumApplied,
	}
}

// restoreMembership installs ms as the membership in effect, replacing whatever
// was there. Event-loop only.
func (n *Node) restoreMembership(ms *membershipState) {
	n.commitQuorumLatest = ms.commitQuorum
	if !ms.joint {
		peers, present, voter := splitSelf(ms.members, n.cfg.ID)
		n.cfg.Peers = peers
		n.cfg.Voter = present && voter
		n.jointOld, n.jointNew = nil, nil
		n.jointIncludeSelf, n.jointSelfVoter, n.jointSelfVoterOld = false, false, false
		n.storeMembership()
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
	n.storeMembership()
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
	n.restoreMembership(&n.baseMembership)
	n.configIndex = n.log.snapMeta.LastIncludedIndex
	// Whether the group has agreed a client table bound is rebuilt the same
	// way: from the snapshot, then from any cap entry in the log. The bound
	// on the table itself is not touched here -- it follows the apply order,
	// and the entries replayed below have not been applied.
	n.clientTableCapReplicated = n.hasBaseClientTableCap
	// Likewise the commit quorum the log knows of starts from the snapshot's
	// and is updated by every quorum entry replayed below; the one in effect
	// follows the apply order and is not touched here.
	n.commitQuorumLatest = n.baseMembership.commitQuorum

	first, last := n.log.first, n.log.last
	if first == 0 || last < first {
		return nil
	}

	// Read in batches so a long log neither allocates one huge slice nor makes
	// one storage call per entry.
	const batch = 1024
	for lo := first; lo <= last; lo += batch {
		hi := min(lo+batch, last+1)
		// Through the log: a rebuild triggered by a truncation runs while the
		// entries that replaced the discarded ones are still only in memory,
		// and a config change among them is as binding as any other.
		entries, err := n.log.entries(ctx, lo, hi)
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
	case configOpClientTableCap:
		// The log knows of a bound; the table adopts it when the entry is
		// applied. See applyConfigChange.
		n.clientTableCapReplicated = true
		return

	case configOpCommitQuorum:
		// In the log from here on: every decision now requires the stricter
		// of this and the policy in effect, until the entry is applied.
		if quorum, ok := decodeCommitQuorumEntry(configCmd); ok {
			n.commitQuorumLatest = quorum
			if n.state == Leader {
				// A stricter commit quorum can uncommit nothing, but a
				// looser one that was pending may now be met.
				n.maybeAdvanceCommit()
			}
		}
		return

	case configOpAdd:
		if peer.ID == n.cfg.ID {
			// This node's own promotion or demotion. The peer list is
			// unchanged, but the mirror still has to be refreshed: the role in
			// it is what every reader outside the event loop sees.
			n.cfg.Voter = peer.Voter
			n.storeMembership()
			return
		}
		if i := indexOfPeer(n.cfg.Peers, peer.ID); i >= 0 {
			// Already a member. The entry still carries a role, so this is how
			// a learner is promoted to a voter, or a voter demoted: applying it
			// as a no-op would silently ignore the change.
			if n.cfg.Peers[i].Voter != peer.Voter {
				n.cfg.Peers[i].Voter = peer.Voter
				n.storeMembership()
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
		n.storeMembership()
		if n.state == Leader {
			if _, leaving := n.departing[peer.ID]; leaving {
				// Re-added before it had been told it was removed. Its
				// progress is still being tracked and its pump still runs;
				// it is simply a member again.
				delete(n.departing, peer.ID)
			} else {
				n.nextIndex[peer.ID] = n.log.lastLogIndex() + 1
				n.matchIndex[peer.ID] = 0
				// inflight and snapshotInflight are zero/false by map default.
				// Start a heartbeat pump so the newly added peer receives
				// ongoing heartbeats immediately, without waiting for the
				// next proposal.
				n.startHBPumpFor(peer.ID)
			}
		}
		n.logger.Info("config change: added peer", "id", peer.ID, "voter", peer.Voter)

	case configOpRemove:
		id := peer.ID
		if id == n.cfg.ID {
			// Self-removal takes effect for quorum purposes immediately, but
			// stepping down waits until the entry commits (applyConfigChange).
			n.cfg.Voter = false
			n.storeMembership()
			n.logger.Info("config change: self removed from membership")
			return
		}
		for i, p := range n.cfg.Peers {
			if p.ID == id {
				n.cfg.Peers = append(n.cfg.Peers[:i], n.cfg.Peers[i+1:]...)
				break
			}
		}
		n.storeMembership()
		if n.state == Leader {
			n.beginDeparture(id, index)
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
		n.restoreMembership(&membershipState{
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
		n.restoreMembership(&membershipState{members: members})

		// Peers that left the cluster are told so before their tracking is
		// dropped.
		if n.state == Leader {
			for _, p := range oldPeers {
				if containsPeer(n.cfg.Peers, p.ID) {
					continue
				}
				n.beginDeparture(p.ID, index)
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
	// Observers are told what the entry did by comparing the membership before
	// it with the membership after, which is the one description that holds for
	// every shape a config entry comes in. The report is deferred because self
	// removal is only recognised further down, and because an entry that
	// changed nothing must report nothing.
	before := n.membershipRoles()
	n.adoptConfigEntry(configCmd, index)
	after := n.membershipRoles()
	// Deferred, and after is amended rather than re-read, because a node that
	// removes itself keeps its own ID in cfg -- it goes on running as a
	// follower -- so the fact that it is no longer a member is recorded by the
	// switch below deleting it from this map. Re-reading the membership here
	// would put it back.
	defer func() { n.emitMembershipChanges(before, after) }()

	switch op {
	case configOpClientTableCap:
		if capacity, ok := decodeClientTableCapEntry(configCmd); ok {
			n.adoptClientTableCap(capacity, "log")
		}
		if n.capEntryPending == index {
			n.capEntryPending = 0
		}

	case configOpCommitQuorum:
		if quorum, ok := decodeCommitQuorumEntry(configCmd); ok {
			n.adoptCommitQuorum(quorum, "log")
			if n.state == Leader {
				n.maybeAdvanceCommit()
			}
		}
		if n.quorumEntryPending == index {
			n.quorumEntryPending = 0
		}

	case configOpRemove:
		// A node removed from the cluster stops being a leader or candidate for
		// it. Waiting for the commit matters: a removal that never commits must
		// not take a healthy leader down.
		if peer.ID == n.cfg.ID {
			// Self-removal keeps this node's ID in cfg, since it goes on
			// running; what changed is that it is no longer a member.
			delete(after, n.cfg.ID)
			n.logger.Info("config change: self removed, stepping down")
			n.becomeFollower(n.currentTerm, "")
			n.notifyRemoved()
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
			delete(after, n.cfg.ID)
			n.logger.Info("config change: self removed, stepping down")
			n.becomeFollower(n.currentTerm, "")
			n.notifyRemoved()
		}
	}
}

// notifyRemoved invokes Config.OnRemoved, once, after a committed
// configuration change has removed this node from the cluster. Event-loop
// only.
//
// The callback runs on its own goroutine because the one thing it is most
// likely to do is call Stop, and Stop waits for the event loop to exit.
func (n *Node) notifyRemoved() {
	if n.removedNotified {
		return
	}
	n.removedNotified = true
	if n.cfg.OnRemoved != nil {
		go n.cfg.OnRemoved()
	}
}

// ---- Departing peers --------------------------------------------------------
//
// A peer removed by a configuration change leaves the membership the moment
// the leader appends the entry, and from then on it counts towards nothing.
// But a leader that also stopped talking to it at that moment would leave it
// with no way to find out: the removal entry, and the commit index that makes
// it binding, both arrive by AppendEntries. Such a node keeps believing it is
// a voter, times out, and campaigns against a cluster that ignores it, for
// ever, and its Config.OnRemoved never fires.
//
// So a removed peer is kept as a departing one: replicated to and sent
// heartbeats exactly as before, but never counted, until it has acknowledged
// an AppendEntries whose commit index covers its removal. That is the message
// that makes it step down and report the removal. A departure is bounded, so
// that a peer removed because it is dead does not keep a pump for ever, and
// abandoned rather than served with a snapshot, which is more than a node on
// its way out is owed.

// departure is what a leader remembers about a peer it has removed but is
// still telling about the removal.
type departure struct {
	// index is the config entry that removed the peer. The departure ends
	// once the peer has acknowledged a commit index at or above it.
	index Index
	// elapsed counts ticks since the departure began.
	elapsed int
}

// departureGraceTicks is how many election timeouts, at the longest, a
// leader spends telling a removed peer about its removal before giving up on
// it. The removal has to commit first, which is one round trip to a quorum,
// and the goodbye is one more to the peer itself; a peer that cannot be
// reached in this long is not listening.
const departureGraceTicks = 4

// beginDeparture starts telling peer that the config entry at index removed
// it. Event-loop only; leader only.
func (n *Node) beginDeparture(peer NodeID, index Index) {
	if n.state != Leader {
		return
	}
	if n.departing == nil {
		n.departing = make(map[NodeID]departure)
	}
	n.departing[peer] = departure{index: index}
	// Push the entry now rather than at the next heartbeat.
	n.replicateToPeer(peer)
}

// endDeparture stops tracking a departing peer, whether or not it was told.
// Event-loop only.
func (n *Node) endDeparture(peer NodeID) {
	delete(n.departing, peer)
	n.stopHBPumpFor(peer)
	delete(n.nextIndex, peer)
	delete(n.matchIndex, peer)
	delete(n.inflight, peer)
	delete(n.snapshotInflight, peer)
	delete(n.peerHealth, peer)
}

// departingPeer handles an AppendEntries result from a peer that is on its way
// out, and reports whether the result was one. A departing peer's progress is
// tracked so that it can be caught up to its removal entry, but it counts
// towards nothing, so none of the leader's other bookkeeping applies to it.
// Event-loop only.
func (n *Node) departingPeer(r *appendResult) bool {
	d, ok := n.departing[r.peer]
	if !ok {
		return false
	}
	if r.success && r.req.LeaderCommit >= d.index {
		// It has the entry and knows it committed: it will step down on its
		// own from here.
		n.logger.Info("removed peer told of its removal", "id", r.peer)
		n.endDeparture(r.peer)
		return true
	}
	if r.success && len(r.req.Entries) > 0 {
		last := r.req.Entries[len(r.req.Entries)-1].Index
		if last > n.matchIndex[r.peer] {
			n.matchIndex[r.peer] = last
			n.nextIndex[r.peer] = last + 1
		}
	}
	// Either it is still behind, or it has the entry but has not yet been
	// sent a commit index covering it. In both cases the next message is the
	// same: whatever entries it lacks, with the current commit index.
	if n.nextIndex[r.peer] <= n.log.lastLogIndex() || n.commitIndex >= d.index {
		n.replicateToPeer(r.peer)
	}
	return true
}

// tickDepartures ages every departure and gives up on those that have gone on
// too long. Event-loop only; leader only.
func (n *Node) tickDepartures() {
	for peer, d := range n.departing {
		d.elapsed++
		if d.elapsed < departureGraceTicks*n.electionMaxTicks {
			n.departing[peer] = d
			continue
		}
		n.logger.Warn("removed peer could not be told of its removal; giving up",
			"id", peer)
		n.endDeparture(peer)
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
	seq := n.log.appendOne(entry)
	// pendingConfigIndex is cleared again if the write fails, so that a failed
	// finalise entry does not block future config changes behind an index that
	// was never written. In practice a failed write stops the node, and the
	// retry comes from whichever node wins the next election.
	n.pendingConfigIndex = idx
	n.afterWrite(seq, nil, func(err error) {
		n.logger.Error("appendFinaliseEntry: append", "err", err)
		if n.pendingConfigIndex == idx {
			n.pendingConfigIndex = 0
		}
	})
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
// AddVoter brings a new node into the cluster as a voter without ever letting
// it weaken the quorum on the way in.
//
// This is the safe way to grow a cluster, and the reason it exists is that the
// obvious way is not. AddServer with Voter true makes the new node count
// towards every quorum from the moment the change commits, while its log is
// still empty: a three-node cluster becomes a four-node cluster needing three
// votes, one of which cannot be given until the new node has caught up. The
// cluster has gone from tolerating one failure to tolerating none, for however
// long the catch-up takes, which on a large state machine is the worst moment
// to have done it.
//
// So the node is added as a learner first, which costs nothing -- a learner
// replicates the log and votes on nothing -- and promoted to voter only once
// it is within maxLag entries of the leader. A maxLag of zero means it must
// have replicated everything.
//
// It blocks until the promotion commits or ctx is cancelled, which for a node
// starting from nothing can be a long time; the context is the caller's way of
// bounding that. A node already a voter is left alone and nil is returned.
//
// If leadership moves while this is waiting, it returns ErrNotLeader and the
// caller should call it again on the new leader. The work already done is not
// lost: the learner is a committed member and stays one.
func (n *Node) AddVoter(ctx context.Context, id NodeID, maxLag Index) error {
	if id == "" {
		return errors.New("raft: AddVoter: empty node ID")
	}

	// Add it as a learner unless it is already a member. A learner that is
	// already there is exactly where this wants it.
	member, voter := n.memberRole(id)
	switch {
	case member && voter:
		return nil
	case !member:
		if err := n.AddServer(ctx, PeerConfig{ID: id, Voter: false}); err != nil {
			return err
		}
	}

	// Wait for it to catch up, then promote. PromoteMember re-checks the lag
	// on the leader itself, so the decision is never made from a stale view.
	for {
		err := n.PromoteMember(ctx, id, maxLag)
		if !errors.Is(err, ErrMemberNotCaughtUp) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-n.stopCh:
			return n.stoppedErr()
		case <-time.After(n.cfg.HeartbeatInterval):
		}
	}
}

// memberRole reports whether id is in the membership this node currently
// believes in, and whether it votes. Safe for concurrent use.
func (n *Node) memberRole(id NodeID) (member, voter bool) {
	for _, m := range n.Members() {
		if m.ID == id {
			return true, m.Voter
		}
	}
	return false, false
}

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
