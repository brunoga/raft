package main

// Tests for the discovery integration in idprovider:
//   - raftPeerAdder calls both the transport AddPeer and node.AddServer
//   - calling AddPeer on a follower (AddServer returns NotLeaderError) does not panic
//   - a late-joining node is added to Raft membership via raftPeerAdder
//   - a DiscoveryAgent wired to a raftPeerAdder propagates a discovered peer end-to-end

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/discovery"
	"github.com/brunoga/raft/discovery/dnsdiscovery"
	"github.com/brunoga/raft/storage/memstore"
)

// ---- helpers ---------------------------------------------------------------

// fakePeerAdder records AddPeer calls and satisfies discovery.PeerAdder.
type fakePeerAdder struct {
	mu    sync.Mutex
	peers map[raft.NodeID]string
}

func newFakePeerAdder() *fakePeerAdder {
	return &fakePeerAdder{peers: make(map[raft.NodeID]string)}
}

func (f *fakePeerAdder) AddPeer(id raft.NodeID, addr string) {
	f.mu.Lock()
	f.peers[id] = addr
	f.mu.Unlock()
}

func (f *fakePeerAdder) get(id raft.NodeID) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.peers[id]
	return v, ok
}

// lateJoiner adds a new Raft node to an existing idpCluster's in-memory
// network WITHOUT adding it to the initial peer lists of the existing nodes.
// It returns the new node; the caller is responsible for stopping it.
func lateJoiner(t *testing.T, c *idpCluster, id raft.NodeID, knownPeers []raft.NodeID) *raft.Node {
	t.Helper()
	sm := &IDSM{domains: make(map[string]uint64)}
	cfg := raft.DefaultConfig()
	cfg.ID = id
	cfg.Peers = knownPeers
	cfg.Storage = memstore.New()
	cfg.StateMachine = sm
	cfg.Transport = c.net.NewTransport(id)
	cfg.TickInterval = 0
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New(%s): %v", id, err)
	}
	c.net.Register(id, node)
	node.Start()
	return node
}

// waitLeaderKnown polls until node.Leader() returns a non-empty NodeID or the
// deadline expires.
func waitLeaderKnown(t *testing.T, node *raft.Node, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if node.Leader() != "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("node %s: leader never became known within %s", node.ID(), timeout)
}

// ---- raftPeerAdder unit tests ----------------------------------------------

// TestRaftPeerAdder_CallsTransportAddPeer verifies that raftPeerAdder.AddPeer
// always forwards the call to the underlying transport PeerAdder.
func TestRaftPeerAdder_CallsTransportAddPeer(t *testing.T) {
	c := newIDPCluster(t, 1)
	_ = c.LeaderIdx() // ensure a leader is elected

	ft := newFakePeerAdder()
	adder := &raftPeerAdder{tr: ft, node: c.nodes[0]}

	adder.AddPeer("n99", "192.0.2.1:7099")

	if addr, ok := ft.get("n99"); !ok || addr != "192.0.2.1:7099" {
		t.Errorf("transport.AddPeer: got %q ok=%v, want 192.0.2.1:7099 true", addr, ok)
	}
}

// TestRaftPeerAdder_AddsToMembership verifies that raftPeerAdder.AddPeer
// proposes adding the peer to Raft membership when called on the leader, so
// the new node eventually receives heartbeats and learns who the leader is.
//
// We start with a 2-node cluster (n1, n2) and add n3 as a late joiner.
// Using 2→3 rather than 1→2 avoids a check-quorum edge case: with 2 existing
// nodes already heartbeating each other, the leader continues to satisfy the
// check-quorum requirement (n1+n2 = majority of both C_old={n1,n2} and
// C_new={n1,n2,n3}) while n3 is catching up.
func TestRaftPeerAdder_AddsToMembership(t *testing.T) {
	c := newIDPCluster(t, 2)
	leaderIdx := c.LeaderIdx()

	// n3 starts knowing about n1+n2 but is not in their membership yet.
	existingIDs := []raft.NodeID{c.nodes[0].ID(), c.nodes[1].ID()}
	n3 := lateJoiner(t, c, "n3-member", existingIDs)
	defer n3.Stop()

	// Simulate what DiscoveryAgent fires on the leader.
	adder := &raftPeerAdder{tr: newFakePeerAdder(), node: c.nodes[leaderIdx]}
	adder.AddPeer("n3-member", "unused:0") // addr irrelevant for memtransport

	// n3 must receive heartbeats and learn the leader's identity.
	waitLeaderKnown(t, n3, testElectionTimeout)
}

// TestRaftPeerAdder_NonLeaderBestEffort verifies that calling AddPeer on a
// follower does not panic or return an error to the caller. AddServer on a
// follower returns NotLeaderError, which is intentionally ignored.
func TestRaftPeerAdder_NonLeaderBestEffort(t *testing.T) {
	c := newIDPCluster(t, 3)
	followerIdx := c.FollowerIdx()

	adder := &raftPeerAdder{tr: newFakePeerAdder(), node: c.nodes[followerIdx]}
	// Must not panic. The AddServer call will silently fail with NotLeaderError.
	adder.AddPeer("ghost", "unused:0")
}

// ---- DiscoveryAgent end-to-end tests ---------------------------------------

// fakeDiscovery is a Discovery implementation that returns a static peer list.
type fakeDiscovery struct {
	peers []discovery.PeerInfo
}

func (f *fakeDiscovery) Discover(_ context.Context) ([]discovery.PeerInfo, error) {
	return f.peers, nil
}

// TestDiscoveryAgent_LateJoinerJoinsCluster is an end-to-end test: a
// DiscoveryAgent wired to a raftPeerAdder causes a late-joining node to be
// added to the Raft membership and start receiving heartbeats.
func TestDiscoveryAgent_LateJoinerJoinsCluster(t *testing.T) {
	// Start a 2-node cluster (n1, n2).
	c := newIDPCluster(t, 2)
	leaderIdx := c.LeaderIdx()

	// n3 starts with n1+n2 as known peers but is not in their membership.
	existingIDs := make([]raft.NodeID, len(c.nodes))
	for i, nd := range c.nodes {
		existingIDs[i] = nd.ID()
	}
	n3 := lateJoiner(t, c, "n3-late", existingIDs)
	defer n3.Stop()

	// Wire a DiscoveryAgent on the leader: it discovers n3 and calls AddPeer.
	d := &fakeDiscovery{peers: []discovery.PeerInfo{{ID: "n3-late", Addr: "unused:0"}}}
	adder := &raftPeerAdder{tr: newFakePeerAdder(), node: c.nodes[leaderIdx]}
	agent := discovery.NewAgent(d, adder, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), testElectionTimeout)
	defer cancel()
	go func() { _ = agent.Run(ctx) }() //nolint:errcheck // Run returns ctx.Err(); unactionable here.

	// n3 should eventually receive heartbeats and learn the leader.
	waitLeaderKnown(t, n3, testElectionTimeout)
}

// ---- DNS A-record discovery integration test --------------------------------

// fakeResolver implements dnsdiscovery.Resolver using a fixed IP list.
// It satisfies both LookupHost and LookupSRV so that it can be used for
// A-record tests without requiring real DNS I/O.
type fakeResolver struct {
	ips []string
}

func (r *fakeResolver) LookupHost(_ context.Context, _ string) ([]string, error) {
	return r.ips, nil
}

func (r *fakeResolver) LookupSRV(_ context.Context, _, _, _ string) (string, []*net.SRV, error) {
	return "", nil, nil
}

// TestDNSDiscovery_ThreeNodeCluster is a 3-node end-to-end integration test for
// DNS A-record peer discovery. A fake DNS resolver returns three IP addresses;
// a NodeIDMapper translates each IP to the corresponding Raft node ID. A
// DiscoveryAgent wired to a raftPeerAdder on the leader discovers the late
// joiner and adds it to the Raft membership, after which the late joiner
// receives heartbeats and learns who the leader is.
func TestDNSDiscovery_ThreeNodeCluster(t *testing.T) {
	// Start a 2-node cluster with IDs "n1" and "n2".
	c := newIDPCluster(t, 2)
	leaderIdx := c.LeaderIdx()

	// n3 starts knowing about n1 and n2 but is not in their membership yet.
	existingIDs := []raft.NodeID{c.nodes[0].ID(), c.nodes[1].ID()}
	n3 := lateJoiner(t, c, "n3-dns", existingIDs)
	defer n3.Stop()

	// A fake DNS resolver returns 3 IPs. The mapper translates each IP to the
	// corresponding node ID — mirroring what a Kubernetes operator would do when
	// pod names are predictable (e.g. from a StatefulSet).
	ipToID := map[string]raft.NodeID{
		"10.0.0.1": "n1",
		"10.0.0.2": "n2",
		"10.0.0.3": "n3-dns",
	}
	resolver := &fakeResolver{ips: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}}
	dns := dnsdiscovery.NewARecord(
		"idprovider-raft.default.svc.cluster.local",
		"7001",
		func(ip string) (raft.NodeID, bool) {
			id, ok := ipToID[ip]
			return id, ok
		},
		resolver,
	)

	// Wire a DiscoveryAgent on the leader. The transport PeerAdder is a
	// fakePeerAdder (memtransport routes by NodeID, not address); the Raft
	// membership addition goes through raftPeerAdder.AddPeer → node.AddServer.
	adder := &raftPeerAdder{tr: newFakePeerAdder(), node: c.nodes[leaderIdx]}
	agent := discovery.NewAgent(dns, adder, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), testElectionTimeout)
	defer cancel()
	go func() { _ = agent.Run(ctx) }() //nolint:errcheck // Run returns ctx.Err(); unactionable here.

	// n3 must receive heartbeats and learn who the leader is.
	waitLeaderKnown(t, n3, testElectionTimeout)
}

// TestDiscoveryAgent_MultipleNewPeers verifies that the agent handles
// discovering several new peers in one poll: all of them end up in membership.
func TestDiscoveryAgent_MultipleNewPeers(t *testing.T) {
	c := newIDPCluster(t, 1)
	leaderIdx := c.LeaderIdx()

	const numLate = 3
	lateNodes := make([]*raft.Node, numLate)
	discoveryPeers := make([]discovery.PeerInfo, numLate)
	for i := range numLate {
		id := raft.NodeID(fmt.Sprintf("late-%d", i+1))
		lateNodes[i] = lateJoiner(t, c, id, []raft.NodeID{c.nodes[0].ID()})
		t.Cleanup(lateNodes[i].Stop)
		discoveryPeers[i] = discovery.PeerInfo{ID: id, Addr: "unused:0"}
	}

	d := &fakeDiscovery{peers: discoveryPeers}
	adder := &raftPeerAdder{tr: newFakePeerAdder(), node: c.nodes[leaderIdx]}
	agent := discovery.NewAgent(d, adder, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), testElectionTimeout)
	defer cancel()
	go func() { _ = agent.Run(ctx) }() //nolint:errcheck // Run returns ctx.Err(); unactionable here.

	// All late nodes must eventually learn the leader.
	for _, nd := range lateNodes {
		waitLeaderKnown(t, nd, testElectionTimeout)
	}
}
