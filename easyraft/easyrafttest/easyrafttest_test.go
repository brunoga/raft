package easyrafttest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/easyrafttest"
)

type User struct {
	Name string `json:"name"`
}

type Counter struct {
	Value uint64 `json:"value"`
}

// TestCluster_ReplicatesAWrite is the shape of a test a caller of this
// package writes: three nodes, one write, and every replica holding it.
func TestCluster_ReplicatesAWrite(t *testing.T) {
	c := easyrafttest.NewCluster(t, 3)
	users := easyrafttest.AddCollection[User](c, "users")
	ctx := c.Context()

	if err := users.Leader().Create(ctx, "alice", User{Name: "Alice"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.WaitApplied()

	for i, coll := range users.All() {
		got, err := coll.ReadStale("alice")
		if err != nil {
			t.Fatalf("node %d: ReadStale: %v", i, err)
		}
		if got.Name != "Alice" {
			t.Errorf("node %d holds %q", i, got.Name)
		}
	}
}

// TestCluster_MutationsRegisteredBeforeStart covers the two-step build, which
// exists because a mutation has to be on every replica before any entry can
// call it.
func TestCluster_MutationsRegisteredBeforeStart(t *testing.T) {
	c := easyrafttest.New(t, 3)
	counters := easyrafttest.AddCollection[Counter](c, "counters")
	counters.RegisterMutation("increment", func(cur *Counter, args []byte) (*Counter, []byte, error) {
		var delta uint64
		if len(args) > 0 {
			if err := json.Unmarshal(args, &delta); err != nil {
				return nil, nil, err
			}
		}
		cur.Value += delta
		return cur, nil, nil
	})
	c.Start()

	ctx := c.Context()
	if err := counters.Leader().Create(ctx, "hits", Counter{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	delta, err := json.Marshal(uint64(3))
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := counters.Leader().Mutate(ctx, "hits", "increment", delta); err != nil {
			t.Fatalf("Mutate: %v", err)
		}
	}
	c.WaitApplied()

	for i, coll := range counters.All() {
		got, err := coll.ReadStale("hits")
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
		if got.Value != 12 {
			t.Errorf("node %d holds %d, want 12", i, got.Value)
		}
	}
}

// TestCluster_MinorityCannotWrite is what an in-memory network buys: cutting
// one node off and checking it knows it cannot write, then healing it and
// checking it catches up.
func TestCluster_MinorityCannotWrite(t *testing.T) {
	c := easyrafttest.NewCluster(t, 3)
	users := easyrafttest.AddCollection[User](c, "users")
	ctx := c.Context()

	if err := users.Leader().Create(ctx, "before", User{Name: "Before"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.WaitApplied()

	// Cut off a follower, so the majority keeps its leader.
	isolated := (c.LeaderIndex() + 1) % 3
	c.Partition(isolated)

	if err := users.Leader().Create(ctx, "during", User{Name: "During"}); err != nil {
		t.Fatalf("the majority could not write while one node was cut off: %v", err)
	}

	// The isolated node cannot serve a linearizable read: it cannot reach a
	// quorum to confirm it is current.
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	_, err := users.Node(isolated).Read(readCtx, "during")
	cancel()
	if err == nil {
		t.Error("the isolated node served a linearizable read")
	}

	c.Heal(isolated)
	c.WaitApplied()
	got, err := users.Node(isolated).ReadStale("during")
	if err != nil {
		t.Fatalf("the healed node did not catch up: %v", err)
	}
	if got.Name != "During" {
		t.Errorf("the healed node holds %q", got.Name)
	}
}

// TestCluster_PartitioningTheLeaderElectsAnother checks that the harness can
// express the failure everything else is built to survive.
func TestCluster_PartitioningTheLeaderElectsAnother(t *testing.T) {
	c := easyrafttest.NewCluster(t, 3)
	users := easyrafttest.AddCollection[User](c, "users")
	ctx := c.Context()

	oldLeader := c.LeaderIndex()
	c.Partition(oldLeader)

	// The majority elects somebody else and takes writes.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if idx := c.LeaderIndex(); idx != -1 && idx != oldLeader {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if idx := c.LeaderIndex(); idx == -1 || idx == oldLeader {
		t.Fatalf("no new leader was elected; leader index is %d", idx)
	}
	if err := users.Leader().Create(ctx, "after", User{Name: "After"}); err != nil {
		t.Fatalf("the new leader could not write: %v", err)
	}

	c.Heal(oldLeader)
	c.WaitApplied()
	if _, err := users.Node(oldLeader).ReadStale("after"); err != nil {
		t.Errorf("the old leader did not catch up after healing: %v", err)
	}
}

// TestCluster_StopAndRestartKeepsState covers restarting a node from its own
// data directory, which is what a process restart looks like.
func TestCluster_StopAndRestartKeepsState(t *testing.T) {
	c := easyrafttest.NewCluster(t, 3)
	users := easyrafttest.AddCollection[User](c, "users")
	ctx := c.Context()

	if err := users.Leader().Create(ctx, "durable", User{Name: "Durable"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.WaitApplied()

	victim := (c.LeaderIndex() + 1) % 3
	c.StopNode(victim)

	// The remaining two still form a majority.
	if err := users.Leader().Create(ctx, "while-down", User{Name: "While"}); err != nil {
		t.Fatalf("the majority could not write with one node down: %v", err)
	}

	c.RestartNode(victim)
	users.Rebind(victim)
	c.WaitApplied()

	for _, key := range []string{"durable", "while-down"} {
		if _, err := users.Node(victim).ReadStale(key); err != nil {
			t.Errorf("the restarted node is missing %s: %v", key, err)
		}
	}
}

// TestCluster_SingleNode covers the smallest cluster, which is what most
// tests of a service's own logic want.
func TestCluster_SingleNode(t *testing.T) {
	c := easyrafttest.NewCluster(t, 1)
	users := easyrafttest.AddCollection[User](c, "users")
	ctx := c.Context()

	c.Ready()
	if err := users.Leader().Create(ctx, "solo", User{Name: "Solo"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := users.Leader().Read(ctx, "solo")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Name != "Solo" {
		t.Errorf("read back %q", got.Name)
	}
	if len(c.Followers()) != 0 {
		t.Errorf("a one-node cluster reported %d followers", len(c.Followers()))
	}
}

// TestCluster_OptionsReachTheNodes checks the two escape hatches: options for
// every node, and options for one.
func TestCluster_OptionsReachTheNodes(t *testing.T) {
	c := easyrafttest.NewCluster(t, 3, easyrafttest.Options{
		Store: []easyraft.Option{easyraft.WithMaxClientTableSize(7)},
		PerNode: func(i int) []easyraft.Option {
			if i == 2 {
				return []easyraft.Option{easyraft.WithLeaseReads()}
			}
			return nil
		},
	})
	for i, s := range c.Stores {
		if got := s.MaxClientTableSize(); got != 7 {
			t.Errorf("node %d reports a client table bound of %d, want the configured 7", i, got)
		}
	}

	// The per-node option reached exactly one node, which shows by that node
	// still serving reads -- a lease read falls back to a quorum read, so the
	// observable difference is that it works at all.
	users := easyrafttest.AddCollection[User](c, "users")
	ctx := c.Context()
	if err := users.Leader().Create(ctx, "k", User{Name: "v"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := users.Node(2).Read(ctx, "k"); err != nil {
		t.Errorf("the node configured with lease reads could not read: %v", err)
	}
}

// TestCluster_LeasesAndRevisionsWork spot-checks that the features added
// around the same time as this harness are reachable through it, since a
// harness that only supports the oldest half of the API is a trap.
func TestCluster_LeasesAndRevisionsWork(t *testing.T) {
	c := easyrafttest.NewCluster(t, 3, easyrafttest.Options{
		Store: []easyraft.Option{easyraft.WithKeyLeaseSweepInterval(50 * time.Millisecond)},
	})
	users := easyrafttest.AddCollection[User](c, "users")
	ctx := c.Context()
	leader := c.WaitLeader()

	// Compare-and-swap.
	if err := users.Leader().Create(ctx, "alice", User{Name: "Alice"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, rev, revErr := users.Leader().ReadRev(ctx, "alice")
	if revErr != nil {
		t.Fatalf("ReadRev: %v", revErr)
	}
	if err := users.Leader().UpdateIf(ctx, "alice", User{Name: "Alice2"}, rev); err != nil {
		t.Fatalf("UpdateIf: %v", err)
	}
	if err := users.Leader().UpdateIf(ctx, "alice", User{Name: "Alice3"}, rev); !errors.Is(err, easyraft.ErrRevisionMismatch) {
		t.Errorf("a stale conditional update returned %v", err)
	}

	// A lease, and the key going when nothing renews it.
	lease, leaseErr := leader.GrantLease(ctx, 200*time.Millisecond)
	if leaseErr != nil {
		t.Fatalf("GrantLease: %v", leaseErr)
	}
	if err := users.Leader().UpsertWithLease(ctx, "ephemeral", User{Name: "Gone soon"}, lease); err != nil {
		t.Fatalf("UpsertWithLease: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := users.Leader().ReadStale("ephemeral"); errors.Is(err, easyraft.ErrKeyNotFound) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the leased key outlived its lease")
}

// TestCluster_NodeIndexOutOfRangeSaysSo keeps the harness's own errors
// readable, since they are what a test author reads first.
func TestCluster_NodeIndexOutOfRangeSaysSo(t *testing.T) {
	c := easyrafttest.NewCluster(t, 1)
	if got := len(c.IDs); got != 1 {
		t.Fatalf("cluster has %d ids", got)
	}
	if c.IDs[0] != raft.NodeID("n1") {
		t.Errorf("first node is %q, want n1", c.IDs[0])
	}
}
