package raft

import (
	"testing"
	"testing/synctest"
)

// TestQuorumSizes pins the arithmetic: a majority when unset, the given
// count otherwise, clamped to the voter count, and an election quorum that
// always intersects it.
func TestQuorumSizes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cases := []struct {
			q, total     int
			commit, elec int
		}{
			{0, 1, 1, 1},
			{0, 3, 2, 2},
			{0, 4, 3, 3},
			{0, 5, 3, 3},
			{1, 5, 1, 5},
			{2, 5, 2, 4},
			{3, 5, 3, 3},
			{4, 5, 4, 3}, // elections never go below a majority
			{5, 5, 5, 3},
			{7, 5, 5, 3}, // more than the voters: all of them
			{3, 0, 0, 0}, // no voters at all
		}
		for _, tc := range cases {
			if got := commitQuorumFor(tc.q, tc.total); got != tc.commit {
				t.Errorf("commitQuorumFor(%d, %d) = %d, want %d", tc.q, tc.total, got, tc.commit)
			}
			if got := electionQuorumFor(tc.q, tc.total); got != tc.elec {
				t.Errorf("electionQuorumFor(%d, %d) = %d, want %d", tc.q, tc.total, got, tc.elec)
			}
			if tc.total > 0 && commitQuorumFor(tc.q, tc.total)+electionQuorumFor(tc.q, tc.total) <= tc.total {
				t.Errorf("q=%d N=%d: quorums do not intersect", tc.q, tc.total)
			}
			if tc.total > 0 && 2*electionQuorumFor(tc.q, tc.total) <= tc.total {
				t.Errorf("q=%d N=%d: two election quorums could fail to intersect", tc.q, tc.total)
			}
		}
	})
}

// TestQuorumNeeded_StricterWhilePending pins that a policy in the log but
// not yet applied makes every decision need the stricter of the two sizes,
// in both directions.
func TestQuorumNeeded_StricterWhilePending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := &Node{}
		const N = 5

		// Relaxing the commit quorum (3 -> 2) tightens elections (3 -> 4):
		// commits stay at 3 until applied, elections need 4 at once.
		n.commitQuorumApplied, n.commitQuorumLatest = 0, 2
		if got := n.quorumNeeded(commitQuorum, N); got != 3 {
			t.Errorf("pending relaxation: commit quorum %d, want the old 3", got)
		}
		if got := n.quorumNeeded(electionQuorum, N); got != 4 {
			t.Errorf("pending relaxation: election quorum %d, want the new 4", got)
		}
		n.commitQuorumApplied = 2
		if got := n.quorumNeeded(commitQuorum, N); got != 2 {
			t.Errorf("applied relaxation: commit quorum %d, want 2", got)
		}

		// Tightening the commit quorum (3 -> 4): commits need 4 at once, and
		// elections stay at the majority floor throughout.
		n.commitQuorumApplied, n.commitQuorumLatest = 0, 4
		if got := n.quorumNeeded(commitQuorum, N); got != 4 {
			t.Errorf("pending tightening: commit quorum %d, want the new 4", got)
		}
		if got := n.quorumNeeded(electionQuorum, N); got != 3 {
			t.Errorf("pending tightening: election quorum %d, want 3", got)
		}
		n.commitQuorumApplied = 4
		if got := n.quorumNeeded(electionQuorum, N); got != 3 {
			t.Errorf("applied tightening: election quorum %d, want the majority floor 3", got)
		}
	})
}

// TestCommitQuorumEntry_RoundTrip pins the config entry encoding.
func TestCommitQuorumEntry_RoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, q := range []int{0, 1, 3, 1 << 30} {
			cmd := encodeCommitQuorumEntry(q)
			if !isConfigEntry(cmd) {
				t.Fatalf("quorum entry for %d is not a config entry", q)
			}
			got, ok := decodeCommitQuorumEntry(cmd)
			if !ok || got != q {
				t.Fatalf("decodeCommitQuorumEntry = (%d, %v), want (%d, true)", got, ok, q)
			}
		}
		if _, ok := decodeCommitQuorumEntry(encodeClientTableCapEntry(3)); ok {
			t.Fatal("a cap entry decoded as a quorum entry")
		}
	})
}

// TestMembershipEncoding_CarriesCommitQuorum pins that the quorum trails the
// lists, round-trips for both shapes, and that an encoding without it reads
// as a majority.
func TestMembershipEncoding_CarriesCommitQuorum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		simple := membershipState{members: []PeerConfig{{ID: "a", Voter: true}, {ID: "b", Voter: true}}, commitQuorum: 2}
		got, ok := decodeMembership(encodeMembership(&simple))
		if !ok || got.commitQuorum != 2 || len(got.members) != 2 || got.joint {
			t.Fatalf("simple round trip: %+v ok=%v", got, ok)
		}
		joint := membershipState{joint: true, old: simple.members, new: simple.members[:1], commitQuorum: 1}
		got, ok = decodeMembership(encodeMembership(&joint))
		if !ok || got.commitQuorum != 1 || !got.joint || len(got.old) != 2 || len(got.new) != 1 {
			t.Fatalf("joint round trip: %+v ok=%v", got, ok)
		}
		// As an older build wrote it: the lists and nothing after.
		legacy := appendPeerList([]byte{membershipKindSimple}, simple.members)
		got, ok = decodeMembership(legacy)
		if !ok || got.commitQuorum != 0 || len(got.members) != 2 {
			t.Fatalf("legacy decode: %+v ok=%v", got, ok)
		}
	})
}
