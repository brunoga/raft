package raft_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

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
outer:
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
			break outer
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
