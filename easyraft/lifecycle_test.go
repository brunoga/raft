package easyraft_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/easyraft"
)

// refusedAddr returns a loopback address that is guaranteed to refuse
// connections: the listener is bound only long enough to claim a port and is
// closed again before the address is handed back.
func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

// TestStore_StartReportsAFailedJoinToItsCaller pins the difference between
// "my process started" and "my process is part of the cluster". A node given
// [easyraft.WithJoinAddr] that never reached a seed holds an empty log, is in
// nobody's membership, and will fail every write it accepts. If Start swallowed
// that, the only trace would be a log line — which goes nowhere at all in a
// service that configures its own logger — and the application would have no
// way to fail its own startup, retry, or report unreadiness to an orchestrator.
func TestStore_StartReportsAFailedJoinToItsCaller(t *testing.T) {
	// joinCluster spends its full retry budget before giving up, so keep this
	// off the critical path of the rest of the suite.
	t.Parallel()

	store, err := easyraft.NewStore(
		easyraft.WithID("joiner"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithJoinAddr(refusedAddr(t)),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Stop() }()

	startErr := store.Start()
	if startErr == nil {
		t.Fatal("Start reported success for a node that never joined the cluster it was given")
	}
	if !strings.Contains(startErr.Error(), "join") {
		t.Errorf("Start error = %v, want it to name the failed join", startErr)
	}
	if !strings.Contains(startErr.Error(), "joiner") {
		t.Errorf("Start error = %v, want it to name the node that failed to join", startErr)
	}

	// A node that did not join must not be pretending to be part of a cluster.
	if leader := store.Leader(); leader != "" {
		t.Errorf("Leader() = %q after a failed join, want no leader", leader)
	}
}

// TestManager_StartReportsAGroupThatCouldNotJoin pins the same guarantee for
// the multi-group path. A node that silently comes up missing one of its
// shards is worse than one that does not come up at all: nothing in the
// cluster is short a member it knows about, so there is nothing for an
// operator to notice.
func TestManager_StartReportsAGroupThatCouldNotJoin(t *testing.T) {
	// Same full retry budget as the store-level case.
	t.Parallel()

	raftAddr := freePort(t)
	mgr, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(raftAddr),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, addErr := mgr.AddStore(7,
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithJoinAddr(refusedAddr(t)),
	); addErr != nil {
		t.Fatal(addErr)
	}

	startErr := mgr.Start()
	if startErr == nil {
		_ = mgr.Stop()
		t.Fatal("Start reported success with a group that never joined its cluster")
	}
	if !strings.Contains(startErr.Error(), "group 7") {
		t.Errorf("Start error = %v, want it to name the group that failed to join", startErr)
	}

	// A failed Start cleans up after itself, so the shared Raft port is free.
	ln, err := net.Listen("tcp", raftAddr)
	if err != nil {
		t.Fatalf("Start left the shared Raft listener bound after failing: %v", err)
	}
	_ = ln.Close()
}

// TestStore_StartSucceedsForAHealthyNode is the other half of the guarantee
// above: reporting a failed join must not turn into reporting failure always.
// A single-node store with nothing to join starts, elects itself, serves a
// write, and shuts down clean.
func TestStore_StartSucceedsForAHealthyNode(t *testing.T) {
	store, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if startErr := store.Start(); startErr != nil {
		t.Fatalf("Start on a healthy single node: %v", startErr)
	}

	counters := easyraft.AddCollection[Counter](store, "counters")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := store.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if err := counters.Create(ctx, "a", Counter{Value: 1}); err != nil {
		t.Fatalf("Create after a successful Start: %v", err)
	}

	if err := store.Stop(); err != nil {
		t.Errorf("Stop on a healthy node: %v", err)
	}
}

// TestStore_StartSucceedsWhenItsSeedAnswers covers the join path itself, so
// that the fatal-join rule cannot degrade into "any node configured to join
// fails to start". The seed is a real single-node cluster.
func TestStore_StartSucceedsWhenItsSeedAnswers(t *testing.T) {
	tmpDir := t.TempDir()
	seedRaft := freePort(t)
	seedHTTP := freePort(t)

	seed, err := easyraft.NewStore(
		easyraft.WithID("seed"),
		easyraft.WithRaftAddr(seedRaft),
		easyraft.WithHTTPAddr(seedHTTP),
		easyraft.WithDataDir(filepath.Join(tmpDir, "seed")),
		easyraft.WithPeers(map[raft.NodeID]string{"seed": seedRaft}),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if startErr := seed.Start(); startErr != nil {
		t.Fatalf("seed Start: %v", startErr)
	}
	defer func() { _ = seed.Stop() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if readyErr := seed.Ready(ctx); readyErr != nil {
		t.Fatalf("seed never became ready: %v", readyErr)
	}

	joiner, err := easyraft.NewStore(
		easyraft.WithID("joiner"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(filepath.Join(tmpDir, "joiner")),
		easyraft.WithJoinAddr(seedHTTP),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = joiner.Stop() }()

	if startErr := joiner.Start(); startErr != nil {
		t.Fatalf("Start against a seed that answers: %v", startErr)
	}
}

// TestStore_StopReportsAFailedDeparture pins that [easyraft.WithLeaveOnStop]
// tells the caller whether the departure actually happened. A node that could
// not remove itself is still in the committed membership and still counts
// towards quorum, so the operator has to remove it by hand — which they can
// only do if they are told. Here the node is a follower in a two-voter cluster
// whose only peer does not exist, so there is no leader to remove it.
func TestStore_StopReportsAFailedDeparture(t *testing.T) {
	store, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithPeers(map[raft.NodeID]string{"n2": refusedAddr(t)}),
		easyraft.WithLeaveOnStop(),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if startErr := store.Start(); startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}

	stopErr := store.Stop()
	if stopErr == nil {
		t.Fatal("Stop reported a clean shutdown for a node that never left the cluster")
	}
	if !strings.Contains(stopErr.Error(), "leave") {
		t.Errorf("Stop error = %v, want it to name the failed departure", stopErr)
	}

	// Shutdown still completed: repeating it reports the same verdict rather
	// than a second round of close failures.
	if again := store.Stop(); again.Error() != stopErr.Error() {
		t.Errorf("second Stop = %v, want the first call's result %v", again, stopErr)
	}
}

// TestStore_CreatesAMissingDataDirectory pins that the caller does not have to
// create the data directory, including its parents.
//
// The store used to call os.MkdirAll itself before handing the path to the
// filestore. That has been removed, because pre-creating the directory left
// the filestore nothing to create and so skipped the fsync that makes the
// directory's own name survive a crash. The convenience is not meant to go
// with it: a path several levels below anything that exists must still work,
// which is the normal shape of a data directory under a per-node subdirectory.
func TestStore_CreatesAMissingDataDirectory(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "cluster", "n1", "raft")

	er, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(dataDir),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if startErr := er.Start(); startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}
	defer func() { _ = er.Stop() }()

	info, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("data directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dataDir)
	}
	// The filestore creates it private to the owning user. Raft state is a
	// node's votes and its log; nothing else on the host has business reading
	// it, and anything that can write it can rewrite history.
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("data directory mode = %#o, want %#o", perm, 0o700)
	}
}
