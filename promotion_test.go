package raft_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

type promoteSM struct{}

func (promoteSM) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }
func (promoteSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("s"))
	return err
}
func (promoteSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// promotionCluster is a leader plus a learner that the test can keep offline.
type promotionCluster struct {
	net     *memtransport.Network
	leader  *raft.Node
	learner *raft.Node
	tick    func(int)
}

func newPromotionCluster(t *testing.T) *promotionCluster {
	t.Helper()

	net := memtransport.NewNetwork()
	mk := func(id raft.NodeID, peers []raft.PeerConfig) *raft.Node {
		cfg := raft.DefaultConfig()
		cfg.ID = id
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = promoteSM{}
		cfg.Transport = net.NewTransport(id)
		cfg.TickInterval = 0
		cfg.SnapshotThreshold = 0
		tuneForManualTicks(&cfg)
		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(%s): %v", id, err)
		}
		net.Register(id, node.Handler())
		node.Start()
		return node
	}

	// The second node starts as a learner, so the first is the only voter and
	// wins on its own.
	leader := mk("n1", []raft.PeerConfig{{ID: "n2", Voter: false}})
	learner := mk("n2", []raft.PeerConfig{{ID: "n1", Voter: true}})

	c := &promotionCluster{net: net, leader: leader, learner: learner}
	c.tick = func(n int) {
		for range n {
			leader.Tick()
			learner.Tick()
			time.Sleep(time.Millisecond)
		}
	}
	t.Cleanup(func() {
		leader.Stop()
		learner.Stop()
	})

	deadline := time.Now().Add(3 * time.Second)
	for leader.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("no leader elected")
		}
		c.tick(1)
	}
	return c
}

// TestPromoteMember_RefusesAMemberThatIsBehind asserts that a learner too far
// behind is not made a voter.
//
// Adding a voter changes the quorum immediately. A member promoted while it is
// still catching up counts towards the larger quorum without being able to help
// satisfy it: a three-node cluster that promotes a fourth, empty member goes
// from tolerating one failure to tolerating none until that member catches up.
func TestPromoteMember_RefusesAMemberThatIsBehind(t *testing.T) {
	ctx := context.Background()
	c := newPromotionCluster(t)

	// Cut the learner off and build a backlog it has not seen.
	c.net.Drop("n1", "n2")
	c.net.Drop("n2", "n1")
	for i := range 50 {
		if _, err := c.leader.Propose(ctx, fmt.Appendf(nil, "entry-%d", i)); err != nil {
			t.Fatalf("propose: %v", err)
		}
	}
	c.tick(5)

	err := c.leader.PromoteMember(ctx, "n2", 5)
	if !errors.Is(err, raft.ErrMemberNotCaughtUp) {
		t.Fatalf("PromoteMember on a lagging member returned %v, want ErrMemberNotCaughtUp", err)
	}

	for _, m := range c.leader.Members() {
		if m.ID == "n2" && m.Voter {
			t.Error("member was promoted despite being refused")
		}
	}
}

// TestPromoteMember_PromotesAMemberThatHasCaughtUp is the companion: once the
// learner is up to date, promotion goes through and the new role reaches both
// nodes, because it travels through the log like any other agreed change.
func TestPromoteMember_PromotesAMemberThatHasCaughtUp(t *testing.T) {
	ctx := context.Background()
	c := newPromotionCluster(t)

	for i := range 20 {
		if _, err := c.leader.Propose(ctx, fmt.Appendf(nil, "entry-%d", i)); err != nil {
			t.Fatalf("propose: %v", err)
		}
	}
	// Let the learner catch up.
	deadline := time.Now().Add(3 * time.Second)
	for c.learner.LastApplied() < c.leader.LastApplied() {
		if time.Now().After(deadline) {
			t.Fatalf("learner never caught up: %d vs %d",
				c.learner.LastApplied(), c.leader.LastApplied())
		}
		c.tick(1)
	}

	if err := c.leader.PromoteMember(ctx, "n2", 5); err != nil {
		t.Fatalf("PromoteMember on a caught-up member: %v", err)
	}
	c.tick(20)

	assertVoter := func(node *raft.Node, who string) {
		t.Helper()
		for _, m := range node.Members() {
			if m.ID == "n2" {
				if !m.Voter {
					t.Errorf("%s still lists n2 as a non-voter after promotion", who)
				}
				return
			}
		}
		t.Errorf("%s does not list n2 at all", who)
	}
	assertVoter(c.leader, "leader")
	assertVoter(c.learner, "promoted member")
}

// TestPromoteMember_ReportsAnUnknownMember guards the case where the caller
// names a node that is not in the cluster at all, which would otherwise look
// like a successful no-op.
func TestPromoteMember_ReportsAnUnknownMember(t *testing.T) {
	c := newPromotionCluster(t)

	err := c.leader.PromoteMember(context.Background(), "nobody", 0)
	if !errors.Is(err, raft.ErrNotMember) {
		t.Errorf("PromoteMember for an unknown node returned %v, want ErrNotMember", err)
	}
}

// TestReplicationProgress_ReportsLagAndIsLeaderOnly asserts that the leader can
// report how far each peer has kept up, and that other nodes say so rather than
// inventing an answer. Only the leader tracks this.
func TestReplicationProgress_ReportsLagAndIsLeaderOnly(t *testing.T) {
	ctx := context.Background()
	c := newPromotionCluster(t)

	if _, err := c.learner.ReplicationProgress(ctx); !errors.Is(err, raft.ErrNotLeader) {
		t.Errorf("ReplicationProgress on a non-leader returned %v, want ErrNotLeader", err)
	}

	c.net.Drop("n1", "n2")
	c.net.Drop("n2", "n1")
	for i := range 30 {
		if _, err := c.leader.Propose(ctx, fmt.Appendf(nil, "entry-%d", i)); err != nil {
			t.Fatalf("propose: %v", err)
		}
	}
	c.tick(5)

	progress, err := c.leader.ReplicationProgress(ctx)
	if err != nil {
		t.Fatalf("ReplicationProgress: %v", err)
	}
	if len(progress) != 1 {
		t.Fatalf("progress reported for %d peers, want 1", len(progress))
	}
	p := progress[0]
	if p.ID != "n2" {
		t.Errorf("progress reported for %q, want n2", p.ID)
	}
	if p.Voter {
		t.Error("learner reported as a voter")
	}
	if p.Lag() == 0 {
		t.Errorf("peer cut off for 30 entries reported no lag: %+v", p)
	}
}

// TestAddServer_ChangesTheRoleOfAnExistingMember pins that re-adding an
// existing member with a different role actually changes it. Treating it as a
// no-op silently ignored every promotion and demotion.
func TestAddServer_ChangesTheRoleOfAnExistingMember(t *testing.T) {
	ctx := context.Background()
	c := newPromotionCluster(t)

	if err := c.leader.AddServer(ctx, raft.PeerConfig{ID: "n2", Voter: true}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	c.tick(10)

	for _, m := range c.leader.Members() {
		if m.ID == "n2" {
			if !m.Voter {
				t.Error("re-adding an existing member with a new role left the old role in place")
			}
			return
		}
	}
	t.Error("member n2 is missing after AddServer")
}
