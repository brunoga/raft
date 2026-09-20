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
// This is not a consensus operation, and it cannot be made safe in the sense
// the rest of this package is. Two things go wrong, and they are not the same
// kind of problem.
//
// The first is not fixable by anything. Entries the lost majority committed
// that never reached this node are gone, because nothing that remains has
// them: a client told its write succeeded may find that it did not. Raft's
// guarantee is that a committed entry is on a majority, so losing a majority
// is losing the guarantee, and no algorithm run afterwards recovers what no
// surviving disk holds. The answer to it is not recovery but not needing it:
// more voters, Config.MinCommitZones to spread them across failure domains,
// and backups taken off the cluster.
//
// The second is bounded, and this package bounds it. The node's log splits at
// the highest index it can prove was committed -- RecoveryInfo.KnownCommittedIndex.
// Below that, everything is certain. Above it, up to the end of the log, each
// entry either committed on the majority that died or was still in flight, and
// nothing that survives can say which. That band is what recovery has to
// decide about, and there are only two ways to decide:
//
//   - Keep it, and entries that were never committed become committed: a
//     client told its write failed may find that it succeeded. This is the
//     default, because it is what an operator trying to get their data back
//     almost always wants.
//   - Discard it, and entries that had committed but that this node had not
//     yet applied are thrown away on top of whatever the first problem already
//     cost. DiscardUncommitted does this, for a system where a write reported
//     as failed must stay failed.
//
// Neither is safe; which is the lesser harm depends on the application rather
// than on Raft. What can be done, and is, is to make the band small and to
// make it visible: WithKnownCommitted narrows it to the entries the state
// machine had not yet applied, InspectStorage reports it before anything is
// written, and RecoveryReport records exactly which indices were promoted or
// discarded after the fact.
//
// Recovery is therefore an operator action for a cluster that is already dead,
// not a tool for one that is merely slow or partitioned. Do not run it against
// storage a node is still using -- filestore locks its directory so that this
// fails rather than corrupts -- and do not run it to sidestep an unavailable
// but intact cluster: if the lost nodes come back afterwards holding a history
// this one does not have, there is no longer a single agreed history.
//
// The procedure:
//
//  1. Stop every surviving node.
//  2. Call InspectStorage on each survivor's storage and pick the most recent,
//     with RecoveryInfo.MoreRecentThan. That is the one whose history is kept.
//     RecoveryInfo.UncommittedBand says which of its entries are in doubt;
//     look at them before going on, because this is the last moment at which
//     they can be told apart from the rest.
//  3. Call RecoverCluster on that node's storage, naming a membership it is a
//     voting majority of. Naming only itself is the usual choice. Pass
//     WithKnownCommitted if the state machine keeps its own durable state, and
//     keep the returned report.
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

	// KnownCommittedIndex is the highest index this node's storage proves was
	// committed. Entries above it, up to LastIndex, are the ones recovery has
	// to guess about: each was either committed by the majority that is gone,
	// or was still in flight when it died, and nothing left behind can say
	// which.
	//
	// It comes from the snapshot's last included index, because a snapshot is
	// taken at an applied index and applying follows committing, and from the
	// store's own record of how far the log had committed when it implements
	// CommitRecorder -- a much tighter bound, since it tracks the commit index
	// rather than compaction. A state machine holding its own durable state
	// knows better still; RecoverCluster takes that as WithKnownCommitted.
	KnownCommittedIndex Index

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

	// Two things on disk prove commitment. Whatever went into a snapshot was
	// applied, and nothing is applied before it commits. And a store that
	// implements CommitRecorder has been told, as the node ran, how far the
	// log had committed and reached this node's disk -- a much tighter bound,
	// since it moves with the commit index rather than with compaction.
	info.KnownCommittedIndex = info.SnapshotIndex
	if cr, ok := store.(CommitRecorder); ok {
		recorded, rerr := cr.LoadCommitIndex(ctx)
		if rerr != nil {
			return RecoveryInfo{}, fmt.Errorf("raft.InspectStorage: load commit index: %w", rerr)
		}
		// Clamped to the log: a recorded index above it would be a claim about
		// entries this node does not have, which is the one thing a lower
		// bound must never be.
		info.KnownCommittedIndex = min(max(info.KnownCommittedIndex, recorded), info.LastIndex)
	}
	return info, nil
}

// UncommittedBand returns the range of indices recovery cannot classify: every
// entry above what the node can prove committed, up to the end of its log.
// ok is false when there is none, which is the case for a node whose log ends
// at a snapshot boundary.
//
// Recovering this node either promotes that range to committed history or
// discards it. Which of those is the lesser harm depends on what the
// application does with a write it was told had failed, so the choice is the
// operator's; see RecoverCluster and DiscardUncommitted.
func (r *RecoveryInfo) UncommittedBand() (from, to Index, ok bool) {
	if r.LastIndex <= r.KnownCommittedIndex {
		return 0, 0, false
	}
	return r.KnownCommittedIndex + 1, r.LastIndex, true
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

// RecoverOption adjusts what RecoverCluster does with the part of the log it
// cannot prove was committed.
type RecoverOption func(*recoverOptions)

type recoverOptions struct {
	knownCommitted     Index
	discardUncommitted bool
}

// WithKnownCommitted raises the index recovery treats as proven committed.
//
// Storage alone proves only what went into a snapshot, which can be thousands
// of entries behind. A state machine that keeps its own durable state knows
// more: everything at or below its AppliedIndex has been applied, and nothing
// is applied before it commits. Pass that index here and the band recovery has
// to guess about shrinks to the handful of entries the apply loop had not
// reached.
//
// It must be a number the caller can actually prove, not an estimate. Claiming
// more than was committed makes DiscardUncommitted keep entries it should have
// discarded, and makes the report say entries were committed when they were
// not. Indices at or below what storage already proves are ignored rather than
// lowering the floor.
func WithKnownCommitted(index Index) RecoverOption {
	return func(o *recoverOptions) { o.knownCommitted = index }
}

// DiscardUncommitted truncates the log to the last index proven committed
// instead of keeping it whole.
//
// It picks the other side of the trade recovery cannot avoid. By default the
// node's whole log becomes the recovered cluster's history, which means an
// entry that was still in flight when the majority died is now committed: a
// client told its write failed finds that it succeeded. With this option
// nothing uncommitted is ever promoted -- and entries that had committed, but
// that this node had not yet applied or snapshotted, are thrown away instead:
// a client told its write succeeded finds that it did not.
//
// Neither is safe. Which is the lesser harm is a property of the application,
// not of Raft: a system that compensates for failed writes is damaged by the
// first, and one that acknowledges durably is damaged by the second. Use
// WithKnownCommitted to make the band small before choosing either.
//
// Recovery refuses to discard when nothing at all is proven -- no snapshot and
// no WithKnownCommitted -- since that would throw the entire log away on the
// strength of an option.
func DiscardUncommitted() RecoverOption {
	return func(o *recoverOptions) { o.discardUncommitted = true }
}

// RecoveryReport records what a recovery did, so that the one operation in this
// package that can lose or invent committed data leaves an account of which it
// did and to what.
//
// Log it. A cluster that comes back after a recovery looks like any other
// cluster, and six months later the only way to explain a write that vanished
// or one that reappeared is a record made at the time.
type RecoveryReport struct {
	// Term is the term the recovery entry was written in, and Index is where
	// it went. Together they identify the entry in the recovered log.
	Term  Term
	Index Index

	// KnownCommittedIndex is the floor recovery worked from: the highest index
	// it could prove was committed, from the snapshot and from
	// WithKnownCommitted.
	KnownCommittedIndex Index

	// PromotedFrom and PromotedTo bound the entries that were not proven
	// committed and are now part of the recovered cluster's history. Both are
	// zero when there were none, or when DiscardUncommitted was used.
	//
	// These are the writes that may have been reported to a client as failed.
	PromotedFrom, PromotedTo Index

	// DiscardedFrom and DiscardedTo bound the entries DiscardUncommitted threw
	// away. Both are zero unless it was used.
	//
	// These are the writes that may have been reported to a client as
	// succeeded.
	DiscardedFrom, DiscardedTo Index

	// MembersBefore is the membership the node would have restarted with, and
	// MembersAfter the one it will restart with now.
	MembersBefore, MembersAfter []PeerConfig
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
// By default nothing about the node's log or state machine changes: everything
// it had, it keeps, and the recovery entry is appended at the end in a term
// higher than any the node has seen, so that it is the last word on the
// cluster's membership however the log ended. The cost is that entries the
// node held but that were never committed become committed. DiscardUncommitted
// takes the other side of that trade, and WithKnownCommitted shrinks how much
// either one is deciding about.
//
// The returned report says what it did, including exactly which indices were
// promoted or discarded. Log it: it is the only record of the one operation
// here that can lose or invent committed data.
//
// The node must be stopped and this must be the only open handle on its
// storage. A Storage that can tell refuses to help you break that: filestore
// takes an exclusive lock on its directory, so recovery run against a node
// that is still up fails at the open rather than racing its writes. A Storage
// that cannot tell offers no such protection.
func RecoverCluster(ctx context.Context, store Storage, self NodeID, members []PeerConfig, opts ...RecoverOption) (RecoveryReport, error) {
	var o recoverOptions
	for _, opt := range opts {
		opt(&o)
	}

	if err := validateRecoveryMembers(self, members); err != nil {
		return RecoveryReport{}, err
	}

	info, err := InspectStorage(ctx, store)
	if err != nil {
		return RecoveryReport{}, err
	}

	// Evidence from outside storage can only raise the floor. A caller who
	// passes something lower than the snapshot has told us nothing new.
	floor := max(info.KnownCommittedIndex, o.knownCommitted)
	if floor > info.LastIndex {
		// A state machine cannot have applied what the log does not hold. This
		// is a caller mistake or a mismatched pair of directories, and going
		// ahead would truncate nothing while reporting a floor that is not in
		// the log.
		return RecoveryReport{}, fmt.Errorf(
			"raft.RecoverCluster: WithKnownCommitted(%d) is past the last index in the log (%d); "+
				"the state machine and the log do not belong to the same node",
			o.knownCommitted, info.LastIndex)
	}

	report := RecoveryReport{
		KnownCommittedIndex: floor,
		MembersBefore:       info.Members,
		MembersAfter:        members,
	}

	// Where the recovery entry goes, and what happens to whatever is above the
	// floor, is the whole of the choice this function offers.
	tail := info.LastIndex
	if o.discardUncommitted {
		if floor == 0 && info.LastIndex > 0 {
			return RecoveryReport{}, fmt.Errorf(
				"raft.RecoverCluster: DiscardUncommitted would discard the entire log (%d entries): "+
					"this node has no snapshot, so nothing in it is proven committed. Supply "+
					"WithKnownCommitted if you can prove more, or recover without the option",
				info.LastIndex)
		}
		if info.LastIndex > floor {
			report.DiscardedFrom, report.DiscardedTo = floor+1, info.LastIndex
		}
		tail = floor
	} else if info.LastIndex > floor {
		report.PromotedFrom, report.PromotedTo = floor+1, info.LastIndex
	}

	// A term above everything the node has seen, so that the recovery entry
	// wins every comparison against what the log already holds, and so that
	// the restarted node can win an election against any stale peer that is
	// brought back by mistake. The log's own last term is considered as well
	// as the hard state's: storage that disagrees with itself is exactly the
	// kind of state recovery exists to clean up.
	term := max(info.Term, info.LastTerm) + 1
	report.Term, report.Index = term, tail+1

	// Hard state first. A crash between the two writes must not leave a log
	// holding an entry from a term the node does not believe it reached; the
	// other order -- a term with no entry to go with it -- costs nothing but a
	// repeat of the recovery.
	if err := store.SaveHardState(ctx, HardState{CurrentTerm: term}); err != nil {
		return RecoveryReport{}, fmt.Errorf("raft.RecoverCluster: save hard state: %w", err)
	}

	// Then the truncation, if one was asked for. Before the append rather than
	// after, so that a crash in between leaves a log that is short rather than
	// one that has had its tail removed from under the entry that was supposed
	// to end it.
	if o.discardUncommitted && info.LastIndex > floor {
		if err := store.TruncateSuffix(ctx, floor+1); err != nil {
			return RecoveryReport{}, fmt.Errorf("raft.RecoverCluster: discard uncommitted suffix: %w", err)
		}
	}

	entry := LogEntry{
		Index:   tail + 1,
		Term:    term,
		Command: encodeFinaliseConfigEntry(members),
	}
	if err := store.AppendLogEntries(ctx, []LogEntry{entry}); err != nil {
		return RecoveryReport{}, fmt.Errorf("raft.RecoverCluster: append membership entry: %w", err)
	}
	return report, nil
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
