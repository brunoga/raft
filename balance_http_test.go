package raft_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

// ---- Manager.Handler unit tests ---------------------------------------------

// TestManagerHTTP_Status verifies that GET /status returns the correct JSON
// representation of Manager.StatusAll().
func TestManagerHTTP_Status(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()
	node := newManagedNode(t, mgr, net, 1, "n1", nil)
	node.Start()
	defer node.Stop()

	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}

	var statuses []raft.GroupStatus
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("want 1 status, got %d", len(statuses))
	}
	s := statuses[0]
	if s.GroupID != 1 {
		t.Errorf("GroupID: want 1, got %d", s.GroupID)
	}
	if s.NodeID != "n1" {
		t.Errorf("NodeID: want n1, got %q", s.NodeID)
	}
	// State must round-trip as a string.
	if s.State != raft.Follower && s.State != raft.Leader && s.State != raft.Candidate && s.State != raft.PreCandidate {
		t.Errorf("State: unexpected value %v", s.State)
	}
}

// TestManagerHTTP_Transfer_OK verifies that POST /transfer with valid JSON
// is accepted and returns 204.
func TestManagerHTTP_Transfer_OK(t *testing.T) {
	net := memtransport.NewNetwork()
	mgr := raft.NewManager()

	// Three-node group so TransferLeadership has a valid target.
	ids := []raft.NodeID{"a", "b", "c"}
	nodes := make([]*raft.Node, len(ids))
	for i, id := range ids {
		peers := make([]raft.PeerConfig, 0, len(ids)-1)
		for _, p := range ids {
			if p != id {
				peers = append(peers, raft.PeerConfig{ID: p, Voter: true})
			}
		}
		cfg := raft.DefaultConfig()
		cfg.GroupID = 1
		cfg.ID = id
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = &discardSM{}
		cfg.Transport = net.NewTransport(id)
		cfg.TickInterval = 0
		n, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New %s: %v", id, err)
		}
		nodes[i] = n
		net.Register(id, n.Handler())
		n.Start()
	}
	// Only register node 0 in the manager for this test.
	if err := mgr.Add(1, nodes[0]); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			n.Stop()
		}
	})

	// Tick until a leader is elected on node 0.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			n.Tick()
		}
		if nodes[0].State() == raft.Leader {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if nodes[0].State() != raft.Leader {
		t.Skip("node 0 is not the leader; skipping transfer test")
	}

	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"group_id": 1, "to": "b"})
	respCh := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/transfer", "application/json", bytes.NewReader(body)) //nolint:bodyclose // body closed by receiver after status check
		if err != nil {
			t.Errorf("POST /transfer: %v", err)
			return
		}
		respCh <- resp
	}()

	// Keep ticking so the transfer protocol can complete.
	deadline = time.Now().Add(3 * time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			n.Tick()
		}
		time.Sleep(time.Millisecond)
		select {
		case resp = <-respCh:
			goto done
		default:
		}
	}
done:
	if resp == nil {
		t.Fatal("POST /transfer: no response within deadline")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("POST /transfer: want 204, got %d", resp.StatusCode)
	}
}

// TestManagerHTTP_Transfer_NotFound verifies that POSTing to an unregistered
// group returns 404.
func TestManagerHTTP_Transfer_NotFound(t *testing.T) {
	mgr := raft.NewManager()
	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"group_id": 99, "to": "x"})
	resp, err := http.Post(srv.URL+"/transfer", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /transfer: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", resp.StatusCode)
	}
}

// TestManagerHTTP_Transfer_BadJSON verifies that a malformed body returns 400.
func TestManagerHTTP_Transfer_BadJSON(t *testing.T) {
	mgr := raft.NewManager()
	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/transfer", "application/json",
		bytes.NewReader([]byte("not-json")))
	if err != nil {
		t.Fatalf("POST /transfer: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

// ---- HTTPNodeProvider round-trip test ---------------------------------------

// TestHTTPNodeProvider_StatusAll verifies that HTTPNodeProvider.StatusAll()
// returns the same statuses as Manager.StatusAll() via the HTTP endpoint.
func TestHTTPNodeProvider_StatusAll(t *testing.T) {
	mgr := raft.NewManager()
	net := memtransport.NewNetwork()
	node := newManagedNode(t, mgr, net, 42, "n42", nil)
	node.Start()
	defer node.Stop()

	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	provider := raft.NewHTTPNodeProvider(srv.URL, nil)
	statuses := provider.StatusAll()

	if len(statuses) != 1 {
		t.Fatalf("want 1 status, got %d", len(statuses))
	}
	if statuses[0].GroupID != 42 {
		t.Errorf("GroupID: want 42, got %d", statuses[0].GroupID)
	}
	if statuses[0].NodeID != "n42" {
		t.Errorf("NodeID: want n42, got %q", statuses[0].NodeID)
	}
}

// ---- End-to-end: BalanceController + HTTPNodeProvider -----------------------

// TestBalanceController_HTTPProviders_ConvergesUniform is the HTTP-based
// analogue of TestBalanceController_ConvergesUniform. It starts real httptest
// servers in front of each Manager, wires the BalanceController with
// HTTPNodeProviders, and verifies that leaders converge to ±1 per physical
// node.
func TestBalanceController_HTTPProviders_ConvergesUniform(t *testing.T) {
	if testing.Short() {
		t.Skip("HTTP balance integration test skipped in short mode")
	}

	const (
		numGroups   = 6
		numPhysical = 3
	)

	c := newMRCluster(t, numGroups, numPhysical)
	c.WaitAllLeaders(electionTimeout)

	// Force all leaders to physical node 0.
	for g := range numGroups {
		target := c.ids[g][0]
		deadline := time.Now().Add(electionTimeout)
		for time.Now().Before(deadline) {
			if c.leaderOf(g) == 0 {
				break
			}
			p := c.leaderOf(g)
			if p < 0 {
				c.Tick()
				time.Sleep(time.Millisecond)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			_ = c.groups[g][p].TransferLeadership(ctx, target)
			cancel()
			c.TickN(10)
		}
		if c.leaderOf(g) != 0 {
			t.Fatalf("group %d: could not transfer leader to physical 0", g+1)
		}
	}

	// Start one httptest server per physical node.
	servers := make([]*httptest.Server, numPhysical)
	for p := range numPhysical {
		servers[p] = httptest.NewServer(c.mgrs[p].Handler())
	}
	t.Cleanup(func() {
		for _, s := range servers {
			s.Close()
		}
	})

	// Wire BalanceController with HTTPNodeProviders.
	providers := make(map[raft.NodeID]raft.NodeProvider, numPhysical)
	for p := range numPhysical {
		providers[raft.NodeID(fmt.Sprintf("phys%d", p))] = raft.NewHTTPNodeProvider(servers[p].URL, nil)
	}

	ctrl := raft.NewBalanceController(providers, raft.LeastLeadersBalancer{}, 20*time.Millisecond)
	go ctrl.Run(t.Context())

	// Tick and wait for ±1 balance.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)

		leadersByPhys := make([]int, numPhysical)
		allHaveLeader := true
		for g := range numGroups {
			p := c.leaderOf(g)
			if p < 0 {
				allHaveLeader = false
				break
			}
			leadersByPhys[p]++
		}
		if !allHaveLeader {
			continue
		}
		minC, maxC := leadersByPhys[0], leadersByPhys[0]
		for _, n := range leadersByPhys[1:] {
			if n < minC {
				minC = n
			}
			if n > maxC {
				maxC = n
			}
		}
		if maxC-minC <= 1 {
			return
		}
	}

	leadersByPhys := make([]int, numPhysical)
	for g := range numGroups {
		if p := c.leaderOf(g); p >= 0 {
			leadersByPhys[p]++
		}
	}
	t.Errorf("leaders not balanced within electionTimeout via HTTP: %v (want each ~%d ±1)",
		leadersByPhys, numGroups/numPhysical)
}
