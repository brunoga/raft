package raft_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

// ---- mrCluster — multi-raft test helper ------------------------------------

// mrCluster manages numGroups independent Raft groups each with numPhysical
// replicas, arranged across numPhysical Manager instances (one per "machine").
// groups[g][p] is the Node for group g+1 on physical node p (0-indexed).
type mrCluster struct {
	t           testing.TB
	net         *memtransport.Network
	mgrs        []*raft.Manager
	groups      [][]*raft.Node  // [groupIdx][physicalIdx]
	sms         [][]*kvSM       // [groupIdx][physicalIdx]
	ids         [][]raft.NodeID // [groupIdx][physicalIdx]
	numGroups   int
	numPhysical int
}

func newMRCluster(t testing.TB, numGroups, numPhysical int) *mrCluster {
	t.Helper()
	net := memtransport.NewNetwork()
	mgrs := make([]*raft.Manager, numPhysical)
	for p := range numPhysical {
		mgrs[p] = raft.NewManager()
	}

	groups := make([][]*raft.Node, numGroups)
	sms := make([][]*kvSM, numGroups)
	ids := make([][]raft.NodeID, numGroups)

	for g := range numGroups {
		groupID := uint64(g + 1)
		groups[g] = make([]*raft.Node, numPhysical)
		sms[g] = make([]*kvSM, numPhysical)
		ids[g] = make([]raft.NodeID, numPhysical)

		allIDs := make([]raft.NodeID, numPhysical)
		for p := range numPhysical {
			allIDs[p] = raft.NodeID(fmt.Sprintf("g%d-p%d", groupID, p))
			ids[g][p] = allIDs[p]
		}

		for p := range numPhysical {
			id := allIDs[p]
			peers := make([]raft.NodeID, 0, numPhysical-1)
			for _, pid := range allIDs {
				if pid != id {
					peers = append(peers, pid)
				}
			}
			sm := &kvSM{data: make(map[string]string)}
			cfg := raft.DefaultConfig()
			cfg.GroupID = groupID
			cfg.ID = id
			cfg.Peers = peers
			cfg.Storage = memstore.New()
			cfg.StateMachine = sm
			cfg.Transport = net.NewTransport(id)
			cfg.TickInterval = 0
			node, err := raft.New(&cfg)
			if err != nil {
				t.Fatalf("raft.New g=%d p=%d: %v", g+1, p, err)
			}
			net.Register(id, node)
			if err := mgrs[p].Add(groupID, node); err != nil {
				t.Fatalf("mgrs[%d].Add(g=%d): %v", p, g+1, err)
			}
			groups[g][p] = node
			sms[g][p] = sm
		}
	}

	for _, m := range mgrs {
		m.StartAll()
	}
	t.Cleanup(func() {
		for _, m := range mgrs {
			m.StopAll()
		}
	})

	return &mrCluster{
		t: t, net: net, mgrs: mgrs,
		groups: groups, sms: sms, ids: ids,
		numGroups: numGroups, numPhysical: numPhysical,
	}
}

// Tick delivers one tick to every node across all groups and physical nodes.
func (c *mrCluster) Tick() {
	for _, g := range c.groups {
		for _, n := range g {
			n.Tick()
		}
	}
}

// TickN delivers n ticks with 1 ms sleep between each.
func (c *mrCluster) TickN(n int) {
	for range n {
		c.Tick()
		time.Sleep(time.Millisecond)
	}
}

// leaderOf returns the physical index of the current leader of group g (0-based),
// or -1 if none.
func (c *mrCluster) leaderOf(g int) int {
	for p, n := range c.groups[g] {
		if n.StateSnapshot() == raft.Leader {
			return p
		}
	}
	return -1
}

// WaitGroupLeader ticks until group g (0-based) has a leader, returning its
// physical index. Fails the test after timeout.
func (c *mrCluster) WaitGroupLeader(g int, timeout time.Duration) int {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		if p := c.leaderOf(g); p >= 0 {
			return p
		}
	}
	c.t.Fatalf("group %d: no leader within %s", g+1, timeout)
	return -1
}

// WaitAllLeaders ticks until every group has a leader.
func (c *mrCluster) WaitAllLeaders(timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		allFound := true
		for g := range c.numGroups {
			if c.leaderOf(g) < 0 {
				allFound = false
				break
			}
		}
		if allFound {
			return
		}
	}
	for g := range c.numGroups {
		if c.leaderOf(g) < 0 {
			c.t.Errorf("group %d: no leader within %s", g+1, timeout)
		}
	}
}

// ProposeToGroup proposes cmd to group g's current leader, ticking in the
// background. Retries transparently on NotLeaderError (leadership can change
// during chaos). Returns an error only if the overall timeout expires.
func (c *mrCluster) ProposeToGroup(g int, timeout time.Duration, cmd []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		// Find the current leader; keep ticking if none yet.
		p := c.leaderOf(g)
		if p < 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				c.Tick()
			}
			continue
		}

		leader := c.groups[g][p]
		resultCh := make(chan error, 1)
		propCtx, propCancel := context.WithCancel(ctx)
		go func() {
			_, err := leader.Propose(propCtx, cmd)
			resultCh <- err
		}()

		var propErr error
		waiting := true
		for waiting {
			select {
			case propErr = <-resultCh:
				waiting = false
			case <-ticker.C:
				c.Tick()
			case <-ctx.Done():
				propCancel()
				return ctx.Err()
			}
		}
		propCancel()

		if propErr == nil {
			return nil
		}
		var nle *raft.NotLeaderError
		if errors.As(propErr, &nle) {
			// Leadership changed; retry with whoever is leader now.
			continue
		}
		return propErr
	}
}

// DisconnectPhysical partitions all nodes on physical node p from the network.
func (c *mrCluster) DisconnectPhysical(p int) {
	for g := range c.numGroups {
		c.net.Partition(c.ids[g][p])
	}
}

// ReconnectPhysical heals all partitions for physical node p.
func (c *mrCluster) ReconnectPhysical(p int) {
	for g := range c.numGroups {
		c.net.Heal(c.ids[g][p])
	}
}

// DisconnectNode partitions the single node at (group g, physical p).
func (c *mrCluster) DisconnectNode(g, p int) { c.net.Partition(c.ids[g][p]) }

// ReconnectNode heals the single node at (group g, physical p).
func (c *mrCluster) ReconnectNode(g, p int) { c.net.Heal(c.ids[g][p]) }

// TestMultiRaft_ThreeGroups_ThreeNodes models a three-physical-node cluster
// running three independent Raft groups. Each physical node hosts one node per
// group, managed by its own Manager — the realistic multi-Raft topology.
//
// The test verifies:
//  1. Every group independently elects exactly one leader.
//  2. A Propose on each group's leader is applied successfully.
//  3. Removing one group from all managers does not affect leadership in the
//     remaining groups.
func TestMultiRaft_ThreeGroups_ThreeNodes(t *testing.T) {
	const (
		numGroups    = 3
		numPhysical  = 3 // one Manager per physical node
		electionTime = 10 * time.Second
	)

	net := memtransport.NewNetwork()

	// One Manager per physical node.
	mgrs := make([]*raft.Manager, numPhysical)
	for i := range numPhysical {
		mgrs[i] = raft.NewManager()
	}

	// groups[g][p] = the Node for group g+1 on physical node p.
	nodes := make([][]*raft.Node, numGroups)
	for g := range numGroups {
		nodes[g] = make([]*raft.Node, numPhysical)
	}

	for g := range numGroups {
		groupID := uint64(g + 1)
		// All node IDs in this group: "g{groupID}-n{1..numPhysical}"
		allIDs := make([]raft.NodeID, numPhysical)
		for p := range numPhysical {
			allIDs[p] = raft.NodeID(fmt.Sprintf("g%d-n%d", groupID, p+1))
		}

		for p := range numPhysical {
			id := allIDs[p]
			peers := make([]raft.NodeID, 0, numPhysical-1)
			for _, pid := range allIDs {
				if pid != id {
					peers = append(peers, pid)
				}
			}
			cfg := raft.DefaultConfig()
			cfg.GroupID = groupID
			cfg.ID = id
			cfg.Peers = peers
			cfg.Storage = memstore.New()
			cfg.StateMachine = &discardSM{}
			cfg.Transport = net.NewTransport(id)
			cfg.TickInterval = 0
			node, err := raft.New(&cfg)
			if err != nil {
				t.Fatalf("raft.New group=%d physical=%d: %v", groupID, p, err)
			}
			net.Register(id, node)
			if err := mgrs[p].Add(groupID, node); err != nil {
				t.Fatalf("mgrs[%d].Add(group=%d): %v", p, groupID, err)
			}
			nodes[g][p] = node
		}
	}

	for _, m := range mgrs {
		m.StartAll()
	}
	t.Cleanup(func() {
		for _, m := range mgrs {
			m.StopAll()
		}
	})

	// Flatten all nodes for tick driving.
	all := make([]*raft.Node, 0, numGroups*numPhysical)
	for _, g := range nodes {
		all = append(all, g...)
	}

	// Tick until every group has a leader.
	leaders := make([]*raft.Node, numGroups) // leaders[g] = leader node for group g+1
	deadline := time.Now().Add(electionTime)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		<-ticker.C
		for _, n := range all {
			n.Tick()
		}
		for g, groupNodes := range nodes {
			if leaders[g] != nil {
				continue
			}
			for _, n := range groupNodes {
				if n.StateSnapshot() == raft.Leader {
					leaders[g] = n
					break
				}
			}
		}
		allFound := true
		for _, l := range leaders {
			if l == nil {
				allFound = false
				break
			}
		}
		if allFound {
			break
		}
	}
	for g, l := range leaders {
		if l == nil {
			t.Fatalf("group %d never elected a leader within %s", g+1, electionTime)
		}
		t.Logf("group %d leader: %s", g+1, l.ID())
	}

	// Propose one entry per group.
	for g, leader := range leaders {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		stop := make(chan struct{})
		go func() {
			tk := time.NewTicker(time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
					for _, n := range all {
						n.Tick()
					}
				}
			}
		}()

		_, err := leader.Propose(ctx, []byte(fmt.Sprintf("hello from group %d", g+1)))
		close(stop)
		cancel()
		if err != nil {
			t.Errorf("group %d Propose: %v", g+1, err)
		}
	}

	// Stop all nodes of group 1 by removing it from every manager.
	for p, m := range mgrs {
		if err := m.Remove(1); err != nil {
			t.Fatalf("mgrs[%d].Remove(1): %v", p, err)
		}
	}

	// Groups 2 and 3 must still have a leader (or re-elect quickly).
	// Drive a few more ticks to ensure stability.
	for range 200 {
		for _, n := range nodes[1] { // group 2
			n.Tick()
		}
		for _, n := range nodes[2] { // group 3
			n.Tick()
		}
		time.Sleep(time.Millisecond)
	}

	for g := 1; g < numGroups; g++ { // groups 2, 3 (index 1, 2)
		hasLeader := false
		for _, n := range nodes[g] {
			if n.StateSnapshot() == raft.Leader {
				hasLeader = true
				break
			}
		}
		if !hasLeader {
			t.Errorf("group %d lost its leader after group 1 was stopped", g+1)
		}
	}
}

// ---- TestChaos_MultiRaft_LeaderCrashes -------------------------------------

// TestChaos_MultiRaft_LeaderCrashes runs 10 independent Raft groups under
// repeated per-group leader crashes. After each crash the affected group must
// re-elect a leader and accept a new proposal, while every other group
// continues to make progress uninterrupted — verifying full group isolation.
func TestChaos_MultiRaft_LeaderCrashes(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test skipped in short mode")
	}

	const (
		numGroups   = 10
		numPhysical = 3
		rounds      = 10
	)

	c := newMRCluster(t, numGroups, numPhysical)
	c.WaitAllLeaders(electionTimeout)

	rng := rand.New(rand.NewPCG(42, 0))

	for round := range rounds {
		// Pick a random group and crash its leader.
		g := rng.IntN(numGroups)
		p := c.leaderOf(g)
		if p < 0 {
			p = c.WaitGroupLeader(g, electionTimeout)
		}
		c.DisconnectNode(g, p)

		// The group must re-elect from the remaining two replicas.
		deadline := time.Now().Add(electionTimeout)
		newLeader := -1
		for time.Now().Before(deadline) {
			c.Tick()
			time.Sleep(time.Millisecond)
			for np := range numPhysical {
				if np != p && c.groups[g][np].StateSnapshot() == raft.Leader {
					newLeader = np
					break
				}
			}
			if newLeader >= 0 {
				break
			}
		}
		if newLeader < 0 {
			t.Fatalf("round %d: group %d failed to re-elect after crashing physical %d", round, g+1, p)
		}

		// The re-elected leader must accept a proposal.
		cmd := fmt.Appendf(nil, "round=%d,group=%d", round, g+1)
		if err := c.ProposeToGroup(g, electionTimeout, cmd); err != nil {
			t.Fatalf("round %d: group %d propose after re-election: %v", round, g+1, err)
		}

		// All other groups must still have a leader.
		for og := range numGroups {
			if og == g {
				continue
			}
			if c.leaderOf(og) < 0 {
				t.Errorf("round %d: unaffected group %d lost its leader", round, og+1)
			}
		}

		// Heal before the next round.
		c.ReconnectNode(g, p)
		c.TickN(20)
	}

	// Final: every group must still accept proposals.
	for g := range numGroups {
		cmd := fmt.Appendf(nil, "final=%d", g+1)
		if err := c.ProposeToGroup(g, electionTimeout, cmd); err != nil {
			t.Errorf("final propose group %d: %v", g+1, err)
		}
	}
}

// ---- TestChaos_MultiRaft_PhysicalNodePartition -----------------------------

// TestChaos_MultiRaft_PhysicalNodePartition partitions an entire physical node
// (removing it from every group's quorum simultaneously) and verifies that all
// 10 groups re-elect leaders from the remaining two physical nodes. After
// healing the partitioned node must catch up without losing any committed
// entries.
func TestChaos_MultiRaft_PhysicalNodePartition(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test skipped in short mode")
	}

	const (
		numGroups   = 10
		numPhysical = 3
	)

	c := newMRCluster(t, numGroups, numPhysical)
	c.WaitAllLeaders(electionTimeout)

	// Propose a baseline entry in every group before the partition.
	for g := range numGroups {
		cmd := fmt.Appendf(nil, "pre=%d", g+1)
		if err := c.ProposeToGroup(g, electionTimeout, cmd); err != nil {
			t.Fatalf("pre-partition group %d: %v", g+1, err)
		}
	}

	// Partition physical node 0 — it is removed from all 10 groups at once.
	const victim = 0
	c.DisconnectPhysical(victim)

	// All 10 groups must elect a new leader from physical nodes 1 and 2.
	for g := range numGroups {
		deadline := time.Now().Add(electionTimeout)
		found := false
		for time.Now().Before(deadline) {
			c.Tick()
			time.Sleep(time.Millisecond)
			for p := range numPhysical {
				if p != victim && c.groups[g][p].StateSnapshot() == raft.Leader {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			t.Errorf("group %d: no leader on non-victim nodes after physical partition", g+1)
		}
	}

	// With quorum intact, all groups must continue accepting proposals.
	for g := range numGroups {
		cmd := fmt.Appendf(nil, "during=%d", g+1)
		if err := c.ProposeToGroup(g, electionTimeout, cmd); err != nil {
			t.Errorf("during-partition group %d: %v", g+1, err)
		}
	}

	// Heal the partition and tick until every replica of every group has
	// applied the "during" entry. The returning physical node may need to
	// install entries or snapshots for all 10 groups, so we wait actively
	// rather than using a fixed TickN.
	c.ReconnectPhysical(victim)

	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		allConverged := true
		for g := range numGroups {
			want := fmt.Sprintf("%d", g+1)
			for p := range numPhysical {
				if c.sms[g][p].Get("during") != want {
					allConverged = false
					break
				}
			}
			if !allConverged {
				break
			}
		}
		if allConverged {
			return
		}
	}

	// Report which replicas are still behind.
	for g := range numGroups {
		want := fmt.Sprintf("%d", g+1)
		for p := range numPhysical {
			if got := c.sms[g][p].Get("during"); got != want {
				t.Errorf("group %d physical %d: during=%q, want %q after convergence wait", g+1, p, got, want)
			}
		}
	}
}

// ---- TestChaos_MultiRaft_MessageDrops ----------------------------------------

// TestChaos_MultiRaft_MessageDrops simulates a flaky network by randomly
// partitioning and healing individual (group, physical) nodes across 10 Raft
// groups. The cluster must remain available (proposals succeed) and all replicas
// must converge on each group's last written value after chaos ends.
func TestChaos_MultiRaft_MessageDrops(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test skipped in short mode")
	}

	const (
		numGroups   = 10
		numPhysical = 3
		totalTicks  = 400
	)

	c := newMRCluster(t, numGroups, numPhysical)
	c.WaitAllLeaders(electionTimeout)

	rng := rand.New(rand.NewPCG(7, 0))

	// last[g] is the last value successfully written to key "drop" in group g.
	last := make([]string, numGroups)
	// lastTick[g] is the tick of the last successful propose to group g.
	lastTick := make([]int, numGroups)
	for g := range numGroups {
		lastTick[g] = -(numGroups * 2) // allow an immediate first propose
	}

	for tick := range totalTicks {
		// Randomly disconnect/reconnect a single (group, physical) node.
		if rng.IntN(10) == 0 {
			g := rng.IntN(numGroups)
			p := rng.IntN(numPhysical)
			c.DisconnectNode(g, p)
			c.TickN(3)
			c.ReconnectNode(g, p)
		}

		c.Tick()
		time.Sleep(time.Millisecond)

		// Cycle through groups in round-robin, proposing to each every ~20 ticks.
		g := tick % numGroups
		if tick-lastTick[g] >= numGroups*2 {
			cmd := fmt.Appendf(nil, "drop=%d", tick)
			if err := c.ProposeToGroup(g, electionTimeout/4, cmd); err == nil {
				last[g] = fmt.Sprintf("%d", tick)
				lastTick[g] = tick
			}
		}
	}

	// At least one group must have accepted proposals during the chaos window.
	proposed := 0
	for g := range numGroups {
		if last[g] != "" {
			proposed++
		}
	}
	if proposed == 0 {
		t.Fatal("no proposals succeeded during chaos test")
	}

	// Tick until every replica of every group has applied its group's last entry.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)

		allConverged := true
		for g := range numGroups {
			if last[g] == "" {
				continue
			}
			for p := range numPhysical {
				if c.sms[g][p].Get("drop") != last[g] {
					allConverged = false
					break
				}
			}
			if !allConverged {
				break
			}
		}
		if allConverged {
			return
		}
	}

	// Report which replicas are still behind.
	for g := range numGroups {
		if last[g] == "" {
			continue
		}
		for p := range numPhysical {
			if got := c.sms[g][p].Get("drop"); got != last[g] {
				t.Errorf("group %d physical %d: drop=%q, want %q (last written)", g+1, p, got, last[g])
			}
		}
	}
}
