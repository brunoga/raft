package discovery_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/discovery"
)

// ---- Static ----------------------------------------------------------------

func TestStaticDiscovery_ReturnsConfigured(t *testing.T) {
	peers := []discovery.PeerInfo{
		{ID: "n1", Addr: "localhost:7001"},
		{ID: "n2", Addr: "localhost:7002"},
	}
	d := discovery.Static(peers)
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != len(peers) {
		t.Fatalf("got %d peers, want %d", len(got), len(peers))
	}
	for i, p := range peers {
		if got[i] != p {
			t.Errorf("peers[%d]: got %+v, want %+v", i, got[i], p)
		}
	}
}

func TestStaticDiscovery_EmptyList(t *testing.T) {
	d := discovery.Static(nil)
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestStaticDiscovery_MutationSafe(t *testing.T) {
	// Mutating the original slice after construction must not affect the result.
	peers := []discovery.PeerInfo{{ID: "n1", Addr: "a:1"}}
	d := discovery.Static(peers)
	peers[0].Addr = "mutated"
	got, _ := d.Discover(context.Background())
	if got[0].Addr == "mutated" {
		t.Fatal("Static did not copy peers slice")
	}
}

// ---- DiscoveryAgent --------------------------------------------------------

// fakePeerAdder records AddPeer calls.
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

func (f *fakePeerAdder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.peers)
}

func TestDiscoveryAgent_AddsPeersOnDiscovery(t *testing.T) {
	peers := []discovery.PeerInfo{
		{ID: "n1", Addr: "h1:7001"},
		{ID: "n2", Addr: "h2:7002"},
	}
	adder := newFakePeerAdder()
	agent := discovery.NewAgent(discovery.Static(peers), adder, 0)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Run returns when ctx is done; we only care that the seed happened.
	go agent.Run(ctx) //nolint:errcheck // Run returns ctx.Err(); unactionable in a test goroutine.

	// Give the agent a moment to complete the initial seed.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if adder.count() == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	for _, p := range peers {
		if addr, ok := adder.get(p.ID); !ok || addr != p.Addr {
			t.Errorf("peer %s: got %q ok=%v, want %q", p.ID, addr, ok, p.Addr)
		}
	}
}

func TestDiscoveryAgent_SkipsKnownPeers(t *testing.T) {
	// AddPeer is idempotent in GRPCTransport; calling it twice is fine.
	// This test verifies the agent calls AddPeer on every poll (idempotency is
	// the transport's responsibility, not the agent's).
	var calls atomic.Int64
	counter := &callCountAdder{fn: func() { calls.Add(1) }}
	peers := []discovery.PeerInfo{{ID: "n1", Addr: "h1:7001"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agent := discovery.NewAgent(discovery.Static(peers), counter, 10*time.Millisecond)
	go agent.Run(ctx) //nolint:errcheck // Run returns ctx.Err(); unactionable in a test goroutine.

	time.Sleep(50 * time.Millisecond)
	cancel()

	if calls.Load() < 2 {
		t.Errorf("expected at least 2 AddPeer calls (seed + ≥1 poll), got %d", calls.Load())
	}
}

func TestDiscoveryAgent_HandlesTransientError(t *testing.T) {
	// First call returns an error; second call succeeds.
	var mu sync.Mutex
	attempt := 0
	d := &funcDiscovery{fn: func(ctx context.Context) ([]discovery.PeerInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		attempt++
		if attempt == 1 {
			return nil, errors.New("transient network error")
		}
		return []discovery.PeerInfo{{ID: "n1", Addr: "h1:7001"}}, nil
	}}

	adder := newFakePeerAdder()
	agent := discovery.NewAgent(d, adder, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go agent.Run(ctx) //nolint:errcheck // Run returns ctx.Err(); unactionable in a test goroutine.

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := adder.get("n1"); ok {
			return // success
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("peer was never added after transient error cleared")
}

// ---- helpers ---------------------------------------------------------------

type callCountAdder struct {
	fn func()
}

func (c *callCountAdder) AddPeer(_ raft.NodeID, _ string) { c.fn() }

type funcDiscovery struct {
	fn func(context.Context) ([]discovery.PeerInfo, error)
}

func (f *funcDiscovery) Discover(ctx context.Context) ([]discovery.PeerInfo, error) {
	return f.fn(ctx)
}
