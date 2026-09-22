package easyraft_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brunoga/raft/v2/easyraft"
)

// TestShutdown_ReturnsWhenTheDeadlinePasses pins the point of a bounded
// shutdown: a caller that gave it a deadline gets control back when that
// deadline passes, even though the shutdown itself carries on.
func TestShutdown_ReturnsWhenTheDeadlinePasses(t *testing.T) {
	s, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already past its deadline
	if err := s.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		// A shutdown this small usually wins the race with an already-cancelled
		// context, which is a pass too: what must not happen is a hang.
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}

	// Whatever the race, the store ends up stopped.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.Stop(); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the store never finished stopping")
}

// TestShutdown_IsStopWhenThereIsTime pins that with a deadline it can meet,
// Shutdown reports exactly what Stop would.
func TestShutdown_IsStopWhenThereIsTime(t *testing.T) {
	s, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with time to spare: %v", err)
	}
	// Later calls repeat the first verdict, as with Stop.
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

// TestManagerShutdown_IsStopWhenThereIsTime does the same for a Manager.
func TestManagerShutdown_IsStopWhenThereIsTime(t *testing.T) {
	mgr, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithSharedWAL(),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := mgr.AddStore(1); err != nil {
		t.Fatalf("AddStore: %v", err)
	}
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := mgr.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with time to spare: %v", err)
	}
}
