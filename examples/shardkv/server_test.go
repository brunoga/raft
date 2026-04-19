package main

// Integration tests for the shardkv HTTP API.
//
// Each test spins up a 3-physical-node, 4-shard in-process cluster using
// memtransport + memstore. Each physical node gets its own Manager, its own
// set of 4 Raft nodes (one per shard), and one httptest.Server. A background
// goroutine drives each Manager's RunTicker at 1 ms — the same pattern used
// in production, where a single ticker drives all shards on a node.
//
// The test cluster uses TickInterval=0 (Manager-driven ticks) so all timing
// is controlled by the background ticker rather than wall-clock scheduling.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

const (
	testNumPhysical    = 3
	testNumShards      = uint64(4)
	testElectionTimeout = 5 * time.Second
)

// shardCluster is a multi-physical-node shardkv cluster for testing.
type shardCluster struct {
	t            *testing.T
	physIDs      []string
	numShards    uint64
	mgrs         []*raft.Manager
	sms          []map[uint64]*KvSM // [physIdx][shardID]
	servers      []*httptest.Server
	net          *memtransport.Network
	nodeHTTPAddr map[raft.NodeID]string
	stopOnce     sync.Once
	cancelTick   context.CancelFunc
}

// newShardCluster creates a numPhysical-node, numShards-shard in-process cluster.
func newShardCluster(t *testing.T, numPhysical int, numShards uint64) *shardCluster {
	t.Helper()

	network := memtransport.NewNetwork()
	physIDs := make([]string, numPhysical)
	for i := range numPhysical {
		physIDs[i] = fmt.Sprintf("p%d", i+1)
	}

	mgrs := make([]*raft.Manager, numPhysical)
	for i := range numPhysical {
		mgrs[i] = raft.NewManager()
	}

	smsByPhys := make([]map[uint64]*KvSM, numPhysical)
	for i := range numPhysical {
		smsByPhys[i] = make(map[uint64]*KvSM, numShards)
	}

	// nodeHTTPAddr is populated after httptest.Servers are created (below).
	nodeHTTPAddr := make(map[raft.NodeID]string)

	// Create all shard nodes. For each shard gid, each physical node pi hosts
	// one Raft node with a distinct NodeID "g{gid}-p{i+1}".
	for gid := uint64(1); gid <= numShards; gid++ {
		// Collect all NodeIDs in this shard group.
		allIDs := make([]raft.NodeID, numPhysical)
		for i, physID := range physIDs {
			allIDs[i] = shardNodeID(gid, physID)
		}

		for i, physID := range physIDs {
			nodeID := shardNodeID(gid, physID)

			peers := make([]raft.NodeID, 0, numPhysical-1)
			for _, id := range allIDs {
				if id != nodeID {
					peers = append(peers, id)
				}
			}

			sm := &KvSM{data: make(map[string]string)}
			smsByPhys[i][gid] = sm

			cfg := raft.DefaultConfig()
			cfg.GroupID = gid
			cfg.ID = nodeID
			cfg.Peers = peers
			cfg.Storage = memstore.New()
			cfg.StateMachine = sm
			cfg.Transport = network.NewTransport(nodeID)
			cfg.TickInterval = 0 // driven by Manager.RunTicker

			node, err := raft.New(&cfg)
			if err != nil {
				t.Fatalf("raft.New(shard %d, phys %s): %v", gid, physID, err)
			}
			network.Register(nodeID, node)

			if err := mgrs[i].Add(gid, node); err != nil {
				t.Fatalf("mgrs[%d].Add(shard %d): %v", i, gid, err)
			}
		}
	}

	for _, mgr := range mgrs {
		mgr.StartAll()
	}

	// Create HTTP servers. buildMux captures nodeHTTPAddr by reference; we
	// populate the map below after server URLs are known.
	servers := make([]*httptest.Server, numPhysical)
	for i, physID := range physIDs {
		servers[i] = httptest.NewServer(
			buildMux(physID, numShards, mgrs[i], smsByPhys[i], nodeHTTPAddr),
		)
	}

	// Populate nodeHTTPAddr: every shard NodeID on physical pi maps to pi's
	// httptest server host so that redirect targets resolve correctly.
	for i, physID := range physIDs {
		u, _ := url.Parse(servers[i].URL)
		for gid := uint64(1); gid <= numShards; gid++ {
			nodeHTTPAddr[shardNodeID(gid, physID)] = u.Host
		}
	}

	// Start one RunTicker per Manager. Each drives all its shard nodes from a
	// single goroutine — the key multi-Raft efficiency gain.
	tickCtx, cancelTick := context.WithCancel(context.Background())
	for _, mgr := range mgrs {
		go mgr.RunTicker(tickCtx, time.Millisecond)
	}

	c := &shardCluster{
		t:            t,
		physIDs:      physIDs,
		numShards:    numShards,
		mgrs:         mgrs,
		sms:          smsByPhys,
		servers:      servers,
		net:          network,
		nodeHTTPAddr: nodeHTTPAddr,
		cancelTick:   cancelTick,
	}

	t.Cleanup(c.close)
	return c
}

func (c *shardCluster) close() {
	c.stopOnce.Do(func() {
		c.cancelTick()
		for _, srv := range c.servers {
			srv.Close()
		}
		for _, mgr := range c.mgrs {
			mgr.StopAll()
		}
	})
}

// waitAllLeaders polls until every shard on every physical node has a leader.
func (c *shardCluster) waitAllLeaders(timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allOK := true
		for gid := uint64(1); gid <= c.numShards; gid++ {
			hasLeader := false
			for _, mgr := range c.mgrs {
				node, err := mgr.Get(gid)
				if err != nil {
					continue
				}
				if node.StateSnapshot() == raft.Leader {
					hasLeader = true
					break
				}
			}
			if !hasLeader {
				allOK = false
				break
			}
		}
		if allOK {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatal("not all shards elected a leader within timeout")
}

// anyServerURL returns the URL of any physical node's HTTP server.
func (c *shardCluster) anyServerURL() string { return c.servers[0].URL }

// ---- HTTP helpers ----

func doPut(t *testing.T, url, value string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(value))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func doGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

func doDelete(t *testing.T, url string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// ---- Tests ----

func TestHTTP_PutGet(t *testing.T) {
	c := newShardCluster(t, testNumPhysical, testNumShards)
	c.waitAllLeaders(testElectionTimeout)

	base := c.anyServerURL()
	if code := doPut(t, base+"/keys/hello", "world"); code != http.StatusNoContent {
		t.Fatalf("PUT = %d, want 204", code)
	}
	if code, body := doGet(t, base+"/keys/hello"); code != http.StatusOK || body != "world" {
		t.Fatalf("GET = %d %q, want 200 world", code, body)
	}
}

func TestHTTP_GetMissing(t *testing.T) {
	c := newShardCluster(t, testNumPhysical, testNumShards)
	c.waitAllLeaders(testElectionTimeout)

	code, _ := doGet(t, c.anyServerURL()+"/keys/absent")
	if code != http.StatusNotFound {
		t.Fatalf("GET missing = %d, want 404", code)
	}
}

func TestHTTP_Overwrite(t *testing.T) {
	c := newShardCluster(t, testNumPhysical, testNumShards)
	c.waitAllLeaders(testElectionTimeout)

	base := c.anyServerURL()
	doPut(t, base+"/keys/k", "first")
	doPut(t, base+"/keys/k", "second")

	if _, body := doGet(t, base+"/keys/k"); body != "second" {
		t.Errorf("overwrite: body = %q, want second", body)
	}
}

func TestHTTP_Delete(t *testing.T) {
	c := newShardCluster(t, testNumPhysical, testNumShards)
	c.waitAllLeaders(testElectionTimeout)

	base := c.anyServerURL()
	doPut(t, base+"/keys/todelete", "v")
	if code := doDelete(t, base+"/keys/todelete"); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", code)
	}
	if code, _ := doGet(t, base+"/keys/todelete"); code != http.StatusNotFound {
		t.Fatalf("GET after delete = %d, want 404", code)
	}
}

func TestHTTP_DeleteMissing(t *testing.T) {
	c := newShardCluster(t, testNumPhysical, testNumShards)
	c.waitAllLeaders(testElectionTimeout)

	if code := doDelete(t, c.anyServerURL()+"/keys/nope"); code != http.StatusNotFound {
		t.Fatalf("DELETE missing = %d, want 404", code)
	}
}

// TestHTTP_MultiShard verifies that keys mapping to different shards coexist
// correctly: writes and reads across shards do not interfere with each other.
func TestHTTP_MultiShard(t *testing.T) {
	c := newShardCluster(t, testNumPhysical, testNumShards)
	c.waitAllLeaders(testElectionTimeout)

	base := c.anyServerURL()

	// Write a set of keys spread across shards.
	keys := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	for _, k := range keys {
		doPut(t, base+"/keys/"+k, "val-"+k)
	}

	// All reads must return the correct value.
	for _, k := range keys {
		code, body := doGet(t, base+"/keys/"+k)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", k, code)
			continue
		}
		if body != "val-"+k {
			t.Errorf("GET %s = %q, want val-%s", k, body, k)
		}
	}
}

// TestHTTP_ReadFromAnyNode verifies that linearizable GET succeeds even when
// directed at a node that is not the shard leader: ReadIndex ensures the
// follower's state machine is up-to-date before serving the read.
func TestHTTP_ReadFromAnyNode(t *testing.T) {
	c := newShardCluster(t, testNumPhysical, testNumShards)
	c.waitAllLeaders(testElectionTimeout)

	// Write via node 0.
	doPut(t, c.servers[0].URL+"/keys/shared", "value")

	// Read from every physical node and verify the value is visible everywhere.
	for i, srv := range c.servers {
		code, body := doGet(t, srv.URL+"/keys/shared")
		if code != http.StatusOK {
			t.Errorf("node %d GET = %d, want 200", i, code)
			continue
		}
		if body != "value" {
			t.Errorf("node %d GET = %q, want value", i, body)
		}
	}
}

// TestHTTP_NonLeaderWriteRedirects verifies that a PUT to a follower for the
// key's shard is redirected (307) to the shard leader. The Go HTTP client
// follows 307 transparently, so the write ultimately succeeds.
func TestHTTP_NonLeaderWriteRedirects(t *testing.T) {
	c := newShardCluster(t, testNumPhysical, testNumShards)
	c.waitAllLeaders(testElectionTimeout)

	// Send PUT to every physical node; at least one will be a follower for the
	// key's shard and must redirect. The default http.Client follows redirects,
	// so all should succeed.
	for i, srv := range c.servers {
		key := fmt.Sprintf("node%d-key", i)
		code := doPut(t, srv.URL+"/keys/"+key, "ok")
		if code != http.StatusNoContent {
			t.Errorf("PUT via node %d = %d, want 204", i, code)
		}
	}
}

// TestHTTP_ShardsStatus verifies that GET /shards returns one entry per shard.
func TestHTTP_ShardsStatus(t *testing.T) {
	c := newShardCluster(t, testNumPhysical, testNumShards)
	c.waitAllLeaders(testElectionTimeout)

	resp, err := http.Get(c.anyServerURL() + "/shards")
	if err != nil {
		t.Fatalf("GET /shards: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /shards = %d, want 200", resp.StatusCode)
	}

	var statuses []struct {
		GroupID     uint64 `json:"group_id"`
		State       string `json:"state"`
		Term        uint64 `json:"term"`
		LastApplied uint64 `json:"last_applied"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		t.Fatalf("decode /shards: %v", err)
	}
	if uint64(len(statuses)) != c.numShards {
		t.Errorf("len(statuses) = %d, want %d", len(statuses), c.numShards)
	}
	for _, s := range statuses {
		if s.GroupID == 0 {
			t.Errorf("status has zero GroupID: %+v", s)
		}
		if s.State == "" {
			t.Errorf("status has empty State: %+v", s)
		}
	}
}

// TestHTTP_Status verifies that GET /status returns the node's configuration.
func TestHTTP_Status(t *testing.T) {
	c := newShardCluster(t, testNumPhysical, testNumShards)

	resp, err := http.Get(c.servers[0].URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	var status struct {
		ID        string   `json:"id"`
		NumShards uint64   `json:"num_shards"`
		ShardIDs  []string `json:"shard_ids"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	if status.ID != "p1" {
		t.Errorf("id = %q, want p1", status.ID)
	}
	if status.NumShards != testNumShards {
		t.Errorf("num_shards = %d, want %d", status.NumShards, testNumShards)
	}
	if uint64(len(status.ShardIDs)) != testNumShards {
		t.Errorf("len(shard_ids) = %d, want %d", len(status.ShardIDs), testNumShards)
	}
}

// TestHTTP_SnapshotPreservesData verifies that an aggressive snapshot
// threshold (2 entries) does not lose committed key-value pairs.
func TestHTTP_SnapshotPreservesData(t *testing.T) {
	// Rebuild cluster with a very low snapshot threshold.
	network := memtransport.NewNetwork()
	physIDs := []string{"p1", "p2", "p3"}
	const numShards = uint64(2)

	mgrs := make([]*raft.Manager, len(physIDs))
	for i := range physIDs {
		mgrs[i] = raft.NewManager()
	}
	smsByPhys := make([]map[uint64]*KvSM, len(physIDs))
	for i := range physIDs {
		smsByPhys[i] = make(map[uint64]*KvSM, numShards)
	}
	nodeHTTPAddr := make(map[raft.NodeID]string)

	for gid := uint64(1); gid <= numShards; gid++ {
		allIDs := make([]raft.NodeID, len(physIDs))
		for i, pid := range physIDs {
			allIDs[i] = shardNodeID(gid, pid)
		}
		for i, pid := range physIDs {
			nodeID := shardNodeID(gid, pid)
			peers := make([]raft.NodeID, 0, len(physIDs)-1)
			for _, id := range allIDs {
				if id != nodeID {
					peers = append(peers, id)
				}
			}
			sm := &KvSM{data: make(map[string]string)}
			smsByPhys[i][gid] = sm

			cfg := raft.DefaultConfig()
			cfg.GroupID = gid
			cfg.ID = nodeID
			cfg.Peers = peers
			cfg.Storage = memstore.New()
			cfg.StateMachine = sm
			cfg.Transport = network.NewTransport(nodeID)
			cfg.TickInterval = 0
			cfg.SnapshotThreshold = 2 // trigger snapshots aggressively

			node, err := raft.New(&cfg)
			if err != nil {
				t.Fatalf("raft.New: %v", err)
			}
			network.Register(nodeID, node)
			if err := mgrs[i].Add(gid, node); err != nil {
				t.Fatalf("mgrs[%d].Add(shard %d): %v", i, gid, err)
			}
		}
	}
	for _, mgr := range mgrs {
		mgr.StartAll()
	}
	servers := make([]*httptest.Server, len(physIDs))
	for i, pid := range physIDs {
		servers[i] = httptest.NewServer(
			buildMux(pid, numShards, mgrs[i], smsByPhys[i], nodeHTTPAddr),
		)
	}
	for i, pid := range physIDs {
		u, _ := url.Parse(servers[i].URL)
		for gid := uint64(1); gid <= numShards; gid++ {
			nodeHTTPAddr[shardNodeID(gid, pid)] = u.Host
		}
	}
	tickCtx, cancel := context.WithCancel(context.Background())
	for _, mgr := range mgrs {
		go mgr.RunTicker(tickCtx, time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		for _, srv := range servers {
			srv.Close()
		}
		for _, mgr := range mgrs {
			mgr.StopAll()
		}
	})

	// Wait for leaders on all shards.
	deadline := time.Now().Add(testElectionTimeout)
	for time.Now().Before(deadline) {
		allOK := true
		for gid := uint64(1); gid <= numShards; gid++ {
			hasLeader := false
			for _, mgr := range mgrs {
				node, err := mgr.Get(gid)
				if err == nil && node.StateSnapshot() == raft.Leader {
					hasLeader = true
					break
				}
			}
			if !hasLeader {
				allOK = false
				break
			}
		}
		if allOK {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	base := servers[0].URL
	// Write enough entries to trigger several snapshot cycles.
	keys := []string{"snap-a", "snap-b", "snap-c", "snap-d", "snap-e", "snap-f"}
	for _, k := range keys {
		doPut(t, base+"/keys/"+k, "snap-"+k)
	}

	// All values must survive the snapshot cycles.
	for _, k := range keys {
		code, body := doGet(t, base+"/keys/"+k)
		if code != http.StatusOK || body != "snap-"+k {
			t.Errorf("after snapshots: GET %s = %d %q, want 200 snap-%s", k, code, body, k)
		}
	}
}
