package raft

import (
	"slices"
	"sort"
)

// Transfer describes a single leadership handoff: move the leader of GroupID
// from the Raft node identified by From to the one identified by To.
type Transfer struct {
	GroupID uint64
	From    NodeID // current leader's Raft NodeID
	To      NodeID // transfer-target Raft NodeID
}

// Balancer computes leadership transfers that improve distribution across
// physical nodes. view maps each physical-node identifier to the GroupStatus
// slice reported by that node. Implementations must be stateless; the
// BalanceController calls Plan on every rebalance interval.
type Balancer interface {
	Plan(view map[NodeID][]GroupStatus) []Transfer
}

// LeastLeadersBalancer is a greedy balancer that minimises the maximum number
// of leaders hosted by any single physical node. On each call it moves leaders
// from the most-loaded node to the least-loaded until all counts differ by at
// most 1.
type LeastLeadersBalancer struct{}

// Plan implements Balancer.
func (LeastLeadersBalancer) Plan(view map[NodeID][]GroupStatus) []Transfer {
	if len(view) < 2 {
		return nil
	}

	// Build an index: Raft NodeID → physical node ID.
	nodeToPhys := make(map[NodeID]NodeID)
	for physID, statuses := range view {
		for _, s := range statuses {
			nodeToPhys[s.NodeID] = physID
		}
	}

	// Collect all statuses indexed by GroupID (needed to find transfer targets).
	byGroup := make(map[uint64][]GroupStatus)
	for _, statuses := range view {
		for _, s := range statuses {
			byGroup[s.GroupID] = append(byGroup[s.GroupID], s)
		}
	}

	// Build a mutable per-physical-node leader list.
	type physNode struct {
		id      NodeID
		leaders []GroupStatus // statuses where State == Leader on this physical node
	}
	nodes := make([]*physNode, 0, len(view))
	for physID, statuses := range view {
		pn := &physNode{id: physID}
		for _, s := range statuses {
			if s.State == Leader {
				pn.leaders = append(pn.leaders, s)
			}
		}
		nodes = append(nodes, pn)
	}

	var transfers []Transfer

	for {
		// Sort: busiest first, least-busy last.
		sort.Slice(nodes, func(i, j int) bool {
			return len(nodes[i].leaders) > len(nodes[j].leaders)
		})

		busiest := nodes[0]
		leastBusy := nodes[len(nodes)-1]

		if len(busiest.leaders)-len(leastBusy.leaders) <= 1 {
			break // balanced within ±1
		}

		// Find a group on busiest that has a replica on leastBusy, and plan a
		// transfer. We skip groups where leastBusy has no replica.
		moved := false
		for i, leader := range busiest.leaders {
			for _, candidate := range byGroup[leader.GroupID] {
				if candidate.NodeID == leader.NodeID {
					continue // same replica
				}
				if nodeToPhys[candidate.NodeID] != leastBusy.id {
					continue // not on leastBusy
				}
				transfers = append(transfers, Transfer{
					GroupID: leader.GroupID,
					From:    leader.NodeID,
					To:      candidate.NodeID,
				})
				// Update the simulation so the next iteration reflects the
				// transfer we just planned.
				busiest.leaders = slices.Delete(busiest.leaders, i, i+1)
				leastBusy.leaders = append(leastBusy.leaders, leader)
				moved = true
				break
			}
			if moved {
				break
			}
		}

		if !moved {
			break // cannot improve further
		}
	}

	return transfers
}
