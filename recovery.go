package raft

import (
	"context"
	"fmt"
)

// ---- Disaster recovery for permanent quorum loss ----------------------------
//
// Raft keeps a cluster available while a minority is down, and refuses to make
// progress when a majority is. That refusal is the whole point: a cluster that
// committed writes without a majority could lose them. But it also means that
// losing two nodes of three, permanently, leaves a cluster that cannot elect a
// leader, cannot commit, and therefore cannot commit the very configuration
// change that would shrink it back to a size the survivor can form a quorum in.
// No amount of waiting fixes it, and no operation the running node offers can:
// every one of them needs a quorum.
//
// The way out is to rewrite the survivor's durable state while it is stopped,
// declaring a new, smaller membership it *is* a majority of, and restart it.
// The cluster comes back as that membership, and the lost nodes are re-added as
// new members afterwards.
//
// This is not a consensus operation, and it is not safe in the sense the rest
// of this package is. Two things can go wrong, and both are inherent rather
// than defects in this implementation:
//
//   - Entries this node holds but that were never committed become committed,
//     because the recovered cluster's history is whatever this node had. A
//     client that was told its write failed may find that it succeeded.
//   - Entries committed by the lost majority that never reached this node are
//     gone, because nothing that remains has them. A client that was told its
//     write succeeded may find that it did not.
//
// Recovery is therefore an operator action for a cluster that is already dead,
// not a tool for one that is merely slow or partitioned. Do not run it against
// storage a node is still using, and do not run it to sidestep an unavailable
// but intact cluster: if the lost nodes come back afterwards holding a history
// this one does not have, there is no longer a single agreed history.
//
// The procedure:
//
//  1. Stop every surviving node.
//  2. Call InspectStorage on each survivor's storage and pick the most recent,
//     with RecoveryInfo.MoreRecentThan. That is the one whose history is kept.
//  3. Call RecoverCluster on that node's storage, naming a membership it is a
//     voting majority of. Naming only itself is the usual choice.
//  4. Erase the storage of every other survivor. Their logs are now divergent
//     history: a node that restarts holding entries the recovered node does
//     not have can disrupt the elections of the cluster it is no longer part
//     of. Recovering more than one node has the same problem for the same
//     reason, so recover exactly one.
//  5. Restart the recovered node. It elects itself and serves.
//  6. Add the wiped nodes back with AddMember, as learners. They receive a
//     snapshot and catch up the ordinary way.

// RecoveryInfo is the durable state one stopped node holds, as read by
// InspectStorage. It is what an operator compares across survivors to decide
// which node's history a recovered cluster should keep.
type RecoveryInfo struct {
	// Term is the highest term the node had seen, and VotedFor the vote it had
	// cast in it, if any.
	Term     Term
	VotedFor NodeID

	// FirstIndex and LastIndex bound the entries still in the log. FirstIndex
	// is 0 when the log is empty; LastIndex is then the snapshot's last
	// included index, since that is the last entry the node accounts for.
	FirstIndex Index
	LastIndex  Index
	// LastTerm is the term of the entry at LastIndex. Together with LastIndex
	// it is the pair Raft elections compare, which is why MoreRecentThan uses
	// exactly these two.
	LastTerm Term

	// SnapshotIndex and SnapshotTerm describe the node's newest snapshot, and
	// are 0 when it has none.
	SnapshotIndex Index
	SnapshotTerm  Term

	// Members is the membership the node would restart with: the one recorded
	// in its snapshot, advanced by every configuration entry in its log,
	// committed or not. It always includes the node itself, unless the cluster
	// removed it.
	Members []PeerConfig
	// JointMembers is non-nil when the node's last configuration entry started
	// a joint reconfiguration that never finished. Members is then the old
	// configuration and JointMembers the proposed new one.
	JointMembers []PeerConfig
	// MembersComplete reports whether Members was recovered in full from the
	// node's own storage.
	//
	// It is false for a node that has neither a snapshot membership to build on
	// nor an entry in its log recording a whole configuration -- the state a
	// cluster is in from bootstrap until its first snapshot or its first
	// reconfiguration. The membership then lives only in the peer list the
	// operator passes to New, which storage has never seen, and Members holds
	// no more than what the log adds to it.
	MembersComplete bool
}

// MoreRecentThan reports whether r holds a log at least as complete as other's,
// by the rule Raft elections use: the higher last term wins, and the longer log
// breaks a tie.
//
// It is how the survivor whose history a recovered cluster keeps should be
// chosen. Choosing any other node silently discards whatever the more recent
// one had and the chosen one did not.
func (r *RecoveryInfo) MoreRecentThan(other *RecoveryInfo) bool {
	if r.LastTerm != other.LastTerm {
		return r.LastTerm > other.LastTerm
	}
	return r.LastIndex > other.LastIndex
}

// InspectStorage reads the durable state of a stopped node without modifying
// it. The node must not be running: the values would be a mix of before and
// after whatever it is doing.
//
// It makes no assumption that the storage belongs to a healthy cluster; reading
// it is the first step of deciding whether one can be recovered.
func InspectStorage(ctx context.Context, store Storage) (RecoveryInfo, error) {
	var info RecoveryInfo

	hs, err := store.LoadHardState(ctx)
	if err != nil {
		return RecoveryInfo{}, fmt.Errorf("raft.InspectStorage: load hard state: %w", err)
	}
	info.Term, info.VotedFor = hs.CurrentTerm, hs.VotedFor

	ms, complete, err := snapshotMembership(ctx, store, &info)
	if err != nil {
		return RecoveryInfo{}, err
	}

	first, err := store.FirstIndex()
	if err != nil {
		return RecoveryInfo{}, fmt.Errorf("raft.InspectStorage: first index: %w", err)
	}
	last, err := store.LastIndex()
	if err != nil {
		return RecoveryInfo{}, fmt.Errorf("raft.InspectStorage: last index: %w", err)
	}
	info.FirstIndex = first

	if last == 0 {
		// Fully compacted, or never written. The snapshot is what the node
		// accounts for, exactly as raftLog.lastLogIndex treats it.
		info.LastIndex, info.LastTerm = info.SnapshotIndex, info.SnapshotTerm
	} else {
		info.LastIndex = last
		info.LastTerm, complete, err = replayConfigEntries(ctx, store, &ms, complete, first, last)
		if err != nil {
			return RecoveryInfo{}, err
		}
	}

	if ms.joint {
		info.Members, info.JointMembers = ms.old, ms.new
	} else {
		info.Members = ms.members
	}
	info.MembersComplete = complete
	return info, nil
}

// snapshotMembership reads the node's snapshot, filling in the snapshot fields
// of info and returning the membership recorded in it. A node with no snapshot,
// or with one written before membership was stored inside snapshots, yields an
// empty membership: the log is then the only source.
func snapshotMembership(ctx context.Context, store Storage, info *RecoveryInfo) (membershipState, bool, error) {
	meta, r, err := store.LoadSnapshot(ctx)
	if err == ErrNoSnapshot {
		return membershipState{}, false, nil
	}
	if err != nil {
		return membershipState{}, false, fmt.Errorf("raft.InspectStorage: load snapshot: %w", err)
	}
	defer func() { _ = r.Close() }()

	info.SnapshotIndex = meta.LastIncludedIndex
	info.SnapshotTerm = meta.LastIncludedTerm

	_, ms, hasMS, _, err := readWrappedSnapshot(r)
	if err != nil {
		return membershipState{}, false, fmt.Errorf("raft.InspectStorage: read snapshot framing: %w", err)
	}
	if !hasMS {
		return membershipState{}, false, nil
	}
	return ms, true, nil
}

// replayConfigEntries advances ms through every configuration entry in
// [first, last], and returns the term of the entry at last along the way.
//
// It is the standalone form of Node.rebuildMembership: a node adopts the last
// configuration in its log whether or not it committed, so that is what the
// node would restart with, and therefore what an operator needs to see.
func replayConfigEntries(ctx context.Context, store Storage, ms *membershipState, complete bool, first, last Index) (Term, bool, error) {
	var lastTerm Term
	const batch = 1024
	for lo := first; lo <= last; lo += batch {
		hi := min(lo+batch, last+1)
		entries, err := store.GetLogEntries(ctx, lo, hi)
		if err != nil {
			return 0, false, fmt.Errorf("raft.InspectStorage: read entries [%d,%d): %w", lo, hi, err)
		}
		if len(entries) == 0 {
			return 0, false, fmt.Errorf("raft.InspectStorage: read entries [%d,%d): no entries returned", lo, hi)
		}
		for i := range entries {
			if !isConfigEntry(entries[i].Command) {
				continue
			}
			complete = adoptMembershipEntry(ms, entries[i].Command) || complete
		}
		lastTerm = entries[len(entries)-1].Term
	}
	return lastTerm, complete, nil
}

// adoptMembershipEntry applies one configuration entry to ms, and reports
// whether that entry recorded a whole configuration rather than an adjustment
// to one the caller had to already know.
//
// Unlike Node.adoptConfigEntry this keeps every list whole, including the local
// node, because there is no local node here: the caller is a tool inspecting
// somebody else's storage.
func adoptMembershipEntry(ms *membershipState, cmd []byte) bool {
	op, peer, ok := decodeConfigEntry(cmd)
	if !ok {
		return false
	}
	switch op {
	case configOpAdd:
		if ms.joint {
			ms.old, ms.new = upsertPeer(ms.old, peer), upsertPeer(ms.new, peer)
		} else {
			ms.members = upsertPeer(ms.members, peer)
		}
	case configOpRemove:
		if ms.joint {
			ms.old, ms.new = removePeer(ms.old, peer.ID), removePeer(ms.new, peer.ID)
		} else {
			ms.members = removePeer(ms.members, peer.ID)
		}
	case configOpJoint:
		_, newMembers, ok := decodeJointConfigEntry(cmd)
		if !ok {
			return false
		}
		// C_old is the membership that was in effect when the entry was
		// written. The entry also carries it, but without whoever wrote it,
		// so what we already have is the more complete of the two.
		old := ms.members
		if ms.joint {
			old = ms.old
		}
		*ms = membershipState{joint: true, old: old, new: newMembers}
	case configOpFinalise:
		members, ok := decodeFinaliseConfigEntry(cmd)
		if !ok {
			return false
		}
		// The one entry that states a configuration outright: whatever the
		// caller knew before it no longer matters.
		*ms = membershipState{members: members}
		return true
	}
	return false
}

// upsertPeer returns peers with p added, or with p's role updated if it is
// already there. The input slice is never modified.
func upsertPeer(peers []PeerConfig, p PeerConfig) []PeerConfig {
	out := append([]PeerConfig(nil), peers...)
	if i := indexOfPeer(out, p.ID); i >= 0 {
		out[i].Voter = p.Voter
		return out
	}
	return append(out, p)
}

// removePeer returns peers without id. The input slice is never modified.
func removePeer(peers []PeerConfig, id NodeID) []PeerConfig {
	out := make([]PeerConfig, 0, len(peers))
	for _, p := range peers {
		if p.ID != id {
			out = append(out, p)
		}
	}
	return out
}

// RecoverCluster rewrites the durable state of a stopped node so that, when it
// restarts, it belongs to members rather than to whatever membership its log
// records. self is that node's own ID and must appear in members as a voter.
//
// It is the escape from permanent quorum loss, and it trades safety for
// availability to get there: read the commentary at the top of this file before
// using it. In short, it must be run on exactly one survivor, chosen with
// InspectStorage, with every other survivor's storage erased, against a cluster
// that is not coming back on its own.
//
// members is the membership the restarted node adopts, and self must be its
// only voter: a recovered node that is not a majority by itself cannot elect a
// leader either, so nothing would have been gained. Non-voters may be listed
// alongside it, which is how the wiped nodes can be named up front instead of
// being added once the cluster is back; either way they catch up from a
// snapshot, and either way they are promoted to voters afterwards with
// Node.PromoteMember.
//
// Nothing about the node's log or state machine changes. Everything it had
// committed it still has; the recovery entry is appended at the end, in a term
// higher than any the node has seen, so that it is the last word on the
// cluster's membership however the log ended.
//
// The node must be stopped and this must be the only open handle on its
// storage. Running it against a live node races the node's own writes, and
// neither the engine nor any Storage implementation can detect that.
func RecoverCluster(ctx context.Context, store Storage, self NodeID, members []PeerConfig) error {
	if err := validateRecoveryMembers(self, members); err != nil {
		return err
	}

	info, err := InspectStorage(ctx, store)
	if err != nil {
		return err
	}

	// A term above everything the node has seen, so that the recovery entry
	// wins every comparison against what the log already holds, and so that
	// the restarted node can win an election against any stale peer that is
	// brought back by mistake. The log's own last term is considered as well
	// as the hard state's: storage that disagrees with itself is exactly the
	// kind of state recovery exists to clean up.
	term := max(info.Term, info.LastTerm) + 1

	// Hard state first. A crash between the two writes must not leave a log
	// holding an entry from a term the node does not believe it reached; the
	// other order -- a term with no entry to go with it -- costs nothing but a
	// repeat of the recovery.
	if err := store.SaveHardState(ctx, HardState{CurrentTerm: term}); err != nil {
		return fmt.Errorf("raft.RecoverCluster: save hard state: %w", err)
	}

	entry := LogEntry{
		Index:   info.LastIndex + 1,
		Term:    term,
		Command: encodeFinaliseConfigEntry(members),
	}
	if err := store.AppendLogEntries(ctx, []LogEntry{entry}); err != nil {
		return fmt.Errorf("raft.RecoverCluster: append membership entry: %w", err)
	}
	return nil
}

// validateRecoveryMembers rejects a membership that cannot produce a working
// cluster, before anything has been written.
func validateRecoveryMembers(self NodeID, members []PeerConfig) error {
	if self == "" {
		return fmt.Errorf("raft.RecoverCluster: self must not be empty")
	}
	if len(members) == 0 {
		return fmt.Errorf("raft.RecoverCluster: members must not be empty")
	}
	seen := make(map[NodeID]bool, len(members))
	voters := 0
	for _, m := range members {
		if m.ID == "" {
			return fmt.Errorf("raft.RecoverCluster: members contains an empty node ID")
		}
		if seen[m.ID] {
			return fmt.Errorf("raft.RecoverCluster: members contains %q twice", m.ID)
		}
		seen[m.ID] = true
		if m.Voter {
			voters++
		}
	}
	i := indexOfPeer(members, self)
	if i < 0 || !members[i].Voter {
		return fmt.Errorf("raft.RecoverCluster: %q must appear in members as a voter: %w", self, ErrNotMember)
	}
	// The recovered node has to be able to elect itself, or the cluster is
	// still stuck and the entries recovery promoted to committed have been
	// risked for nothing.
	if voters > 1 {
		return fmt.Errorf("raft.RecoverCluster: members has %d voters, and %q cannot form a quorum alone; "+
			"recover to a single-voter membership and add the others back once it is serving", voters, self)
	}
	return nil
}
