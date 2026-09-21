package raft_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/brunoga/raft/internal/simnet"
)

// TestSimChaos_QuorumChanges runs the chaos profile while the commit quorum
// is being changed under it, so that the stricter-while-pending rule is
// exercised with faults in flight rather than argued about. The invariant
// checker holds the safety properties throughout; a policy change that let a
// leader be elected without a committed entry would show up as a Leader
// Completeness or State Machine Safety violation.
//
// Every allowed size is exercised, below and above a majority. An
// acknowledged write under a commit quorum of two is on two nodes, but the
// leader is one of them and every later leader holds it, so healing and
// restarting still converges on it.
func TestSimChaos_QuorumChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos runs skipped in short mode")
	}
	iters := simIterations(t, 1, 15)
	base := simSeed(t)
	for i := range iters {
		seed := base + uint64(i)
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			runQuorumChangeChaos(t, seed)
		})
	}
}

func runQuorumChangeChaos(t *testing.T, seed uint64) {
	p := &chaosProfile{name: "quorum_changes", nodes: 5, policy: simnet.Flaky(), crashes: true, asymmetric: true}
	cfg := defaultSimConfig(seed)
	cfg.nodes = p.nodes
	cfg.policy = p.policy
	c := newSimCluster(t, &cfg)

	if c.waitLeader(5*time.Second) < 0 {
		t.Fatalf("no leader after startup\n%s", c.diagnostics())
	}

	rng := rand.New(rand.NewPCG(seed, 0xf1e5))
	rounds := 16
	if simLong() {
		rounds = 48
	}
	quorums := []int{0, 2, 3, 4, 5}

	committed := make(map[string]string)
	acked, changed := 0, 0
	for round := range rounds {
		if c.ck.violated() {
			break
		}
		// Every other round, ask whoever leads to change the policy. It may
		// fail -- a fault may be in flight, or leadership may move -- and
		// that is fine: what matters is that whatever did land was safe.
		if round%2 == 1 {
			if li := c.leaderIndex(); li >= 0 {
				q := quorums[rng.IntN(len(quorums))]
				ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
				if err := c.node(li).SetCommitQuorum(ctx, q); err == nil {
					changed++
				}
				cancel()
			}
		}

		undo := injectFault(c, rng, p)
		key := fmt.Sprintf("k%d", round)
		val := fmt.Sprintf("v%d", round)
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		if c.put(ctx, key, val) == opOK {
			committed[key] = val
			acked++
			if got, ok := c.get(ctx, key); ok && got != val {
				t.Errorf("round %d: read of %s returned %q after a write of %q was acknowledged\n%s",
					round, key, got, val, c.diagnostics())
			}
		}
		cancel()
		undo()
		time.Sleep(15 * time.Millisecond)
	}
	t.Logf("seed %d: %d writes acknowledged, %d quorum changes applied", seed, acked, changed)
	if acked == 0 {
		t.Fatalf("no write was acknowledged in %d rounds\n%s", rounds, c.diagnostics())
	}

	c.net.HealAll()
	for i := range c.nodes {
		c.restart(i)
	}
	c.awaitConvergence(t, committed, 5*time.Second)
}
