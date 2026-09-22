package raft_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// establishLease runs a full ReadIndex on the leader, ticking the cluster
// until it completes, which grants the leader a read lease.
func establishLease(t *testing.T, c *Cluster, leader *raft.Node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	riCh := make(chan error, 1)
	go func() {
		_, err := leader.ReadIndex(ctx)
		riCh <- err
	}()
	deadline := time.Now().Add(electionTimeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		select {
		case err := <-riCh:
			if err != nil {
				t.Fatalf("ReadIndex: %v", err)
			}
			return
		default:
		}
	}
	t.Fatal("ReadIndex timed out")
}

// leaseRead calls ReadIndexLease on the leader without ticking, so that only
// the lease fast path can answer, and returns its error.
func leaseRead(t *testing.T, leader *raft.Node) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := leader.ReadIndexLease(ctx)
		errCh <- err
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ReadIndexLease did not return; the fast path should not block")
		return nil
	}
}

// TestReadIndexLease_SafetyMarginShortensLease checks that the lease expires
// LeaseSafetyMargin before ElectionTimeoutMin, so that a follower whose
// clock runs a little fast cannot hold an election inside a lease this
// leader still believes in.
func TestReadIndexLease_SafetyMarginShortensLease(t *testing.T) {
	const margin = 50 * time.Millisecond
	clk := newManualClock()
	var electionMin time.Duration
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.Clock = clk
		cfg.LeaseSafetyMargin = margin
		electionMin = cfg.ElectionTimeoutMin
	})
	leader := c.nodes[c.WaitLeader(electionTimeout)]
	establishLease(t, c, leader)

	// Still inside the shortened lease.
	clk.Advance(electionMin - margin - 10*time.Millisecond)
	if err := leaseRead(t, leader); err != nil {
		t.Fatalf("lease read inside the shortened lease failed: %v", err)
	}

	// Past the shortened lease, though still inside ElectionTimeoutMin: the
	// margin is what makes this expired.
	clk.Advance(20 * time.Millisecond)
	if err := leaseRead(t, leader); !errors.Is(err, raft.ErrLeaseExpired) {
		t.Fatalf("lease read %v past the shortened lease returned %v; want ErrLeaseExpired",
			electionMin-margin+10*time.Millisecond, err)
	}
}

// TestReadIndexLease_ZeroMarginIsTheOldLease checks that a zero margin keeps
// the lease at exactly ElectionTimeoutMin, which is what configurations
// written before the field existed get.
func TestReadIndexLease_ZeroMarginIsTheOldLease(t *testing.T) {
	clk := newManualClock()
	var electionMin time.Duration
	c := newClusterWith(t, 3, func(cfg *raft.Config) {
		cfg.Clock = clk
		cfg.LeaseSafetyMargin = 0
		electionMin = cfg.ElectionTimeoutMin
	})
	leader := c.nodes[c.WaitLeader(electionTimeout)]
	establishLease(t, c, leader)

	clk.Advance(electionMin - 10*time.Millisecond)
	if err := leaseRead(t, leader); err != nil {
		t.Fatalf("lease read just inside ElectionTimeoutMin failed: %v", err)
	}
	clk.Advance(20 * time.Millisecond)
	if err := leaseRead(t, leader); !errors.Is(err, raft.ErrLeaseExpired) {
		t.Fatalf("lease read past ElectionTimeoutMin returned %v; want ErrLeaseExpired", err)
	}
}

// TestLeaseSafetyMargin_Validate checks the bounds Validate enforces.
func TestLeaseSafetyMargin_Validate(t *testing.T) {
	base := func() raft.Config {
		cfg := raft.DefaultConfig()
		cfg.ID = "n1"
		cfg.Storage = memstore.New()
		cfg.StateMachine = &kvSM{data: make(map[string]string)}
		cfg.Transport = memtransport.NewNetwork().NewTransport(cfg.ID)
		return cfg
	}
	cfg := base()
	cfg.LeaseSafetyMargin = -time.Millisecond
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a negative LeaseSafetyMargin")
	}
	cfg = base()
	cfg.LeaseSafetyMargin = cfg.ElectionTimeoutMin
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a LeaseSafetyMargin equal to ElectionTimeoutMin")
	}
	cfg = base()
	if cfg.LeaseSafetyMargin <= 0 {
		t.Fatal("DefaultConfig should set a non-zero LeaseSafetyMargin")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected the defaults: %v", err)
	}
}
