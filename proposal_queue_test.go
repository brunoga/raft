package raft_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// newUnstartedNode builds a node whose event loop never runs, so that
// everything handed to Propose stays in the proposal queue. It is how a test
// fills the queue deterministically: nothing drains it.
func newUnstartedNode(t *testing.T, mutate func(*raft.Config)) *raft.Node {
	t.Helper()
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = &kvSM{data: make(map[string]string)}
	cfg.Transport = memtransport.NewNetwork().NewTransport(cfg.ID)
	cfg.TickInterval = 0
	if mutate != nil {
		mutate(&cfg)
	}
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	return node
}

// fillProposalQueue submits n proposals from goroutines that stay parked
// waiting for an answer, and returns once every one of them is in the queue.
// The returned cancel releases them.
func fillProposalQueue(t *testing.T, node *raft.Node, n int) (release func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := node.Propose(ctx, []byte("x"))
			if !errors.Is(err, context.Canceled) {
				t.Errorf("parked Propose returned %v; want context.Canceled", err)
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for node.ProposalQueueDepth() < n {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("queue depth %d after 5s, want %d", node.ProposalQueueDepth(), n)
		}
		time.Sleep(time.Millisecond)
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

// TestProposalQueue_RejectWhenFull checks that with ProposalOverflowReject a
// proposal that finds the queue full is refused at once with
// ErrProposalQueueFull rather than waiting, and that ProposalQueueDepth
// reports the occupancy that caused it.
func TestProposalQueue_RejectWhenFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const size = 3
		node := newUnstartedNode(t, func(c *raft.Config) {
			c.ProposalQueueSize = size
			c.ProposalOverflow = raft.ProposalOverflowReject
		})

		release := fillProposalQueue(t, node, size)
		defer release()

		if got := node.ProposalQueueDepth(); got != size {
			t.Fatalf("ProposalQueueDepth = %d, want %d", got, size)
		}

		start := time.Now()
		_, err := node.Propose(context.Background(), []byte("one too many"))
		if !errors.Is(err, raft.ErrProposalQueueFull) {
			t.Fatalf("Propose on a full queue returned %v; want ErrProposalQueueFull", err)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("rejection took %v; it should not have waited", took)
		}

		_, err = node.ProposeOnce(context.Background(), "client", 1, []byte("also too many"))
		if !errors.Is(err, raft.ErrProposalQueueFull) {
			t.Fatalf("ProposeOnce on a full queue returned %v; want ErrProposalQueueFull", err)
		}
	})
}

// TestProposalQueue_WaitWhenFull checks the default policy: a proposal that
// finds the queue full waits, bounded by its context, and never sees
// ErrProposalQueueFull.
func TestProposalQueue_WaitWhenFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const size = 2
		node := newUnstartedNode(t, func(c *raft.Config) {
			c.ProposalQueueSize = size
		})

		release := fillProposalQueue(t, node, size)
		defer release()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := node.Propose(ctx, []byte("waits"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Propose on a full queue returned %v; want context.DeadlineExceeded", err)
		}
		if node.ProposalQueueDepth() != size {
			t.Fatalf("a proposal that gave up waiting must not be in the queue; depth = %d",
				node.ProposalQueueDepth())
		}
	})
}

// TestProposalQueue_DefaultSize checks that a zero ProposalQueueSize selects
// the documented default rather than an unbuffered queue.
func TestProposalQueue_DefaultSize(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newUnstartedNode(t, func(c *raft.Config) {
			c.ProposalQueueSize = 0
			c.ProposalOverflow = raft.ProposalOverflowReject
		})
		release := fillProposalQueue(t, node, raft.DefaultProposalQueueSize)
		defer release()
		if _, err := node.Propose(context.Background(), []byte("x")); !errors.Is(err, raft.ErrProposalQueueFull) {
			t.Fatalf("got %v after %d proposals; want ErrProposalQueueFull", err, raft.DefaultProposalQueueSize)
		}
	})
}

// TestProposalQueue_Validate checks that Validate refuses a negative size and
// an unknown overflow policy.
func TestProposalQueue_Validate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := func() raft.Config {
			cfg := raft.DefaultConfig()
			cfg.ID = "n1"
			cfg.Storage = memstore.New()
			cfg.StateMachine = &kvSM{data: make(map[string]string)}
			cfg.Transport = memtransport.NewNetwork().NewTransport(cfg.ID)
			return cfg
		}
		cfg := base()
		cfg.ProposalQueueSize = -1
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate accepted a negative ProposalQueueSize")
		}
		cfg = base()
		cfg.ProposalOverflow = raft.ProposalOverflowPolicy(42)
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate accepted an unknown ProposalOverflow policy")
		}
		cfg = base()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate rejected the defaults: %v", err)
		}
	})
}
