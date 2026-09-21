package raft

import (
	"context"
	"errors"
	"fmt"
)

// ---- Flexible quorums -------------------------------------------------------
//
// Raft commits an entry on a majority and elects a leader on a majority. The
// Leader Completeness argument only ever uses one fact about those two sets:
// they intersect, so a new leader has every committed entry. Any pair of
// sizes with that property serves it (Howard, Malkhi and Spiegelman,
// "Flexible Paxos"), so a group may trade one against the other. With N
// voters and a commit quorum of Q, an election quorum of N - Q + 1 is enough
// for that: a group that writes to two of five needs four to agree on a
// leader.
//
// Raft needs one more thing of election quorums that Paxos does not: two of
// them must intersect each other, so that a term has at most one leader.
// Paxos gives each proposer its own ballot numbers; Raft shares terms, and
// its log matching property -- an index and a term name one entry -- rests on
// one leader per term. So the election quorum is never below a majority.
// Below-majority election quorums are the one trade this cannot make: a
// group that writes to every replica still elects on a majority, and what
// it buys is durability (every acknowledged write is on every disk), not
// cheaper elections.
//
// The commit quorum is group state, because a node that counted differently
// could elect a leader without the entries another node had committed. It is
// changed by a config entry, and while that entry is in a node's log but not
// yet applied there, the node requires the stricter of the old and new sizes
// for every decision. Stricter is always safe -- a larger quorum intersects
// everything a smaller one did -- and it is what makes the change itself
// safe: the entry commits under the larger of the two commit quorums, so any
// node the new election quorum can be drawn from has seen it.
//
// Every quorum decision in the node goes through the two helpers here.

// quorumKind says which of the two quorums a decision needs.
type quorumKind uint8

const (
	// commitQuorum is the set an entry must reach to commit. Check-quorum
	// and read confirmations use it too: a leader that has heard from a
	// commit quorum has heard from a set that intersects every election
	// quorum, so no other leader can have been elected meanwhile.
	commitQuorum quorumKind = iota
	// electionQuorum is the set of votes a candidate needs.
	electionQuorum
)

// commitQuorumFor returns how many of total voters an entry must reach under
// policy q, which is a majority when q is 0 and never more than total.
func commitQuorumFor(q, total int) int {
	if total == 0 {
		return 0
	}
	if q <= 0 {
		return total/2 + 1
	}
	return min(q, total)
}

// electionQuorumFor returns how many of total voters a candidate needs under
// policy q: enough to intersect every commit quorum, and never fewer than a
// majority, so that two election quorums intersect each other too.
func electionQuorumFor(q, total int) int {
	if total == 0 {
		return 0
	}
	return max(total-commitQuorumFor(q, total)+1, total/2+1)
}

// quorumNeeded returns how many of total voters kind needs on this node right
// now: the stricter of the policy in effect and any policy the log holds that
// has not been applied yet. Event-loop only.
func (n *Node) quorumNeeded(kind quorumKind, total int) int {
	applied, latest := n.commitQuorumApplied, n.commitQuorumLatest
	switch kind {
	case electionQuorum:
		return max(electionQuorumFor(applied, total), electionQuorumFor(latest, total))
	default:
		return max(commitQuorumFor(applied, total), commitQuorumFor(latest, total))
	}
}

// CommitQuorum returns how many voters an entry must currently reach to
// commit, or 0 while the group uses a simple majority. Safe for concurrent
// use.
func (n *Node) CommitQuorum() int {
	return int(n.atomicCommitQuorum.Load())
}

// SetCommitQuorum sets how many voters an entry must reach to commit, for the
// whole group, and with it the election quorum, which becomes
// voters - quorum + 1 or a majority, whichever is larger. A quorum of 0
// restores a simple majority. It blocks until the change is committed and
// applied, or until ctx is cancelled, and returns ErrNotLeader on any node
// but the leader.
//
// This is the flexible quorum of Howard, Malkhi and Spiegelman, with the one
// restriction Raft's shared terms impose: election quorums stay majorities,
// so that a term has one leader. Within that there are two trades. A commit
// quorum below a majority makes a write need fewer acknowledgements than a
// leader needs votes -- two of five, say, against four -- for a group whose
// writes are frequent and whose elections are not. A commit quorum above a
// majority, up to the whole group, means no acknowledged write is ever on
// fewer than that many disks, for a group that would rather stall than
// acknowledge a write that a single failure could lose. Neither changes what
// is safe: an entry acknowledged by the commit quorum is on every future
// leader's log.
//
// What it changes is liveness, which is the point. A commit quorum of N
// stalls writes when any one voter is down; a commit quorum of two makes
// elections need four of five. Choose against the failures the deployment
// expects. The quorum is a count and follows the membership: if voters are
// later removed below it, it is treated as "all of them".
//
// The change is safe to make while the group is running. A node holding the
// entry before it commits requires the stricter of the old and new sizes for
// everything, so no decision is ever made under a pair of quorums that do not
// intersect. Like a membership change, only one is in flight at a time:
// ErrConfigChangeInProgress is returned while another configuration change is
// pending. It is refused when quorum exceeds the number of voters, which is
// the one value that could never be met.
func (n *Node) SetCommitQuorum(ctx context.Context, quorum int) error {
	if quorum < 0 {
		return errors.New("raft: SetCommitQuorum: quorum must not be negative (0 means a majority)")
	}
	voters := 0
	for _, m := range n.Members() {
		if m.Voter {
			voters++
		}
	}
	if quorum > voters {
		return fmt.Errorf("raft: SetCommitQuorum: quorum %d exceeds the %d voters", quorum, voters)
	}
	_, err := n.Propose(ctx, encodeCommitQuorumEntry(quorum))
	return err
}

// adoptCommitQuorum puts a group-agreed commit quorum into effect, in apply
// order. Event-loop only.
func (n *Node) adoptCommitQuorum(quorum int, source string) {
	if quorum != n.commitQuorumApplied {
		n.logger.Info("commit quorum changed", "quorum", quorum, "source", source)
	}
	n.commitQuorumApplied = quorum
	n.commitQuorumAgreed = true
	n.atomicCommitQuorum.Store(int64(quorum))
}

// noteAppendedConfig records what a run of entries just put in the log says
// about how the group counts, before any of them is applied. Event-loop only.
//
// Membership is adopted at apply time here (see docs/divergence.md), and that
// is safe for membership because a single-server change keeps every pair of
// majorities overlapping. A quorum policy has no such property: a node that
// held a relaxed policy in its log but went on counting by the old one could
// elect itself on the old election quorum without an entry the new commit
// quorum had already committed. So the policy is in force from the append,
// as the stricter of old and new, and relaxes only at apply. The client
// table bound is noted for the same reason a leader notes its own: so that a
// node which becomes leader does not append a second one.
func (n *Node) noteAppendedConfig(entries []LogEntry) {
	for i := range entries {
		cmd := entries[i].Command
		if !isConfigEntry(cmd) {
			continue
		}
		if quorum, ok := decodeCommitQuorumEntry(cmd); ok {
			n.commitQuorumLatest = quorum
			continue
		}
		if _, ok := decodeClientTableCapEntry(cmd); ok {
			n.clientTableCapReplicated = true
		}
	}
}
