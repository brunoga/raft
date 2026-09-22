package raft_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
)

// waitCommitQuorum ticks until every node reports q as the commit quorum in
// effect.
func waitCommitQuorum(t *testing.T, c *Cluster, q int) {
	t.Helper()
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		all := true
		for _, n := range c.nodes {
			if n.CommitQuorum() != q {
				all = false
			}
		}
		if all {
			return
		}
	}
	for i, n := range c.nodes {
		t.Logf("%s: commit quorum %d", c.ids[i], n.CommitQuorum())
	}
	t.Fatalf("commit quorum did not reach %d everywhere", q)
}

// disconnectAllBut isolates every node not in keep.
func disconnectAllBut(c *Cluster, keep ...int) {
	for i := range c.nodes {
		kept := false
		for _, k := range keep {
			if k == i {
				kept = true
			}
		}
		if !kept {
			c.Disconnect(i)
		}
	}
}

// reconnectAll restores every node.
func reconnectAll(c *Cluster) {
	for i := range c.nodes {
		c.Reconnect(i)
	}
}

// TestFlexibleQuorum_CommitOnFour sets a five-voter group to commit on four:
// three voters, a majority, are no longer enough to commit, and elections are
// unchanged because they never go below a majority.
func TestFlexibleQuorum_CommitOnFour(t *testing.T) {
	c := newCluster(t, 5)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	if err := leader.SetCommitQuorum(ctx, 4); err != nil {
		t.Fatalf("SetCommitQuorum(4): %v", err)
	}
	waitCommitQuorum(t, c, 4)

	// Cut two followers off. Three voters remain, which is a majority but
	// not a commit quorum of four, so a write must not be acknowledged.
	cut := []int{}
	for i := range c.nodes {
		if i != leaderIdx && len(cut) < 2 {
			cut = append(cut, i)
		}
	}
	for _, i := range cut {
		c.Disconnect(i)
	}
	if _, err := c.Propose(500*time.Millisecond, []byte("k=stalls")); err == nil {
		t.Fatal("a write was acknowledged by three of five voters under a commit quorum of four")
	}
	reconnectAll(c)
	if _, err := c.Propose(electionTimeout, []byte("k=commits")); err != nil {
		t.Fatalf("write after healing: %v", err)
	}

	// Elections still need a majority: two nodes on their own get no leader,
	// three do.
	disconnectAllBut(c, cut[0], cut[1])
	c.TickN(200)
	for _, k := range cut {
		if c.nodes[k].State() == raft.Leader {
			t.Fatal("two of five voters elected a leader; election quorums must stay majorities")
		}
	}
	reconnectAll(c)
	if c.Leader(electionTimeout) == nil {
		t.Fatal("no leader after healing")
	}
}

// TestFlexibleQuorum_CommitOnTwo is the other direction: a five-voter group
// that commits on two keeps taking writes with three voters cut off, and
// those three cannot elect a leader among themselves because elections now
// need four.
func TestFlexibleQuorum_CommitOnTwo(t *testing.T) {
	c := newCluster(t, 5)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]

	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	if err := leader.SetCommitQuorum(ctx, 2); err != nil {
		t.Fatalf("SetCommitQuorum(2): %v", err)
	}
	waitCommitQuorum(t, c, 2)

	partner := (leaderIdx + 1) % 5
	disconnectAllBut(c, leaderIdx, partner)

	// The leader and one follower are a commit quorum.
	if _, err := c.Propose(electionTimeout, []byte("k=two")); err != nil {
		t.Fatalf("a write did not commit on two of five under a commit quorum of two: %v", err)
	}
	if got := c.nodes[partner].CommitIndex(); got == 0 {
		t.Fatal("the partner did not learn the commit")
	}

	// The three cut off are a majority, but an election needs four.
	c.TickN(300)
	for i := range c.nodes {
		if i != leaderIdx && i != partner && c.nodes[i].State() == raft.Leader {
			t.Fatalf("%s led with three of five voters under an election quorum of four", c.ids[i])
		}
	}
	reconnectAll(c)
	if _, err := c.Propose(electionTimeout, []byte("k=healed")); err != nil {
		t.Fatalf("write after healing: %v", err)
	}
}

// TestFlexibleQuorum_BackToMajority checks that 0 restores a majority and
// that the value is reported everywhere.
func TestFlexibleQuorum_BackToMajority(t *testing.T) {
	c := newCluster(t, 3)
	leader := c.nodes[c.WaitLeader(electionTimeout)]

	// Ticked throughout, because a configuration change has to be replicated
	// to commit and a cluster nobody ticks replicates only what a proposal
	// happens to carry with it. The reconnected follower in particular is
	// caught up by a heartbeat, which is a thing that happens on a tick.
	stopTicking := tickWhile(c.nodes...)
	defer stopTicking()

	// A context per call rather than one for the test: a single budget spent
	// across an election, a write that is meant to time out, and a wait for
	// the cluster to agree is a budget that runs out on a loaded machine,
	// and the failure then lands on whichever call was last.
	setQuorum := func(node *raft.Node, quorum int) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
		defer cancel()
		return node.SetCommitQuorum(ctx, quorum)
	}

	if err := setQuorum(leader, 3); err != nil {
		t.Fatalf("SetCommitQuorum(3): %v", err)
	}
	waitCommitQuorum(t, c, 3)
	c.Disconnect((c.LeaderIndex() + 1) % 3)
	if _, err := c.Propose(300*time.Millisecond, []byte("k=stalls")); err == nil {
		t.Fatal("a write committed on two of three under a commit quorum of three")
	}
	c.Reconnect((c.LeaderIndex() + 1) % 3)

	// Under a commit quorum of three, one follower out of contact is also
	// one short of the set check-quorum needs, so the leader may have
	// stepped down during the cut. Find whoever leads now.
	leader = c.Leader(electionTimeout)
	if leader == nil {
		t.Fatal("no leader after healing")
	}
	if err := setQuorum(leader, 0); err != nil {
		t.Fatalf("SetCommitQuorum(0): %v", err)
	}
	waitCommitQuorum(t, c, 0)
	c.Disconnect((c.LeaderIndex() + 1) % 3)
	if _, err := c.Propose(electionTimeout, []byte("k=majority")); err != nil {
		t.Fatalf("a write did not commit on a majority after restoring it: %v", err)
	}
	c.Reconnect((c.LeaderIndex() + 1) % 3)
}

// TestFlexibleQuorum_Refusals pins the argument checks.
func TestFlexibleQuorum_Refusals(t *testing.T) {
	c := newCluster(t, 3)
	leaderIdx := c.WaitLeader(electionTimeout)
	leader := c.nodes[leaderIdx]
	ctx := context.Background()
	if err := leader.SetCommitQuorum(ctx, -1); err == nil {
		t.Error("SetCommitQuorum(-1) succeeded")
	}
	if err := leader.SetCommitQuorum(ctx, 4); err == nil {
		t.Error("SetCommitQuorum(4) on three voters succeeded")
	}
	if err := c.nodes[(leaderIdx+1)%3].SetCommitQuorum(ctx, 2); !errors.Is(err, raft.ErrNotLeader) {
		t.Errorf("SetCommitQuorum on a follower returned %v, want ErrNotLeader", err)
	}
}

// TestFlexibleQuorum_ConfigIsOnlyABootstrapValue checks that a node's
// Config.CommitQuorum never applies locally: the group counts by a majority
// until the first leader writes the policy into the log, and then every
// node counts by that, including one whose Config said otherwise.
func TestFlexibleQuorum_ConfigIsOnlyABootstrapValue(t *testing.T) {
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		if cfg.ID == "n1" {
			cfg.CommitQuorum = 3
		}
	})
	leaderIdx := c.WaitLeader(electionTimeout)
	for i, n := range c.nodes {
		if n.CommitQuorum() != 0 {
			t.Fatalf("%s counts by %d before any agreement", c.ids[i], n.CommitQuorum())
		}
	}
	if _, err := c.Propose(electionTimeout, []byte("k=v")); err != nil {
		t.Fatal(err)
	}
	// Whether the policy is now 3 or still a majority depends on who led,
	// and either way every node agrees.
	want := 0
	if c.ids[leaderIdx] == "n1" {
		want = 3
	}
	waitCommitQuorum(t, c, want)
}
