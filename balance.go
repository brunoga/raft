package raft

import (
	"slices"
	"sort"
)

// Transfer describes a single leadership handoff: move the leader of GroupID
// from the Raft node identified by From to the one identified by To.
type Transfer struct {
	// GroupID identifies the Raft group whose leadership should move.
	GroupID uint64
	// From is the Raft node currently leading that group.
	From NodeID
	// To is the Raft node that should lead it instead. It must be a voter:
	// leadership cannot be transferred to a member that cannot win an
	// election.
	To NodeID
}

// Balancer computes leadership transfers that improve distribution across
// physical hosts. view maps each host to the GroupStatus slice that host
// reported. Implementations must be stateless; the BalanceController calls
// Plan on every rebalance interval.
type Balancer interface {
	Plan(view map[HostID][]GroupStatus) []Transfer
}

// LeastLeadersBalancer is a greedy balancer that minimises the maximum number
// of leaders hosted by any single physical node. On each call it moves leaders
// from the most-loaded node to the least-loaded until all counts differ by at
// most 1.
//
// A replica is only considered as a destination if it votes and is close enough
// to the leader to take over: a non-voter cannot be elected at all, and a
// replica that is far behind would have to catch up before it could serve
// anything, turning a rebalance into an outage for that group.
type LeastLeadersBalancer struct {
	// MaxLag is how far behind the current leader a replica may be and still be
	// considered as a destination, in log entries. Zero means the default of
	// 1024.
	MaxLag Index
}

// defaultBalancerMaxLag is the lag tolerated by LeastLeadersBalancer when
// MaxLag is not set.
const defaultBalancerMaxLag Index = 1024

// eligibleTarget reports whether candidate can take leadership of a group
// currently led by leader.
func eligibleTarget(leader, candidate GroupStatus, maxLag Index) bool {
	if candidate.NodeID == leader.NodeID || !candidate.Voter {
		return false
	}
	if maxLag == 0 {
		maxLag = defaultBalancerMaxLag
	}
	return candidate.LastApplied+maxLag >= leader.LastApplied
}

// Plan implements Balancer.
func (b LeastLeadersBalancer) Plan(view map[HostID][]GroupStatus) []Transfer {
	if len(view) < 2 {
		return nil
	}

	// Build an index: Raft node → the host running it.
	nodeToHost := make(map[NodeID]HostID)
	for hostID, statuses := range view {
		for _, s := range statuses {
			nodeToHost[s.NodeID] = hostID
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
	type physHost struct {
		id      HostID
		leaders []GroupStatus // statuses where State == Leader on this host
	}
	hosts := make([]*physHost, 0, len(view))
	for hostID, statuses := range view {
		ph := &physHost{id: hostID}
		for _, s := range statuses {
			if s.State == Leader {
				ph.leaders = append(ph.leaders, s)
			}
		}
		hosts = append(hosts, ph)
	}

	var transfers []Transfer

	for {
		// Sort: busiest first, least-busy last.
		sort.Slice(hosts, func(i, j int) bool {
			return len(hosts[i].leaders) > len(hosts[j].leaders)
		})

		busiest := hosts[0]
		leastBusy := hosts[len(hosts)-1]

		if len(busiest.leaders)-len(leastBusy.leaders) <= 1 {
			break // balanced within ±1
		}

		// Find a group on busiest that has a replica on leastBusy, and plan a
		// transfer. We skip groups where leastBusy has no replica.
		moved := false
		for i, leader := range busiest.leaders {
			for _, candidate := range byGroup[leader.GroupID] {
				if nodeToHost[candidate.NodeID] != leastBusy.id {
					continue // not on leastBusy
				}
				if !eligibleTarget(leader, candidate, b.MaxLag) {
					continue
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
