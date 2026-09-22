package easyraft_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft/v2/easyraft"
)

// TestManager_SharedWAL runs several groups on one Manager over a single
// write-ahead log: the thing a log per group costs at scale is an fsync per
// group per round, and this is the wiring that removes it.
func TestManager_SharedWAL(t *testing.T) {
	dir := t.TempDir()
	const groups = 4

	mgr, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(dir),
		easyraft.WithSharedWAL(),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	counters := make(map[uint64]*easyraft.Collection[Counter], groups)
	for g := uint64(1); g <= groups; g++ {
		// No per-group data directory: with a shared log there is nothing for
		// one to hold.
		s, addErr := mgr.AddStore(g)
		if addErr != nil {
			t.Fatalf("AddStore(%d): %v", g, addErr)
		}
		counters[g] = easyraft.AddCollection[Counter](s, "counters")
	}
	if err = mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for g := uint64(1); g <= groups; g++ {
		s, _ := mgr.GetStore(g)
		if err = s.Ready(ctx); err != nil {
			t.Fatalf("group %d never became ready: %v", g, err)
		}
		if err = counters[g].Create(ctx, "k", Counter{Value: g}); err != nil {
			t.Fatalf("group %d write: %v", g, err)
		}
	}

	// One log holds them all, and knows which groups it holds.
	wal := mgr.SharedWAL()
	if wal == nil {
		t.Fatal("SharedWAL returned nil on a manager configured with one")
	}
	if got := len(wal.Groups()); got != groups {
		t.Errorf("the log holds %d groups, want %d", got, groups)
	}
	segments := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "wal-") {
			segments++
		}
		if e.IsDir() && e.Name() != "snap" {
			t.Errorf("a per-group directory was created alongside the shared log: %s", e.Name())
		}
	}
	if segments == 0 {
		t.Error("no shared log segment was written")
	}

	// And it all comes back: stop the manager, open a new one on the same
	// directory, and every group still has its own data.
	if err = mgr.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mgr2, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(dir),
		easyraft.WithSharedWAL(),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewManager (restart): %v", err)
	}
	counters2 := make(map[uint64]*easyraft.Collection[Counter], groups)
	for g := uint64(1); g <= groups; g++ {
		s, addErr := mgr2.AddStore(g)
		if addErr != nil {
			t.Fatalf("AddStore(%d) after restart: %v", g, addErr)
		}
		counters2[g] = easyraft.AddCollection[Counter](s, "counters")
	}
	if err = mgr2.Start(); err != nil {
		t.Fatalf("Start after restart: %v", err)
	}
	t.Cleanup(func() { _ = mgr2.Stop() })

	for g := uint64(1); g <= groups; g++ {
		s, _ := mgr2.GetStore(g)
		if err = s.Ready(ctx); err != nil {
			t.Fatalf("group %d not ready after restart: %v", g, err)
		}
		v, readErr := counters2[g].Read(ctx, "k")
		if readErr != nil {
			all, listErr := counters2[g].ListStale()
			st := s.Status()
			t.Fatalf("group %d read after restart: %v; collection holds %v (%v); "+
				"status: group=%d state=%v term=%d applied=%d",
				g, readErr, all, listErr, st.GroupID, st.State, st.Term, st.LastApplied)
		}
		if v.Value != g {
			t.Errorf("group %d holds %d after restart, want %d — groups are sharing more than a log",
				g, v.Value, g)
		}
	}
}

// TestManager_SharedWALNeedsADirectory pins that the log has to live
// somewhere, and that without the option a per-group directory is still
// required.
func TestManager_SharedWALNeedsADirectory(t *testing.T) {
	mgr, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithSharedWAL(),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err = mgr.AddStore(1); err != nil {
		t.Fatalf("AddStore without a data dir should be allowed with a shared log: %v", err)
	}
	if err = mgr.Start(); err == nil {
		_ = mgr.Stop()
		t.Fatal("Start succeeded with a shared log and nowhere to put it")
	} else if !strings.Contains(err.Error(), "WithDataDir") {
		t.Errorf("error does not say what is missing: %v", err)
	}

	plain, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err = plain.AddStore(1); err == nil {
		t.Error("AddStore without a data dir and without a shared log was accepted")
	}
}
