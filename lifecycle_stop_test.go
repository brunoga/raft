package raft_test

import (
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// TestStop_WithoutStart pins that a node which was built and then abandoned
// can still be stopped.
//
// Stop used to wait on the channels the event loop and the apply loop close
// when they exit. Neither runs until Start, so Stop on a node that was never
// started waited for something that was never going to happen: New succeeds,
// some later piece of setup fails, the caller stops what it built, and hangs.
func TestStop_WithoutStart(t *testing.T) {
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = &kvSM{data: make(map[string]string)}
	cfg.Transport = memtransport.NewNetwork().NewTransport("n1")
	cfg.TickInterval = 0

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan struct{})
	go func() {
		node.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop on a node that was never started did not return")
	}

	// And again, because Stop is documented as safe to call more than once.
	node.Stop()
}
