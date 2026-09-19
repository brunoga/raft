package raft_test

// Randomized chaos runs over the simulated network, with the invariant checker
// watching throughout.
//
// The older chaos tests in linearizability_test.go assert only that one key's
// last value eventually converges. That catches a cluster that has wedged, but
// it says nothing about whether the cluster reached agreement legally: a run
// that elected two leaders in one term, truncated a committed entry and then
// happened to converge would pass. These runs instead assert the five Raft
// safety properties continuously (see siminvariant_test.go) and only then check
// convergence.
//
// Knobs, all read from the environment so that `go test ./...` stays fast while
// a maintainer can soak the same code for as long as they like:
//
//	RAFT_SIM_ITERS=<n>     number of randomized iterations per profile
//	RAFT_SIM_LONG=1        soak: many more iterations and longer runs
//	RAFT_SIM_SEED=<seed>   replay a specific run (see internal/simnet)
//
// A failure prints the seed and the full ordered fault schedule.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/internal/simnet"
)

// simLong reports whether this is a soak run.
func simLong() bool { return os.Getenv("RAFT_SIM_LONG") == "1" }

// simIterations returns how many randomized iterations to run. The default is
// deliberately low: the whole package test suite is expected to finish in about
// a minute, and these runs are the expensive part of it.
func simIterations(t testing.TB, short, long int) int {
	t.Helper()
	if s := os.Getenv("RAFT_SIM_ITERS"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			t.Fatalf("RAFT_SIM_ITERS=%q: want a positive integer", s)
		}
		return v
	}
	if simLong() {
		return long
	}
	return short
}

// chaosProfile describes one family of randomized runs.
type chaosProfile struct {
	name string
	// nodes is the cluster size.
	nodes int
	// policy is the per-link behaviour: loss, duplication, delay, reordering.
	policy simnet.LinkPolicy
	// crashes enables crash-restart faults, where a node is stopped, its
	// storage is rolled back to what was durable, and a new node is started on
	// it. Without this the only faults are network faults.
	crashes bool
	// asymmetric enables one-way link cuts, where a node can send but not
	// receive (or the reverse). These are the partitions that break naive
	// leader-election implementations, because the isolated side keeps
	// bumping its term without ever learning it has lost.
	asymmetric bool
}

// TestSimChaos exercises the cluster under randomized faults and asserts the
// Raft safety properties throughout, then convergence at the end.
//
// What each profile protects:
//
//	message faults   that retries and the idempotency of the RPC handlers are
//	                 enough: a lost, delayed, duplicated or reordered message
//	                 must never produce a different outcome from a clean run
//	crash-restart    that everything a node acknowledged is really on stable
//	                 storage: a restarted node must not forget a vote or a log
//	                 entry it had already promised
//	asymmetric       that a node which can send but not receive cannot disrupt
//	                 a healthy cluster, and that the healthy side keeps its
//	                 leader
func TestSimChaos(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos runs skipped in short mode")
	}

	profiles := []chaosProfile{
		{
			name:   "message_faults",
			nodes:  5,
			policy: simnet.Flaky(),
		},
		{
			name:    "crash_restart",
			nodes:   5,
			policy:  simnet.Lossless(),
			crashes: true,
		},
		{
			name:       "everything",
			nodes:      5,
			policy:     simnet.Flaky(),
			crashes:    true,
			asymmetric: true,
		},
	}

	iters := simIterations(t, 1, 15)
	base := simSeed(t)

	for _, p := range profiles {
		t.Run(p.name, func(t *testing.T) {
			for i := range iters {
				seed := base + uint64(i)
				t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
					runChaosIteration(t, &p, seed)
				})
			}
		})
	}
}

// runChaosIteration builds a cluster, beats on it, and then checks that what
// survived is consistent.
func runChaosIteration(t *testing.T, p *chaosProfile, seed uint64) {
	cfg := defaultSimConfig(seed)
	cfg.nodes = p.nodes
	cfg.policy = p.policy
	c := newSimCluster(t, &cfg)

	if c.waitLeader(5*time.Second) < 0 {
		t.Fatalf("no leader after startup\n%s", c.diagnostics())
	}

	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	rounds := 12
	if simLong() {
		rounds = 40
	}

	// committed records the rounds whose write the client was told succeeded.
	// Those values must be present on every node at the end; the rest may go
	// either way, and the only requirement is that every node agrees.
	committed := make(map[string]string)
	acked := 0

	for round := range rounds {
		if c.ck.violated() {
			break
		}
		undo := injectFault(c, rng, p)

		key := fmt.Sprintf("k%d", round)
		val := fmt.Sprintf("v%d", round)
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		if c.put(ctx, key, val) == opOK {
			committed[key] = val
			acked++
			// A linearizable read issued after an acknowledged write must
			// observe it. This is the only liveness-and-correctness assertion
			// made while the fault is still in place.
			if got, ok := c.get(ctx, key); ok && got != val {
				t.Errorf("round %d: read of %s returned %q after a write of %q was acknowledged\n%s",
					round, key, got, val, c.diagnostics())
			}
		}
		cancel()

		undo()
		time.Sleep(15 * time.Millisecond)
	}

	if acked == 0 {
		t.Fatalf("no write was acknowledged in %d rounds; the cluster never made progress\n%s",
			rounds, c.diagnostics())
	}

	// Heal everything and let the cluster settle.
	c.net.HealAll()
	for i := range c.nodes {
		c.restart(i)
	}
	c.awaitConvergence(t, committed, 5*time.Second)
}

// injectFault picks and applies one fault, returning the function that undoes
// it. Only one fault is in flight at a time, which keeps a quorum available and
// keeps the failures that do turn up attributable.
func injectFault(c *simCluster, rng *rand.Rand, p *chaosProfile) func() {
	n := len(c.nodes)

	kinds := []string{"isolate", "minority"}
	if p.crashes {
		kinds = append(kinds, "crash")
	}
	if p.asymmetric {
		kinds = append(kinds, "oneway")
	}

	switch kinds[rng.IntN(len(kinds))] {
	case "isolate":
		v := c.ids[rng.IntN(n)]
		c.net.Isolate(v)
		return func() { c.net.Heal(v) }

	case "minority":
		// Cut a minority of the cluster off from the majority. The majority
		// must keep serving; the minority must not elect anyone.
		perm := rng.Perm(n)
		size := 1 + rng.IntN((n-1)/2)
		minority := perm[:size]
		for _, a := range minority {
			for b := range n {
				if !contains(minority, b) {
					c.net.Cut(c.ids[a], c.ids[b])
					c.net.Cut(c.ids[b], c.ids[a])
				}
			}
		}
		return func() {
			for _, a := range minority {
				c.net.Heal(c.ids[a])
			}
		}

	case "crash":
		v := rng.IntN(n)
		c.crash(v)
		return func() { c.restart(v) }

	default: // oneway
		a := rng.IntN(n)
		b := (a + 1 + rng.IntN(n-1)) % n
		c.net.Cut(c.ids[a], c.ids[b])
		return func() { c.net.Restore(c.ids[a], c.ids[b]) }
	}
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// awaitConvergence waits for every node to hold every acknowledged write and
// for all nodes to agree on every key they hold.
func (c *simCluster) awaitConvergence(t *testing.T, committed map[string]string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ok, why := c.converged(committed)
		if ok {
			return
		}
		last = why
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("cluster did not converge within %s: %s\n%s", timeout, last, c.diagnostics())
}

// converged reports whether every node holds every acknowledged write and no
// two nodes disagree about any key.
func (c *simCluster) converged(committed map[string]string) (ok bool, reason string) {
	for key, want := range committed {
		for i, sn := range c.nodes {
			if _, running := sn.get(); !running {
				continue
			}
			if got := sn.sm.Get(key); got != want {
				return false, fmt.Sprintf("node %s has %s=%q, want %q (write was acknowledged)", c.ids[i], key, got, want)
			}
		}
	}
	// Every node must also agree about the keys nobody acknowledged: an
	// unacknowledged write may or may not have landed, but it cannot have
	// landed on some replicas and not others once the cluster is healthy.
	for key := range allKeys(c) {
		var ref string
		var refID raft.NodeID
		first := true
		for i, sn := range c.nodes {
			if _, running := sn.get(); !running {
				continue
			}
			got := sn.sm.Get(key)
			if first {
				ref, refID, first = got, c.ids[i], false
				continue
			}
			if got != ref {
				return false, fmt.Sprintf("nodes disagree about %s: %s has %q, %s has %q", key, refID, ref, c.ids[i], got)
			}
		}
	}
	return true, ""
}

// allKeys returns the union of the keys every node's state machine holds.
func allKeys(c *simCluster) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, sn := range c.nodes {
		sn.sm.mu.RLock()
		for k := range sn.sm.data {
			keys[k] = struct{}{}
		}
		sn.sm.mu.RUnlock()
	}
	return keys
}
