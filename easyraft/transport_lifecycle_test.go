package easyraft_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
)

// mustBind checks that addr is free, which is how a test asks whether a
// listener was actually released.
func mustBind(t *testing.T, addr, what string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	_ = ln.Close()
}

// TestStop_ReleasesTheRaftListener pins that a stopped store gives its Raft
// address back.
//
// Node.Stop unregisters the node's handler but does not close the transport --
// correctly, since a transport can be shared by several groups -- and nothing
// else was closing the one a Store opens for itself. A service that restarts
// its store in process could not rebind, and one that creates and destroys
// stores leaked a listener and its goroutines each time.
func TestStop_ReleasesTheRaftListener(t *testing.T) {
	raftAddr, httpAddr := freePort(t), freePort(t)
	dir := filepath.Join(t.TempDir(), "n1")

	start := func() *easyraft.EasyRaft[Counter] {
		t.Helper()
		er, err := easyraft.New[Counter](
			easyraft.WithID("n1"),
			easyraft.WithRaftAddr(raftAddr),
			easyraft.WithHTTPAddr(httpAddr),
			easyraft.WithDataDir(dir),
			easyraft.WithPeers(map[raft.NodeID]string{"n1": raftAddr}),
			easyraft.WithInsecureTransportAcknowledged(),
			easyraft.WithInsecureHTTPAcknowledged(),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if startErr := er.Start(); startErr != nil {
			t.Fatalf("Start: %v", startErr)
		}
		return er
	}

	er := start()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := er.Ready(ctx); err != nil {
		cancel()
		t.Fatalf("Ready: %v", err)
	}
	cancel()
	if err := er.Create(context.Background(), "k", Counter{Value: 1}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := er.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mustBind(t, raftAddr, "the Raft listener is still open after Stop")
	mustBind(t, httpAddr, "the HTTP listener is still open after Stop")

	// The real test of it: the same process starts the same store again on the
	// same addresses, which is what a restart in place looks like.
	er = start()
	defer func() { _ = er.Stop() }()
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := er.Ready(ctx); err != nil {
		t.Fatalf("Ready after restart: %v", err)
	}
	got, err := er.Read(ctx, "k")
	if err != nil {
		t.Fatalf("Read after restart: %v", err)
	}
	if got.Value != 1 {
		t.Errorf("value after restart is %d, want 1", got.Value)
	}
}

// TestStop_ConstructedButNeverStartedReleasesIt covers the other path: a store
// whose construction succeeded but which was never started still holds an open
// listener, because NewStore binds.
func TestStop_ConstructedButNeverStartedReleasesIt(t *testing.T) {
	raftAddr, httpAddr := freePort(t), freePort(t)
	er, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(raftAddr),
		easyraft.WithHTTPAddr(httpAddr),
		easyraft.WithDataDir(filepath.Join(t.TempDir(), "n1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": raftAddr}),
		easyraft.WithInsecureTransportAcknowledged(),
		easyraft.WithInsecureHTTPAcknowledged(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := er.Stop(); err != nil {
		t.Fatalf("Stop without Start: %v", err)
	}
	mustBind(t, raftAddr, "the Raft listener is still open")
	mustBind(t, httpAddr, "the HTTP listener is still open")
}

// TestManager_StopReleasesTheSharedListenerOnce checks that the ownership rule
// holds the other way. A Manager's groups share one transport, so no group may
// close it: the first one to stop would take the listener away from the ones
// still shutting down, and the Manager would then close it a second time.
func TestManager_StopReleasesTheSharedListenerOnce(t *testing.T) {
	raftAddr := freePort(t)
	dir := t.TempDir()

	m, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(raftAddr),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Numbered from one: a Manager reserves group zero, since it routes by
	// group ID and zero is what a single-group node's RPCs carry.
	for g := uint64(1); g <= 2; g++ {
		if _, addErr := m.AddStore(g,
			easyraft.WithDataDir(filepath.Join(dir, "g"+string(rune('0'+g)))),
			easyraft.WithPeers(map[raft.NodeID]string{"n1": raftAddr}),
		); addErr != nil {
			t.Fatalf("AddStore %d: %v", g, addErr)
		}
	}
	if startErr := m.Start(); startErr != nil {
		t.Fatalf("Manager Start: %v", startErr)
	}
	if stopErr := m.Stop(); stopErr != nil {
		t.Fatalf("Manager Stop: %v", stopErr)
	}
	mustBind(t, raftAddr, "the shared Raft listener is still open after Manager.Stop")
}
