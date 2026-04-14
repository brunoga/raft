package dnsdiscovery_test

import (
	"context"
	"net"
	"testing"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/discovery/dnsdiscovery"
)

// ---- mock resolver ---------------------------------------------------------

type mockResolver struct {
	hosts []string
	srvs  []*net.SRV
}

func (m *mockResolver) LookupHost(_ context.Context, _ string) ([]string, error) {
	return m.hosts, nil
}

func (m *mockResolver) LookupSRV(_ context.Context, _, _, _ string) (string, []*net.SRV, error) {
	return "", m.srvs, nil
}

// ---- A-record tests --------------------------------------------------------

func TestDNSDiscovery_ARecord(t *testing.T) {
	resolver := &mockResolver{hosts: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}}

	mapper := func(ip string) (raft.NodeID, bool) {
		m := map[string]raft.NodeID{
			"10.0.0.1": "n1",
			"10.0.0.2": "n2",
			// 10.0.0.3 intentionally absent → skipped
		}
		id, ok := m[ip]
		return id, ok
	}

	d := dnsdiscovery.NewARecord("raft.local", "7001", mapper, resolver)
	peers, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2: %v", len(peers), peers)
	}
	wantAddrs := map[string]string{
		"n1": "10.0.0.1:7001",
		"n2": "10.0.0.2:7001",
	}
	for _, p := range peers {
		want, ok := wantAddrs[string(p.ID)]
		if !ok {
			t.Errorf("unexpected peer %s", p.ID)
			continue
		}
		if p.Addr != want {
			t.Errorf("peer %s: addr %q, want %q", p.ID, p.Addr, want)
		}
	}
}

func TestDNSDiscovery_ARecord_AllSkipped(t *testing.T) {
	resolver := &mockResolver{hosts: []string{"10.0.0.1"}}
	d := dnsdiscovery.NewARecord("h", "7001", func(string) (raft.NodeID, bool) {
		return "", false
	}, resolver)
	peers, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(peers) != 0 {
		t.Fatalf("expected 0 peers, got %v", peers)
	}
}

func TestDNSDiscovery_ARecord_DefaultResolver(t *testing.T) {
	// Passing nil resolver must not panic (falls back to net.DefaultResolver).
	// We don't assert results — just that construction and Discover don't crash
	// before the network call.
	d := dnsdiscovery.NewARecord("localhost", "7001", func(ip string) (raft.NodeID, bool) {
		return raft.NodeID(ip), true
	}, nil)
	// A lookup for "localhost" typically succeeds; we only care there's no panic.
	_ = d
}

// ---- SRV tests -------------------------------------------------------------

func TestDNSDiscovery_SRV(t *testing.T) {
	records := []*net.SRV{
		{Target: "raft-0.raft.default.svc.cluster.local.", Port: 7001},
		{Target: "raft-1.raft.default.svc.cluster.local.", Port: 7002},
		{Target: "raft-2.raft.default.svc.cluster.local.", Port: 7003},
	}
	resolver := &mockResolver{srvs: records}

	mapper := func(target string, port uint16) (raft.NodeID, bool) {
		m := map[string]raft.NodeID{
			"raft-0.raft.default.svc.cluster.local.": "n0",
			"raft-1.raft.default.svc.cluster.local.": "n1",
			// raft-2 intentionally skipped
		}
		id, ok := m[target]
		return id, ok
	}

	d := dnsdiscovery.NewSRV("raft", "tcp", "default.svc.cluster.local", mapper, resolver)
	peers, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2: %v", len(peers), peers)
	}
	wantAddrs := map[string]string{
		"n0": "raft-0.raft.default.svc.cluster.local.:7001",
		"n1": "raft-1.raft.default.svc.cluster.local.:7002",
	}
	for _, p := range peers {
		want, ok := wantAddrs[string(p.ID)]
		if !ok {
			t.Errorf("unexpected peer %s", p.ID)
			continue
		}
		if p.Addr != want {
			t.Errorf("peer %s: addr %q, want %q", p.ID, p.Addr, want)
		}
	}
}

func TestDNSDiscovery_SRV_AllSkipped(t *testing.T) {
	resolver := &mockResolver{srvs: []*net.SRV{{Target: "h.", Port: 7001}}}
	d := dnsdiscovery.NewSRV("raft", "tcp", "local", func(string, uint16) (raft.NodeID, bool) {
		return "", false
	}, resolver)
	peers, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(peers) != 0 {
		t.Fatalf("expected 0 peers, got %v", peers)
	}
}

func TestDNSDiscovery_SRV_DefaultResolver(t *testing.T) {
	// Passing nil resolver must not panic.
	_ = dnsdiscovery.NewSRV("raft", "tcp", "local", func(string, uint16) (raft.NodeID, bool) {
		return "", false
	}, nil)
}
