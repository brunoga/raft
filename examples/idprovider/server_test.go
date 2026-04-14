package main

// Integration tests for the idprovider HTTP API.
//
// Each test spins up a 3-node (or 1-node for simpler cases) in-process Raft
// cluster using memtransport + memstore, wires up one httptest.Server per
// node, and drives Raft ticks from a background goroutine at 1 ms.
//
// The cluster uses TickInterval=0 (manual ticks) so the tests are independent
// of wall-clock scheduling, but still run in real time via the background
// ticker.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
	"net/url"
)

// testElectionTimeout is the maximum time WaitLeader will poll before giving up.
// It must be comfortably larger than the Raft election timeout (default 150–300 ms).
const testElectionTimeout = 3 * time.Second

// ---- Test cluster infrastructure ----

type idpCluster struct {
	t        *testing.T
	nodes    []*raft.Node
	sms      []*IDSM
	servers  []*httptest.Server // one HTTP server per Raft node
	net      *memtransport.Network
	httpMap  map[raft.NodeID]string
	stopOnce sync.Once
	stopCh   chan struct{}
}

// newIDPCluster creates an n-node idprovider cluster backed by in-process
// transports. opts are applied to every node's Config before New is called,
// allowing per-test overrides (e.g. SnapshotThreshold).
func newIDPCluster(t *testing.T, n int, opts ...func(*raft.Config)) *idpCluster { //nolint:unparam
	t.Helper()

	network := memtransport.NewNetwork()
	ids := make([]raft.NodeID, n)
	for i := range n {
		ids[i] = raft.NodeID(fmt.Sprintf("n%d", i+1))
	}

	c := &idpCluster{
		t:       t,
		nodes:   make([]*raft.Node, n),
		sms:     make([]*IDSM, n),
		servers: make([]*httptest.Server, n),
		net:     network,
		httpMap: make(map[raft.NodeID]string),
		stopCh:  make(chan struct{}),
	}

	for i, id := range ids {
		peers := make([]raft.NodeID, 0, n-1)
		for _, p := range ids {
			if p != id {
				peers = append(peers, p)
			}
		}

		sm := &IDSM{domains: make(map[string]uint64)}
		cfg := raft.DefaultConfig()
		cfg.ID = id
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = sm
		cfg.Transport = network.NewTransport(id)
		cfg.TickInterval = 0 // driven by background goroutine below
		for _, opt := range opts {
			opt(&cfg)
		}

		node, err := raft.New(cfg)
		if err != nil {
			t.Fatalf("raft.New(%s): %v", id, err)
		}
		network.Register(id, node)
		node.Start()

		c.nodes[i] = node
		c.sms[i] = sm
		c.servers[i] = httptest.NewServer(buildMux(string(id), node, sm, c.httpMap))
	}

	// Populate the httpMap so nodes can resolve each other's addresses for redirects.
	for i, id := range ids {
		u, _ := url.Parse(c.servers[i].URL)
		c.httpMap[id] = u.Host
	}

	// Background ticker: advance Raft logical time at 1 ms per tick.
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				for _, nd := range c.nodes {
					nd.Tick()
				}
			case <-c.stopCh:
				return
			}
		}
	}()

	t.Cleanup(c.close)
	return c
}

func (c *idpCluster) close() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	for _, srv := range c.servers {
		srv.Close()
	}
	for _, nd := range c.nodes {
		nd.Stop()
	}
}

// LeaderIdx polls until a leader is elected and returns its index in c.nodes.
func (c *idpCluster) LeaderIdx() int {
	c.t.Helper()
	deadline := time.Now().Add(testElectionTimeout)
	for time.Now().Before(deadline) {
		for i, nd := range c.nodes {
			if nd.StateSnapshot() == raft.Leader {
				return i
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatal("no leader elected within timeout")
	return -1
}

// LeaderURL returns the HTTP base URL of the current leader.
func (c *idpCluster) LeaderURL() string { return c.servers[c.LeaderIdx()].URL }

// FollowerIdx returns the index of any follower (not the leader).
func (c *idpCluster) FollowerIdx() int {
	c.t.Helper()
	leaderIdx := c.LeaderIdx()
	for i := range c.nodes {
		if i != leaderIdx {
			return i
		}
	}
	c.t.Fatal("no follower (single-node cluster?)")
	return -1
}

// FollowerURL returns the HTTP base URL of any follower.
func (c *idpCluster) FollowerURL() string { return c.servers[c.FollowerIdx()].URL }

// ---- HTTP helpers ----

// do sends a method+url request with optional headers, and returns the
// status code, response body, and headers.
func do(t *testing.T, method, url string, headers map[string]string) (code int, body []byte, hdr http.Header) {
	t.Helper()
	r, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, url, err)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func mustDecodeAlloc(t *testing.T, body []byte) allocResult {
	t.Helper()
	var r allocResult
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode allocResult: %v\nbody: %s", err, body)
	}
	return r
}

// ---- Domain management tests ----

func TestHTTP_CreateDomain(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	code, _, _ := do(t, http.MethodPost, url+"/domains/orders", nil)
	if code != http.StatusOK {
		t.Fatalf("POST /domains/orders = %d, want 200", code)
	}

	// Verify domain appears in GET /domains.
	code, body, _ := do(t, http.MethodGet, url+"/domains", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /domains = %d", code)
	}
	var domains map[string]uint64
	json.Unmarshal(body, &domains)
	if _, ok := domains["orders"]; !ok {
		t.Errorf("'orders' missing from GET /domains: %v", domains)
	}
}

func TestHTTP_CreateDomainIdempotent(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	// Create domain, allocate 1 ID to set counter, then create again.
	do(t, http.MethodPost, url+"/domains/d", nil)
	do(t, http.MethodPost, url+"/domains/d/next", nil)

	code, _, _ := do(t, http.MethodPost, url+"/domains/d", nil)
	if code != http.StatusOK {
		t.Fatalf("second POST /domains/d = %d, want 200", code)
	}

	// Counter must be unchanged.
	_, body, _ := do(t, http.MethodGet, url+"/domains/d/current", nil)
	var resp struct {
		Current uint64 `json:"current"`
	}
	json.Unmarshal(body, &resp)
	if resp.Current != 1 {
		t.Errorf("current after idempotent create = %d, want 1", resp.Current)
	}
}

func TestHTTP_DeleteDomain(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/tmp", nil)
	code, _, _ := do(t, http.MethodDelete, url+"/domains/tmp", nil)
	if code != http.StatusNoContent {
		t.Fatalf("DELETE /domains/tmp = %d, want 204", code)
	}

	// Allocating from a deleted domain must return 404.
	code, _, _ = do(t, http.MethodPost, url+"/domains/tmp/next", nil)
	if code != http.StatusNotFound {
		t.Errorf("POST /domains/tmp/next after delete = %d, want 404", code)
	}
}

func TestHTTP_DeleteNonExistentDomain(t *testing.T) {
	c := newIDPCluster(t, 3)

	code, _, _ := do(t, http.MethodDelete, c.LeaderURL()+"/domains/nope", nil)
	if code != http.StatusNotFound {
		t.Fatalf("DELETE /domains/nope = %d, want 404", code)
	}
}

func TestHTTP_ListDomains_Empty(t *testing.T) {
	c := newIDPCluster(t, 3)

	code, body, _ := do(t, http.MethodGet, c.LeaderURL()+"/domains", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /domains = %d", code)
	}
	var domains map[string]uint64
	json.Unmarshal(body, &domains)
	if len(domains) != 0 {
		t.Errorf("expected empty map, got %v", domains)
	}
}

func TestHTTP_ListDomains_ShowsAllCounters(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/a", nil)
	do(t, http.MethodPost, url+"/domains/b", nil)
	do(t, http.MethodPost, url+"/domains/a/next?count=3", nil)
	do(t, http.MethodPost, url+"/domains/b/next?count=7", nil)

	_, body, _ := do(t, http.MethodGet, url+"/domains", nil)
	var domains map[string]uint64
	json.Unmarshal(body, &domains)
	if domains["a"] != 3 {
		t.Errorf("a = %d, want 3", domains["a"])
	}
	if domains["b"] != 7 {
		t.Errorf("b = %d, want 7", domains["b"])
	}
}

// ---- Allocation tests ----

func TestHTTP_AllocSingleID(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/ids", nil)
	code, body, _ := do(t, http.MethodPost, url+"/domains/ids/next", nil)
	if code != http.StatusOK {
		t.Fatalf("POST /domains/ids/next = %d\n%s", code, body)
	}

	r := mustDecodeAlloc(t, body)
	if r.Start != 1 || r.Count != 1 {
		t.Errorf("alloc = {start:%d count:%d}, want {1 1}", r.Start, r.Count)
	}
}

func TestHTTP_AllocBatch(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/batch", nil)
	_, body, _ := do(t, http.MethodPost, url+"/domains/batch/next?count=100", nil)

	r := mustDecodeAlloc(t, body)
	if r.Start != 1 || r.Count != 100 {
		t.Errorf("batch alloc = {%d %d}, want {1 100}", r.Start, r.Count)
	}
}

func TestHTTP_AllocSequentialRangesAreContiguous(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/seq", nil)
	_, b1, _ := do(t, http.MethodPost, url+"/domains/seq/next?count=10", nil)
	_, b2, _ := do(t, http.MethodPost, url+"/domains/seq/next?count=5", nil)

	r1 := mustDecodeAlloc(t, b1)
	r2 := mustDecodeAlloc(t, b2)
	if r1.Start+r1.Count != r2.Start {
		t.Errorf("ranges not contiguous: [%d,%d) then [%d,%d)",
			r1.Start, r1.Start+r1.Count, r2.Start, r2.Start+r2.Count)
	}
}

func TestHTTP_AllocNonExistentDomain(t *testing.T) {
	c := newIDPCluster(t, 3)

	code, _, _ := do(t, http.MethodPost, c.LeaderURL()+"/domains/ghost/next", nil)
	if code != http.StatusNotFound {
		t.Fatalf("POST /domains/ghost/next = %d, want 404", code)
	}
}

func TestHTTP_AllocBadCount(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/x", nil)
	for _, bad := range []string{"0", "-1", "abc"} {
		code, _, _ := do(t, http.MethodPost, url+"/domains/x/next?count="+bad, nil)
		if code != http.StatusBadRequest {
			t.Errorf("count=%q: got %d, want 400", bad, code)
		}
	}
}

func TestHTTP_AllocProposeOnce_Idempotent(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/idem", nil)

	hdrs := map[string]string{"X-Client-ID": "svc-1", "X-Seq-Num": "42"}

	_, b1, _ := do(t, http.MethodPost, url+"/domains/idem/next", hdrs)
	_, b2, _ := do(t, http.MethodPost, url+"/domains/idem/next", hdrs) // retry

	r1 := mustDecodeAlloc(t, b1)
	r2 := mustDecodeAlloc(t, b2)
	if r1 != r2 {
		t.Errorf("idempotent retry: first=%+v second=%+v, want equal", r1, r2)
	}
}

func TestHTTP_AllocProposeOnce_NewSeqNum(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/seq2", nil)

	_, b1, _ := do(t, http.MethodPost, url+"/domains/seq2/next",
		map[string]string{"X-Client-ID": "c", "X-Seq-Num": "1"})
	_, b2, _ := do(t, http.MethodPost, url+"/domains/seq2/next",
		map[string]string{"X-Client-ID": "c", "X-Seq-Num": "2"})

	r1 := mustDecodeAlloc(t, b1)
	r2 := mustDecodeAlloc(t, b2)
	if r1.Start+r1.Count != r2.Start {
		t.Errorf("ranges from seq 1,2 not contiguous: %+v then %+v", r1, r2)
	}
}

func TestHTTP_AllocBadSeqNum(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/x", nil)
	code, _, _ := do(t, http.MethodPost, url+"/domains/x/next", map[string]string{
		"X-Client-ID": "svc",
		"X-Seq-Num":   "not-a-number",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid X-Seq-Num = %d, want 400", code)
	}
}

func TestHTTP_AllocObsoleteSeqNum(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/obs", nil)

	// Issue seq=2 first, then retry seq=1 (obsolete).
	do(t, http.MethodPost, url+"/domains/obs/next",
		map[string]string{"X-Client-ID": "c", "X-Seq-Num": "2"})

	// The raft layer rejects obsolete seqNums — the handler propagates the error.
	// The SM does not apply the command, so the response is an error (non-2xx).
	// ErrObsoleteSeqNum is not NotLeaderError, so we expect 500.
	code, _, _ := do(t, http.MethodPost, url+"/domains/obs/next",
		map[string]string{"X-Client-ID": "c", "X-Seq-Num": "1"})
	if code == http.StatusOK {
		t.Fatal("expected non-200 for obsolete seqNum")
	}
}

// ---- Current (linearizable read) tests ----

func TestHTTP_GetCurrentDomain(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/cur", nil)
	do(t, http.MethodPost, url+"/domains/cur/next?count=7", nil)

	code, body, _ := do(t, http.MethodGet, url+"/domains/cur/current", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /domains/cur/current = %d\n%s", code, body)
	}

	var resp struct {
		Domain  string `json:"domain"`
		Current uint64 `json:"current"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if resp.Domain != "cur" {
		t.Errorf("domain = %q, want cur", resp.Domain)
	}
	if resp.Current != 7 {
		t.Errorf("current = %d, want 7", resp.Current)
	}
}

func TestHTTP_GetCurrentNonExistentDomain(t *testing.T) {
	c := newIDPCluster(t, 3)

	code, _, _ := do(t, http.MethodGet, c.LeaderURL()+"/domains/ghost/current", nil)
	if code != http.StatusNotFound {
		t.Fatalf("GET /domains/ghost/current = %d, want 404", code)
	}
}

func TestHTTP_GetCurrentReflectsLatestAlloc(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/live", nil)

	for i := range 5 {
		do(t, http.MethodPost, fmt.Sprintf(url+"/domains/live/next?count=%d", i+1), nil)
	}
	// sum(1..5) = 15
	_, body, _ := do(t, http.MethodGet, url+"/domains/live/current", nil)
	var resp struct{ Current uint64 `json:"current"` }
	json.Unmarshal(body, &resp)
	if resp.Current != 15 {
		t.Errorf("current = %d, want 15", resp.Current)
	}
}

// ---- Status test ----

func TestHTTP_Status(t *testing.T) {
	c := newIDPCluster(t, 3)
	leaderIdx := c.LeaderIdx()

	code, body, _ := do(t, http.MethodGet, c.servers[leaderIdx].URL+"/status", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /status = %d", code)
	}

	var status struct {
		ID          string `json:"id"`
		State       string `json:"state"`
		Leader      string `json:"leader"`
		LastApplied uint64 `json:"last_applied"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decode status: %v\n%s", err, body)
	}
	if status.State != "Leader" {
		t.Errorf("state = %q, want Leader", status.State)
	}
	if status.Leader == "" {
		t.Error("leader field should be non-empty on the leader")
	}
	wantID := fmt.Sprintf("n%d", leaderIdx+1)
	if status.ID != wantID {
		t.Errorf("id = %q, want %q", status.ID, wantID)
	}
}

// ---- Non-leader / routing tests ----

func TestHTTP_NonLeaderProposalRedirectsToLeader(t *testing.T) {
	c := newIDPCluster(t, 3)
	leaderIdx := c.LeaderIdx()
	followerIdx := (leaderIdx + 1) % 3

	// We use a custom client that does NOT follow redirects so we can verify the 307.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	targetURL := c.servers[followerIdx].URL + "/domains/redirect-test"
	resp, err := client.Post(targetURL, "application/json", nil)
	if err != nil {
		t.Fatalf("POST to follower: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("POST to follower = %d, want 307", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("Location header missing on 307")
	}

	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}

	wantHost, _ := url.Parse(c.servers[leaderIdx].URL)
	if u.Host != wantHost.Host {
		t.Errorf("Location host = %q, want %q", u.Host, wantHost.Host)
	}
}

func TestHTTP_FollowerReadIndexForwardedToLeader(t *testing.T) {
	c := newIDPCluster(t, 3)
	leaderURL := c.LeaderURL()

	// Create domain + alloc via leader.
	do(t, http.MethodPost, leaderURL+"/domains/shared", nil)
	do(t, http.MethodPost, leaderURL+"/domains/shared/next?count=42", nil)

	// GET /domains on a follower:
	//   ReadIndex is forwarded to the leader, WaitApplied blocks until the
	//   follower has applied that index, then the SM read is safe.
	followerURL := c.FollowerURL()
	code, body, _ := do(t, http.MethodGet, followerURL+"/domains", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /domains on follower = %d\n%s", code, body)
	}
	var domains map[string]uint64
	if err := json.Unmarshal(body, &domains); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if domains["shared"] != 42 {
		t.Errorf("follower domains[shared] = %d, want 42", domains["shared"])
	}
}

func TestHTTP_FollowerCurrentForwardedToLeader(t *testing.T) {
	c := newIDPCluster(t, 3)
	leaderURL := c.LeaderURL()

	do(t, http.MethodPost, leaderURL+"/domains/fwd", nil)
	do(t, http.MethodPost, leaderURL+"/domains/fwd/next?count=99", nil)

	followerURL := c.FollowerURL()
	code, body, _ := do(t, http.MethodGet, followerURL+"/domains/fwd/current", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /domains/fwd/current on follower = %d\n%s", code, body)
	}
	var resp struct{ Current uint64 `json:"current"` }
	json.Unmarshal(body, &resp)
	if resp.Current != 99 {
		t.Errorf("follower current = %d, want 99", resp.Current)
	}
}

// ---- Multi-domain isolation ----

func TestHTTP_MultiDomainIsolation(t *testing.T) {
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/alpha", nil)
	do(t, http.MethodPost, url+"/domains/beta", nil)
	do(t, http.MethodPost, url+"/domains/alpha/next?count=100", nil)
	do(t, http.MethodPost, url+"/domains/beta/next?count=5", nil)

	var ra, rb struct{ Current uint64 `json:"current"` }
	_, ba, _ := do(t, http.MethodGet, url+"/domains/alpha/current", nil)
	_, bb, _ := do(t, http.MethodGet, url+"/domains/beta/current", nil)
	json.Unmarshal(ba, &ra)
	json.Unmarshal(bb, &rb)

	if ra.Current != 100 {
		t.Errorf("alpha = %d, want 100", ra.Current)
	}
	if rb.Current != 5 {
		t.Errorf("beta = %d, want 5", rb.Current)
	}
}

func TestHTTP_MultiDomainAllocationsDontInterfere(t *testing.T) {
	// Interleave allocs across two domains and verify ranges are independent.
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/x", nil)
	do(t, http.MethodPost, url+"/domains/y", nil)

	type rangeSet [][2]uint64 // each element is [start, end)
	collectRanges := func(domain string, counts []uint64) rangeSet {
		var rs rangeSet
		for _, cnt := range counts {
			_, body, _ := do(t, http.MethodPost,
				fmt.Sprintf(url+"/domains/%s/next?count=%d", domain, cnt), nil)
			r := mustDecodeAlloc(t, body)
			rs = append(rs, [2]uint64{r.Start, r.Start + r.Count})
		}
		return rs
	}

	xRanges := collectRanges("x", []uint64{3, 2, 5})
	yRanges := collectRanges("y", []uint64{1, 4, 2})

	// Verify x ranges are contiguous and start at 1.
	if xRanges[0][0] != 1 {
		t.Errorf("x first range starts at %d, want 1", xRanges[0][0])
	}
	for i := 1; i < len(xRanges); i++ {
		if xRanges[i-1][1] != xRanges[i][0] {
			t.Errorf("x ranges have gap between %v and %v", xRanges[i-1], xRanges[i])
		}
	}
	// Verify y ranges are contiguous and start at 1.
	if yRanges[0][0] != 1 {
		t.Errorf("y first range starts at %d, want 1", yRanges[0][0])
	}
	for i := 1; i < len(yRanges); i++ {
		if yRanges[i-1][1] != yRanges[i][0] {
			t.Errorf("y ranges have gap between %v and %v", yRanges[i-1], yRanges[i])
		}
	}
}

// ---- Snapshot persistence ----

func TestHTTP_SnapshotPreservesDomains(t *testing.T) {
	// Use SnapshotThreshold=2 to trigger snapshots aggressively.
	c := newIDPCluster(t, 3, func(cfg *raft.Config) {
		cfg.SnapshotThreshold = 2
	})
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/snap", nil)

	// Commit 6 allocs — well past the snapshot threshold.
	want := uint64(0)
	for i := uint64(1); i <= 6; i++ {
		do(t, http.MethodPost, fmt.Sprintf(url+"/domains/snap/next?count=%d", i), nil)
		want += i // 1+2+3+4+5+6 = 21
	}

	// After snapshots, the counter must be correct and readable.
	code, body, _ := do(t, http.MethodGet, url+"/domains/snap/current", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /domains/snap/current = %d\n%s", code, body)
	}
	var resp struct{ Current uint64 `json:"current"` }
	json.Unmarshal(body, &resp)
	if resp.Current != want {
		t.Errorf("after snapshots: current = %d, want %d", resp.Current, want)
	}
}

func TestHTTP_SnapshotPreservesMultipleDomains(t *testing.T) {
	c := newIDPCluster(t, 3, func(cfg *raft.Config) {
		cfg.SnapshotThreshold = 3
	})
	url := c.LeaderURL()

	// Create two domains and alloc into both, forcing snapshots.
	do(t, http.MethodPost, url+"/domains/m1", nil)
	do(t, http.MethodPost, url+"/domains/m2", nil)
	for i := uint64(1); i <= 4; i++ {
		do(t, http.MethodPost, fmt.Sprintf(url+"/domains/m1/next?count=%d", i), nil)
		do(t, http.MethodPost, fmt.Sprintf(url+"/domains/m2/next?count=%d", i*10), nil)
	}
	// m1 counter: 1+2+3+4 = 10; m2: 10+20+30+40 = 100

	_, body, _ := do(t, http.MethodGet, url+"/domains", nil)
	var domains map[string]uint64
	json.Unmarshal(body, &domains)
	if domains["m1"] != 10 {
		t.Errorf("m1 = %d, want 10", domains["m1"])
	}
	if domains["m2"] != 100 {
		t.Errorf("m2 = %d, want 100", domains["m2"])
	}
}

// ---- CreateDomain ProposeOnce idempotency ----

func TestHTTP_CreateDomainProposeOnce(t *testing.T) {
	// Create must be idempotent at the SM level AND at the ProposeOnce level.
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	hdrs := map[string]string{"X-Client-ID": "admin", "X-Seq-Num": "1"}

	code1, _, _ := do(t, http.MethodPost, url+"/domains/idem", hdrs)
	code2, _, _ := do(t, http.MethodPost, url+"/domains/idem", hdrs) // same seqNum
	if code1 != http.StatusOK || code2 != http.StatusOK {
		t.Fatalf("create codes = %d, %d; want 200, 200", code1, code2)
	}

	// Verify domain exists exactly once.
	_, body, _ := do(t, http.MethodGet, url+"/domains", nil)
	var domains map[string]uint64
	json.Unmarshal(body, &domains)
	if _, ok := domains["idem"]; !ok {
		t.Error("domain 'idem' should exist")
	}
}

// ---- DeleteDomain ProposeOnce idempotency ----

func TestHTTP_DeleteDomainProposeOnce(t *testing.T) {
	// A delete replayed with the same (clientID, seqNum) must return the
	// cached result rather than re-running the delete (which would 404).
	c := newIDPCluster(t, 3)
	url := c.LeaderURL()

	do(t, http.MethodPost, url+"/domains/del", nil)

	hdrs := map[string]string{"X-Client-ID": "admin", "X-Seq-Num": "10"}
	code1, _, _ := do(t, http.MethodDelete, url+"/domains/del", hdrs)
	code2, _, _ := do(t, http.MethodDelete, url+"/domains/del", hdrs)
	if code1 != http.StatusNoContent {
		t.Fatalf("first delete = %d, want 204", code1)
	}
	if code2 != http.StatusNoContent {
		t.Fatalf("ProposeOnce retry of delete = %d, want 204 (cached)", code2)
	}
}

// ---- Errors in SM returned as non-2xx ----

func TestHTTP_InternalErrorPropagated(t *testing.T) {
	// Verify that an SM error for an unknown domain results in a non-2xx response.
	// (ErrDomainNotFound is already tested above; this is a belt-and-suspenders check.)
	c := newIDPCluster(t, 1) // single-node for speed

	code, _, _ := do(t, http.MethodPost, c.LeaderURL()+"/domains/nonexist/next", nil)
	if code == http.StatusOK {
		t.Error("alloc on non-existent domain should not return 200")
	}

	// errors.Is is not available over HTTP; check that it is some client error.
	if code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", code)
	}
}

// ---- helper: verify ErrDomainNotFound is wrapped correctly ----

func TestErrDomainNotFound_IsComparable(t *testing.T) {
	// ErrDomainNotFound must survive errors.Is wrapping.
	wrapped := fmt.Errorf("outer: %w", ErrDomainNotFound)
	if !errors.Is(wrapped, ErrDomainNotFound) {
		t.Error("errors.Is(wrapped, ErrDomainNotFound) should be true")
	}
}
