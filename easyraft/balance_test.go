package easyraft_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
)

// balanceCluster is three hosts, each running a Manager with the same groups,
// so leadership has somewhere to move to.
type balanceCluster struct {
	managers  []*easyraft.Manager
	ids       []raft.NodeID
	raftAddrs []string
	httpAddrs []string
	groups    []uint64
}

// startBalanceCluster builds hosts nodes each hosting groups groups, with
// every Manager configured by extra.
func startBalanceCluster(t *testing.T, hosts, groups int,
	extra func(i int, httpAddrs map[raft.HostID]string) []easyraft.Option,
) *balanceCluster {
	t.Helper()

	c := &balanceCluster{}
	byHost := map[raft.HostID]string{}
	for i := range hosts {
		id := raft.NodeID(fmt.Sprintf("n%d", i+1))
		c.ids = append(c.ids, id)
		c.raftAddrs = append(c.raftAddrs, freePort(t))
		c.httpAddrs = append(c.httpAddrs, freePort(t))
		byHost[raft.HostID(id)] = c.httpAddrs[i]
	}
	peers := map[raft.NodeID]string{}
	for i, id := range c.ids {
		peers[id] = c.raftAddrs[i]
	}
	// Numbered from one: a Manager reserves group zero.
	for g := 1; g <= groups; g++ {
		c.groups = append(c.groups, uint64(g))
	}

	root := t.TempDir()
	for i := range hosts {
		opts := []easyraft.Option{
			easyraft.WithID(c.ids[i]),
			easyraft.WithRaftAddr(c.raftAddrs[i]),
			easyraft.WithHTTPAddr(c.httpAddrs[i]),
			easyraft.WithPeers(peers),
			easyraft.WithRaftTiming(10*time.Millisecond, 20*time.Millisecond,
				150*time.Millisecond, 300*time.Millisecond),
			easyraft.WithInsecureTransportAcknowledged(),
			easyraft.WithInsecureHTTPAcknowledged(),
		}
		opts = append(opts, extra(i, byHost)...)

		m, err := easyraft.NewManager(opts...)
		if err != nil {
			t.Fatalf("NewManager %s: %v", c.ids[i], err)
		}
		for _, g := range c.groups {
			if _, addErr := m.AddStore(g,
				easyraft.WithDataDir(filepath.Join(root, string(c.ids[i]), fmt.Sprintf("g%d", g))),
			); addErr != nil {
				t.Fatalf("AddStore %s/%d: %v", c.ids[i], g, addErr)
			}
		}
		c.managers = append(c.managers, m)
	}
	for i, m := range c.managers {
		if err := m.Start(); err != nil {
			t.Fatalf("Start %s: %v", c.ids[i], err)
		}
		t.Cleanup(func() { _ = m.Stop() })
	}
	return c
}

// leaderCounts returns how many groups each host leads, by host index.
func (c *balanceCluster) leaderCounts() []int {
	counts := make([]int, len(c.managers))
	for i, m := range c.managers {
		for _, status := range m.StatusAll(context.Background()) {
			if status.State == raft.Leader {
				counts[i]++
			}
		}
	}
	return counts
}

// waitAllLed waits until every group has a leader somewhere.
func (c *balanceCluster) waitAllLed(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		total := 0
		for _, n := range c.leaderCounts() {
			total += n
		}
		if total == len(c.groups) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("not every group found a leader; counts are %v", c.leaderCounts())
}

// pileOnto moves every group's leadership to one host, which is the state a
// rolling restart leaves behind. A transfer is aimed at whichever host
// currently leads the group, since only a leader can hand leadership on.
func (c *balanceCluster) pileOnto(t *testing.T, host int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, g := range c.groups {
		deadline := time.Now().Add(20 * time.Second)
		arrived := false
		for time.Now().Before(deadline) && !arrived {
			if c.leaderOf(ctx, g) == host {
				arrived = true
				break
			}
			for _, m := range c.managers {
				if err := m.TransferGroupLeadership(ctx, g, c.ids[host]); err == nil {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		// Every group has to have reached the target at least once, or the
		// test that follows is measuring a pile-up that never happened.
		if !arrived {
			t.Fatalf("group %d never moved to %s; counts are %v",
				g, c.ids[host], c.leaderCounts())
		}
	}
}

// leaderOf returns the index of the host leading g, or -1.
func (c *balanceCluster) leaderOf(ctx context.Context, g uint64) int {
	for i, m := range c.managers {
		for _, status := range m.StatusAll(ctx) {
			if status.GroupID == g && status.State == raft.Leader {
				return i
			}
		}
	}
	return -1
}

func spread(counts []int) int {
	lo, hi := counts[0], counts[0]
	for _, n := range counts[1:] {
		lo = min(lo, n)
		hi = max(hi, n)
	}
	return hi - lo
}

// TestLeaderBalancing_SpreadsLeadersAcrossHosts is the property the feature
// exists for. Nine groups elect leaders independently, so nothing stops one
// host leading most of them; with balancing on, the counts even out.
func TestLeaderBalancing_SpreadsLeadersAcrossHosts(t *testing.T) {
	const hosts, groups = 3, 9
	c := startBalanceCluster(t, hosts, groups, func(_ int, byHost map[raft.HostID]string) []easyraft.Option {
		return []easyraft.Option{
			easyraft.WithLeaderBalancing(byHost, 200*time.Millisecond,
				raft.WithGroupCooldown(400*time.Millisecond),
				raft.WithStatusTimeout(2*time.Second),
				raft.WithTransferTimeout(5*time.Second),
			),
		}
	})
	c.waitAllLed(t)

	// Pile every group onto one host, which is the state a rolling restart
	// leaves behind.
	c.pileOnto(t, 0)

	// pileOnto fails the test unless every group reached the target host, so
	// by here the pile-up definitely happened. What it may not still be is
	// visible: the controllers run every 200ms and will already have started
	// undoing it, which is the point.
	//
	// The controllers pull it apart.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		counts := c.leaderCounts()
		total := 0
		for _, n := range counts {
			total += n
		}
		if total == groups && spread(counts) <= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("leaders never evened out; counts are %v across %d groups",
		c.leaderCounts(), groups)
}

// TestLeaderBalancing_OffByDefault pins that nothing moves unless it was
// asked for: a transfer is an election, and a cluster that reshuffles itself
// without being told to is worse than an uneven one.
func TestLeaderBalancing_OffByDefault(t *testing.T) {
	const hosts, groups = 3, 6
	c := startBalanceCluster(t, hosts, groups, func(int, map[raft.HostID]string) []easyraft.Option {
		return nil
	})
	c.waitAllLed(t)

	c.pileOnto(t, 0)

	// Whatever the pile-up achieved is what should still be there. Comparing
	// against the counts as they actually are, rather than against a perfect
	// pile-up, keeps this test about the one thing it is for: that nothing
	// moves leadership when nothing was asked to.
	before := c.leaderCounts()
	time.Sleep(2 * time.Second)
	after := c.leaderCounts()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("leadership moved with no balancing configured: %v became %v", before, after)
	}
	if spread(before) < 2 {
		t.Fatalf("the pile-up left the counts at %v, so there was nothing for a balancer "+
			"to have undone", before)
	}
}

// TestLeaderBalancing_ConfigurationIsCheckedAtStart covers the three
// misconfigurations that would otherwise look like balancing that silently
// does nothing.
func TestLeaderBalancing_ConfigurationIsCheckedAtStart(t *testing.T) {
	newManager := func(t *testing.T, opts ...easyraft.Option) *easyraft.Manager {
		t.Helper()
		raftAddr := freePort(t)
		base := []easyraft.Option{
			easyraft.WithID("n1"),
			easyraft.WithRaftAddr(raftAddr),
			easyraft.WithPeers(map[raft.NodeID]string{"n1": raftAddr}),
			easyraft.WithInsecureTransportAcknowledged(),
			easyraft.WithInsecureHTTPAcknowledged(),
		}
		m, err := easyraft.NewManager(append(base, opts...)...)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		if _, addErr := m.AddStore(1, easyraft.WithDataDir(filepath.Join(t.TempDir(), "g1"))); addErr != nil {
			t.Fatal(addErr)
		}
		return m
	}

	tests := []struct {
		name  string
		opts  []easyraft.Option
		wants string
	}{
		{
			name: "this host is not in the list",
			opts: []easyraft.Option{
				easyraft.WithHTTPAddr(freePort(t)),
				easyraft.WithLeaderBalancing(map[raft.HostID]string{
					"n2": "127.0.0.1:9002", "n3": "127.0.0.1:9003",
				}, time.Second),
			},
			wants: "not this one",
		},
		{
			name: "only one host",
			opts: []easyraft.Option{
				easyraft.WithHTTPAddr(freePort(t)),
				easyraft.WithLeaderBalancing(map[raft.HostID]string{
					"n1": "127.0.0.1:9001",
				}, time.Second),
			},
			wants: "at least two hosts",
		},
		{
			name: "no HTTP listener",
			opts: []easyraft.Option{
				easyraft.WithLeaderBalancing(map[raft.HostID]string{
					"n1": "127.0.0.1:9001", "n2": "127.0.0.1:9002",
				}, time.Second),
			},
			wants: "WithHTTPAddr",
		},
		{
			name: "an address that is not one",
			opts: []easyraft.Option{
				easyraft.WithHTTPAddr(freePort(t)),
				easyraft.WithLeaderBalancing(map[raft.HostID]string{
					"n1": "127.0.0.1:9001", "n2": "not an address",
				}, time.Second),
			},
			wants: "not a usable host:port",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t, tc.opts...)
			err := m.Start()
			if err == nil {
				_ = m.Stop()
				t.Fatal("Start succeeded")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("Start said %q, which does not mention %q", err, tc.wants)
			}
		})
	}
}

// TestLeaderBalancing_EndpointsAreGuarded checks that the two endpoints a
// controller uses sit behind the same authorization as everything else. One
// of them moves leadership.
func TestLeaderBalancing_EndpointsAreGuarded(t *testing.T) {
	const token = "balance-token"
	raftAddr, httpAddr := freePort(t), freePort(t)
	m, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(raftAddr),
		easyraft.WithHTTPAddr(httpAddr),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": raftAddr}),
		easyraft.WithBearerTokenAuth(token),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, addErr := m.AddStore(1, easyraft.WithDataDir(filepath.Join(t.TempDir(), "g1"))); addErr != nil {
		t.Fatal(addErr)
	}
	if startErr := m.Start(); startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}
	t.Cleanup(func() { _ = m.Stop() })

	base := "http://" + httpAddr
	for _, call := range []struct{ method, path, body string }{
		{"GET", "/__balance/status", ""},
		{"POST", "/__balance/transfer", `{"group_id":1,"to":"n1"}`},
	} {
		status, _, _ := doRequest(t, call.method, base+call.path, call.body, nil)
		if status != 401 && status != 403 {
			t.Errorf("%s %s with no credential answered %d, want 401 or 403",
				call.method, call.path, status)
		}
		authorized := map[string]string{"Authorization": "Bearer " + token}
		status, body, _ := doRequest(t, call.method, base+call.path, call.body, authorized)
		if status == 401 || status == 403 {
			t.Errorf("%s %s with a valid credential answered %d %s",
				call.method, call.path, status, body)
		}
	}
}

// TestLeaderBalancing_StatusEndpointReportsEveryGroup checks the half a
// remote controller reads, since a view that misses groups plans against a
// picture of a cluster that does not exist.
func TestLeaderBalancing_StatusEndpointReportsEveryGroup(t *testing.T) {
	const hosts, groups = 2, 4
	c := startBalanceCluster(t, hosts, groups, func(int, map[raft.HostID]string) []easyraft.Option {
		return nil
	})
	c.waitAllLed(t)

	provider := raft.NewHTTPNodeProvider("http://"+c.httpAddrs[0]+"/__balance", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	statuses := provider.StatusAll(ctx)
	if len(statuses) != groups {
		t.Fatalf("the status endpoint reported %d groups, want %d", len(statuses), groups)
	}
	seen := map[uint64]bool{}
	for _, status := range statuses {
		seen[status.GroupID] = true
		if status.NodeID != c.ids[0] {
			t.Errorf("group %d reports node %q, want %q", status.GroupID, status.NodeID, c.ids[0])
		}
	}
	for _, g := range c.groups {
		if !seen[g] {
			t.Errorf("group %d was missing from the status", g)
		}
	}

	// And the transfer half moves a group to another host.
	//
	// Which host leads which group is whatever the elections decided, so the
	// transfer is aimed from wherever the leader actually is rather than from
	// host 0. Assuming host 0 leads something passes locally and fails on a
	// machine whose elections went the other way.
	var (
		moved    uint64
		fromHost = -1
	)
	for host, m := range c.managers {
		for _, status := range m.StatusAll(ctx) {
			if status.State == raft.Leader {
				moved, fromHost = status.GroupID, host
				break
			}
		}
		if fromHost >= 0 {
			break
		}
	}
	if fromHost < 0 {
		t.Fatalf("no host leads any group; counts are %v", c.leaderCounts())
	}
	destIndex := (fromHost + 1) % len(c.managers)

	fromProvider := raft.NewHTTPNodeProvider("http://"+c.httpAddrs[fromHost]+"/__balance", nil)
	if err := fromProvider.TransferGroupLeadership(ctx, moved, c.ids[destIndex]); err != nil {
		t.Fatalf("TransferGroupLeadership of group %d from %s over HTTP: %v",
			moved, c.ids[fromHost], err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, status := range c.managers[destIndex].StatusAll(ctx) {
			if status.GroupID == moved && status.State == raft.Leader {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("group %d did not move from %s to %s", moved, c.ids[fromHost], c.ids[destIndex])
}

// TestManager_GroupZeroIsRefused pins the trap this guard exists for. A
// Manager routes inbound RPCs by the group ID they carry, and zero is what a
// single-group node's RPCs carry, so the transport refuses it: a group
// numbered zero gets no votes, no appends and no election, and sits in
// PreCandidate for ever with nothing in its own log to say why.
func TestManager_GroupZeroIsRefused(t *testing.T) {
	raftAddr := freePort(t)
	m, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(raftAddr),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": raftAddr}),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop() })

	_, err = m.AddStore(0, easyraft.WithDataDir(filepath.Join(t.TempDir(), "g0")))
	if err == nil {
		t.Fatal("AddStore(0) succeeded")
	}
	if !strings.Contains(err.Error(), "group 0 is reserved") {
		t.Errorf("AddStore(0) said %q, which does not explain that zero is reserved", err)
	}

	// One is fine, and so is any other non-zero id.
	for _, g := range []uint64{1, 7, 1 << 40} {
		if _, addErr := m.AddStore(g,
			easyraft.WithDataDir(filepath.Join(t.TempDir(), fmt.Sprintf("g%d", g))),
		); addErr != nil {
			t.Errorf("AddStore(%d): %v", g, addErr)
		}
	}
}
