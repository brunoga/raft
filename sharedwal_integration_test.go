package raft_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/sharedwal"
	"github.com/brunoga/raft/transport/memtransport"
)

// TestSharedWAL_ClusterWithTwoGroupsPerHost runs two independent Raft groups
// on three hosts, every host keeping both groups in one shared log, through
// writes, snapshots, compaction and a restart of one host from its log.
func TestSharedWAL_ClusterWithTwoGroupsPerHost(t *testing.T) {
	const hosts, groups = 3, 2
	dir := t.TempDir()
	net := memtransport.NewNetwork()

	wals := make([]*sharedwal.WAL, hosts)
	for h := range hosts {
		w, err := sharedwal.Open(filepath.Join(dir, fmt.Sprintf("host%d", h)), sharedwal.WithSegmentSize(8192))
		if err != nil {
			t.Fatal(err)
		}
		wals[h] = w
	}
	t.Cleanup(func() {
		for _, w := range wals {
			_ = w.Close()
		}
	})

	// nodes[g][h] is group g's replica on host h.
	nodes := make([][]*raft.Node, groups)
	sms := make([][]*kvSM, groups)
	transports := make([][]raft.Transport, groups)
	build := func(g, h int, sm *kvSM) *raft.Node {
		id := raft.NodeID(fmt.Sprintf("g%d-h%d", g, h))
		var peers []raft.PeerConfig
		for p := range hosts {
			if p != h {
				peers = append(peers, raft.PeerConfig{ID: raft.NodeID(fmt.Sprintf("g%d-h%d", g, p)), Voter: true})
			}
		}
		cfg := raft.DefaultConfig()
		cfg.ID = id
		cfg.GroupID = uint64(g)
		cfg.Peers = peers
		cfg.Storage = wals[h].Storage(uint64(g))
		cfg.StateMachine = sm
		cfg.TickInterval = 0
		cfg.SnapshotThreshold = 6
		cfg.TrailingLogs = 2
		if transports[g][h] == nil {
			transports[g][h] = net.NewTransport(id)
		}
		cfg.Transport = transports[g][h]
		n, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(%s): %v", id, err)
		}
		net.Register(id, n.Handler())
		n.Start()
		return n
	}
	for g := range groups {
		nodes[g] = make([]*raft.Node, hosts)
		sms[g] = make([]*kvSM, hosts)
		transports[g] = make([]raft.Transport, hosts)
		for h := range hosts {
			sms[g][h] = &kvSM{data: make(map[string]string)}
			nodes[g][h] = build(g, h, sms[g][h])
		}
	}
	all := func() []*raft.Node {
		var out []*raft.Node
		for g := range groups {
			out = append(out, nodes[g]...)
		}
		return out
	}
	t.Cleanup(func() {
		for _, n := range all() {
			n.Stop()
		}
	})

	leaderOf := func(g int) *raft.Node {
		deadline := time.Now().Add(electionTimeout)
		for time.Now().Before(deadline) {
			for _, n := range nodes[g] {
				n.Tick()
				if n.State() == raft.Leader {
					return n
				}
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("group %d: no leader", g)
		return nil
	}
	propose := func(g int, cmd string) {
		t.Helper()
		leader := leaderOf(g)
		stop := tickWhile(all()...)
		defer stop()
		ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
		defer cancel()
		if _, err := leader.Propose(ctx, []byte(cmd)); err != nil {
			t.Fatalf("group %d: Propose(%s): %v", g, cmd, err)
		}
	}

	// Enough writes in both groups to cross the snapshot threshold twice.
	for i := range 20 {
		for g := range groups {
			propose(g, fmt.Sprintf("k%d=g%d-%d", i, g, i))
		}
	}
	for g := range groups {
		if nodes[g][0].SnapshotIndex() == 0 && nodes[g][1].SnapshotIndex() == 0 && nodes[g][2].SnapshotIndex() == 0 {
			t.Fatalf("group %d never snapshotted", g)
		}
	}

	// Restart host 0 from its log: both groups come back with their state.
	for g := range groups {
		nodes[g][0].Stop()
		net.Unregister(nodes[g][0].ID())
	}
	if err := wals[0].Close(); err != nil {
		t.Fatal(err)
	}
	w, err := sharedwal.Open(filepath.Join(dir, "host0"), sharedwal.WithSegmentSize(8192))
	if err != nil {
		t.Fatalf("reopen host 0: %v", err)
	}
	wals[0] = w
	if got := w.Groups(); len(got) != groups {
		t.Fatalf("host 0 knows groups %v after restart, want %d", got, groups)
	}
	for g := range groups {
		sms[g][0] = &kvSM{data: make(map[string]string)}
		nodes[g][0] = build(g, 0, sms[g][0])
	}
	for g := range groups {
		propose(g, fmt.Sprintf("after=g%d", g))
	}
	// The restarted replicas catch up with everything, from their own log
	// plus whatever the leader sends.
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		ok := true
		for g := range groups {
			if sms[g][0].Get("after") != fmt.Sprintf("g%d", g) || sms[g][0].Get("k19") != fmt.Sprintf("g%d-19", g) {
				ok = false
			}
		}
		if ok {
			return
		}
		for _, n := range all() {
			n.Tick()
		}
		time.Sleep(time.Millisecond)
	}
	for g := range groups {
		t.Errorf("group %d on host 0: after=%q k19=%q", g, sms[g][0].Get("after"), sms[g][0].Get("k19"))
	}
}
