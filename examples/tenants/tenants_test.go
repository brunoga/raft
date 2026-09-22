package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/client"
	tenantmap "github.com/brunoga/raft/v2/examples/tenants/tenants"
)

// freePort returns an address nothing is listening on.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if closeErr := ln.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return addr
}

// cluster is a set of tenants nodes, each a Manager hosting every group.
type cluster struct {
	managers  []*easyraft.Manager
	ids       []raft.NodeID
	httpAddrs []string
	groups    int
}

// start brings up hosts nodes, each hosting groups groups, with leader
// balancing on when balance is true.
//
// The Raft transport is the real gRPC one rather than easyrafttest's
// in-memory network, because a Manager is what is under test here and it
// carries the shared WAL and the group routing that go with it.
func start(t *testing.T, hosts, groups int, balance bool) *cluster {
	t.Helper()

	c := &cluster{groups: groups}
	peers := map[raft.NodeID]string{}
	balanceHosts := map[raft.HostID]string{}
	raftAddrs := make([]string, hosts)

	for i := range hosts {
		id := raft.NodeID(fmt.Sprintf("n%d", i+1))
		c.ids = append(c.ids, id)
		raftAddrs[i] = freePort(t)
		c.httpAddrs = append(c.httpAddrs, freePort(t))
		peers[id] = raftAddrs[i]
		balanceHosts[raft.HostID(id)] = c.httpAddrs[i]
	}

	root := t.TempDir()
	for i := range hosts {
		mux := http.NewServeMux()
		opts := []easyraft.Option{
			easyraft.WithID(c.ids[i]),
			easyraft.WithRaftAddr(raftAddrs[i]),
			easyraft.WithHTTPAddr(c.httpAddrs[i]),
			easyraft.WithHTTPMux(mux),
			easyraft.WithDataDir(filepath.Join(root, string(c.ids[i]))),
			easyraft.WithRaftTiming(10*time.Millisecond, 20*time.Millisecond,
				150*time.Millisecond, 300*time.Millisecond),
			easyraft.WithInsecureTransportAcknowledged(),
			easyraft.WithInsecureHTTPAcknowledged(),
			easyraft.WithSharedWAL(),
		}
		if balance {
			opts = append(opts, easyraft.WithLeaderBalancing(balanceHosts, 200*time.Millisecond,
				raft.WithGroupCooldown(400*time.Millisecond),
				raft.WithStatusTimeout(2*time.Second),
				raft.WithTransferTimeout(5*time.Second),
			))
		}

		m, err := easyraft.NewManager(opts...)
		if err != nil {
			t.Fatalf("NewManager %s: %v", c.ids[i], err)
		}
		for g := 1; g <= groups; g++ {
			if _, addErr := m.AddStore(uint64(g), easyraft.WithPeers(peers)); addErr != nil {
				t.Fatalf("AddStore %s/%d: %v", c.ids[i], g, addErr)
			}
		}

		srv := &server{manager: m, groups: groups, known: []string{"acme", "globex", "initech"}}
		mux.HandleFunc("GET /tenants", srv.handleTenants)

		httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		ln, err := net.Listen("tcp", c.httpAddrs[i])
		if err != nil {
			t.Fatalf("listen %s: %v", c.httpAddrs[i], err)
		}
		go func() { _ = httpSrv.Serve(ln) }()
		t.Cleanup(func() { _ = httpSrv.Close() })

		c.managers = append(c.managers, m)
	}

	for i, m := range c.managers {
		if err := m.Start(); err != nil {
			t.Fatalf("Start %s: %v", c.ids[i], err)
		}
		t.Cleanup(func() { _ = m.Stop() })
	}
	c.waitAllLed(t)
	return c
}

func (c *cluster) leaderCounts() []int {
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

func (c *cluster) waitAllLed(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		total := 0
		for _, n := range c.leaderCounts() {
			total += n
		}
		if total == c.groups {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("not every group found a leader; counts are %v", c.leaderCounts())
}

func spread(counts []int) int {
	lo, hi := counts[0], counts[0]
	for _, n := range counts[1:] {
		lo, hi = min(lo, n), max(hi, n)
	}
	return hi - lo
}

// TestTenants_IsolatedByGroup is the shape of the example: each tenant's
// items live in its own Raft group, addressed with client.WithGroup.
func TestTenants_IsolatedByGroup(t *testing.T) {
	c := start(t, 3, 4, false)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Two tenants that hash to different groups, so the isolation is real
	// rather than two names in one group.
	var a, b string
	for _, name := range []string{"acme", "globex", "initech", "umbrella", "hooli"} {
		group, err := tenantmap.GroupFor(name, c.groups)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case a == "":
			a = name
		case b == "":
			otherGroup, _ := tenantmap.GroupFor(a, c.groups)
			if group != otherGroup {
				b = name
			}
		}
	}
	if b == "" {
		t.Fatalf("no two of the candidate tenants landed in different groups of %d", c.groups)
	}

	for _, tenant := range []string{a, b} {
		items := c.items(t, tenant)
		if err := items.Upsert(ctx, "greeting", tenantmap.Item{Value: "hello " + tenant}); err != nil {
			t.Fatalf("%s: Upsert: %v", tenant, err)
		}
	}

	for _, tenant := range []string{a, b} {
		got, err := c.items(t, tenant).Read(ctx, "greeting")
		if err != nil {
			t.Fatalf("%s: Read: %v", tenant, err)
		}
		if got.Value != "hello "+tenant {
			t.Errorf("%s holds %q", tenant, got.Value)
		}
	}

	// One tenant's keys are not in the other's group.
	aItems, err := c.items(t, a).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(aItems) != 1 {
		t.Errorf("tenant %s lists %d items, want only its own", a, len(aItems))
	}
}

// items returns a client pointed at one tenant's group, which is the whole
// job WithGroup does.
func (c *cluster) items(t *testing.T, tenant string) *client.Coll[tenantmap.Item] {
	t.Helper()
	group, err := tenantmap.GroupFor(tenant, c.groups)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(
		client.WithEndpoints(c.httpAddrs...),
		client.WithGroup(group),
	)
	if err != nil {
		t.Fatal(err)
	}
	return client.Collection[tenantmap.Item](cl, tenantmap.CollectionName)
}

// TestTenants_LeadersEvenOut covers the reason this example uses a Manager:
// groups elect independently, so without balancing one host ends up leading
// most of them.
func TestTenants_LeadersEvenOut(t *testing.T) {
	const hosts, groups = 3, 9
	c := start(t, hosts, groups, true)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Pile every group onto one host, the state a rolling restart leaves.
	for _, g := range groupIDs(groups) {
		deadline := time.Now().Add(20 * time.Second)
		arrived := false
		for time.Now().Before(deadline) && !arrived {
			for _, m := range c.managers {
				if err := m.TransferGroupLeadership(ctx, g, c.ids[0]); err == nil {
					break
				}
			}
			for _, status := range c.managers[0].StatusAll(ctx) {
				if status.GroupID == g && status.State == raft.Leader {
					arrived = true
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !arrived {
			t.Fatalf("group %d never moved to %s; counts are %v", g, c.ids[0], c.leaderCounts())
		}
	}

	// The controllers pull it apart again.
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
	t.Errorf("leaders never evened out; counts are %v across %d groups", c.leaderCounts(), groups)
}

// TestTenants_SharedWALHoldsEveryGroup checks the second reason for a
// Manager: one log per host rather than one per group.
func TestTenants_SharedWALHoldsEveryGroup(t *testing.T) {
	const groups = 4
	c := start(t, 1, groups, false)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, tenant := range []string{"acme", "globex", "initech"} {
		if err := c.items(t, tenant).Upsert(ctx, "k", tenantmap.Item{Value: tenant}); err != nil {
			t.Fatalf("%s: %v", tenant, err)
		}
	}

	wal := c.managers[0].SharedWAL()
	if wal == nil {
		t.Fatal("WithSharedWAL was set but the Manager reports no shared log")
	}
	held := wal.Groups()
	if len(held) != groups {
		t.Errorf("the shared log holds %d groups, want %d: %v", len(held), groups, held)
	}
}

// TestTenants_ReportsWhereEachTenantLives covers this example's one route.
func TestTenants_ReportsWhereEachTenantLives(t *testing.T) {
	c := start(t, 3, 4, false)

	var view struct {
		Groups  int `json:"groups"`
		Tenants []struct {
			Tenant string      `json:"tenant"`
			Group  uint64      `json:"group"`
			Leader raft.NodeID `json:"leader"`
			Here   bool        `json:"led_here"`
		} `json:"tenants"`
	}
	body := get(t, "http://"+c.httpAddrs[0]+"/tenants")
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if view.Groups != 4 {
		t.Errorf("reported %d groups, want 4", view.Groups)
	}
	if len(view.Tenants) != 3 {
		t.Fatalf("reported %d tenants, want 3", len(view.Tenants))
	}
	for _, entry := range view.Tenants {
		want, err := tenantmap.GroupFor(entry.Tenant, 4)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Group != want {
			t.Errorf("%s is reported in group %d, want %d", entry.Tenant, entry.Group, want)
		}
		if entry.Group < 1 || entry.Group > 4 {
			t.Errorf("%s is in group %d, outside 1..4", entry.Tenant, entry.Group)
		}
	}
	// Sorted, so the output is stable between calls.
	for i := 1; i < len(view.Tenants); i++ {
		if view.Tenants[i-1].Tenant > view.Tenants[i].Tenant {
			t.Errorf("tenants are not sorted: %v", view.Tenants)
			break
		}
	}
}

// TestGroupFor_NeverReturnsZero pins the trap the mapping exists to avoid: a
// Manager routes by group ID and zero means "no group", so a tenant hashed
// into group zero would be unreachable and easyraft refuses to create one.
func TestGroupFor_NeverReturnsZero(t *testing.T) {
	for groups := 1; groups <= 16; groups++ {
		for i := range 500 {
			group, err := tenantmap.GroupFor(fmt.Sprintf("tenant-%d", i), groups)
			if err != nil {
				t.Fatalf("GroupFor(%d groups): %v", groups, err)
			}
			if group < 1 || group > uint64(groups) {
				t.Fatalf("tenant-%d of %d groups mapped to %d", i, groups, group)
			}
		}
	}
	if _, err := tenantmap.GroupFor("acme", 0); err == nil {
		t.Error("GroupFor accepted zero groups")
	}
	if _, err := tenantmap.GroupFor("", 4); err == nil {
		t.Error("GroupFor accepted an empty tenant name")
	}
}

func groupIDs(n int) []uint64 {
	out := make([]uint64, 0, n)
	for g := 1; g <= n; g++ {
		out = append(out, uint64(g))
	}
	return out
}

func get(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
