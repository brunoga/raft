package raft_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// ----------------------------------------------------------------------------
// Cluster — a self-contained in-process Raft cluster for integration tests.
// ----------------------------------------------------------------------------

// Cluster manages a set of Raft nodes sharing an in-memory network.
type Cluster struct {
	t     testing.TB
	net   *memtransport.Network
	nodes []*raft.Node
	ids   []raft.NodeID
	sms   []*kvSM

	// bubbled records whether this cluster was built inside a synctest
	// bubble, which decides what "wait for the cluster to settle" can mean.
	bubbled bool
}

// newCluster creates a new n-node cluster.
func newCluster(t testing.TB, n int) *Cluster {
	return newClusterWith(t, n, nil)
}

// newClusterWith creates a new n-node cluster with custom configuration.
func newClusterWith(t testing.TB, n int, mutate func(*raft.Config)) *Cluster {
	c := &Cluster{
		t:       t,
		net:     memtransport.NewNetwork(),
		bubbled: inBubble(),
	}

	for i := range n {
		id := raft.NodeID(fmt.Sprintf("n%d", i+1))
		c.ids = append(c.ids, id)
	}

	for i := range n {
		peers := make([]raft.PeerConfig, 0, n-1)
		for j := range n {
			if i == j {
				continue
			}
			peers = append(peers, raft.PeerConfig{ID: c.ids[j], Voter: true})
		}

		sm := &kvSM{data: make(map[string]string)}
		c.sms = append(c.sms, sm)

		cfg := raft.DefaultConfig()
		cfg.ID = c.ids[i]
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = sm
		cfg.Transport = c.net.NewTransport(cfg.ID)
		cfg.TickInterval = 0 // Manual ticks for deterministic tests

		if mutate != nil {
			mutate(&cfg)
		}

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(%d): %v", i+1, err)
		}
		c.nodes = append(c.nodes, node)
		c.net.Register(cfg.ID, node.Handler())
	}

	for _, node := range c.nodes {
		node.Start()
	}

	t.Cleanup(func() {
		for _, node := range c.nodes {
			node.Stop()
		}
	})

	return c
}

// inBubble reports whether the caller is running inside a synctest bubble.
//
// synctest.Wait panics outside one, and that panic is the only way to ask.
// Calling Wait to find out is not a trick played on the runtime: waiting is
// what the caller wants anyway, so inside a bubble the question and the
// answer are the same operation.
func inBubble() (yes bool) {
	defer func() { yes = recover() == nil }()
	synctest.Wait()
	return
}

// settle waits for the cluster to finish reacting to whatever the test just
// did, then lets one unit of time pass.
//
// Inside a bubble the first half is exact: synctest.Wait returns only once
// every other goroutine in the bubble is durably blocked, so every apply,
// every replication round and every heartbeat that the last call set in
// motion has run to completion. Outside one -- the benchmarks, which have to
// measure real work against a real clock -- no such signal exists, and a
// short sleep is the best approximation available.
//
// Time still advances in both cases, because the callers below bound
// themselves with a deadline and a deadline that never arrives is a hang.
// In a bubble that advance is of the fake clock, so it costs nothing and a
// loaded machine cannot shorten it.
//
// # A hang here is a loop with no timer in it
//
// A bubble's clock advances only when every goroutine in it is durably
// blocked. Code that hands work from goroutine to goroutine without ever
// waiting on a timer is never idle, so the clock never moves and the test
// hangs at its timeout with a goroutine dump that shows one goroutine
// runnable and the rest blocked. That is not a deadlock in Raft and not a
// runtime fault: it is a livelock that a real clock would have hidden by
// letting the test's deadline expire.
//
// The first one found this way was real. A stale AppendEntries rejection
// -- produced before the follower acknowledged an entry, delivered after --
// moved nextIndex below matchIndex, and the pipeline then re-sent the same
// entries on every success without a timer between rounds, at about ten
// thousand transfers a second, until the leader lost leadership. It had
// been mistaken for a scheduler bug because the dumps looked like one. See
// handleAppendResult in node_replication.go and the tests in
// stale_rejection_internal_test.go.
//
// So if a bubbled test hangs: read the dump for the goroutine that is
// runnable rather than blocked, and find the cycle it is in.
func (c *Cluster) settle() {
	if c.bubbled {
		synctest.Wait()
	}
	time.Sleep(time.Millisecond)
}

// Tick advances time by one unit for all nodes in the cluster.
func (c *Cluster) Tick() {
	for _, node := range c.nodes {
		node.Tick()
	}
}

// TickN advances time by n units.
func (c *Cluster) TickN(n int) {
	for range n {
		c.Tick()
		c.settle()
	}
}

// WaitLeader waits for any node to become leader, up to timeout.
// Fails the test immediately if no leader is elected within timeout.
// Returns the index of the leader node.
func (c *Cluster) WaitLeader(timeout time.Duration) int {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for i, node := range c.nodes {
			if node.State() == raft.Leader {
				return i
			}
		}
		c.Tick()
		c.settle()
	}
	c.t.Fatalf("no leader elected within %s", timeout)
	return -1 // unreachable; satisfies the compiler
}

// Leader returns the current leader node, or nil if none is elected.
func (c *Cluster) Leader(timeout time.Duration) *raft.Node {
	idx := c.WaitLeader(timeout)
	if idx < 0 {
		return nil
	}
	return c.nodes[idx]
}

func (c *Cluster) LeaderIndex() int {
	for i, node := range c.nodes {
		if node.State() == raft.Leader {
			return i
		}
	}
	return -1
}

// Propose submits a command to the current leader.
func (c *Cluster) Propose(timeout time.Duration, cmd []byte) ([]byte, error) {
	leader := c.Leader(timeout)
	if leader == nil {
		return nil, fmt.Errorf("no leader")
	}

	// We need to tick while waiting for the proposal to commit.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	type result struct {
		val []byte
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		v, err := leader.Propose(ctx, cmd)
		resCh <- result{v, err}
	}()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case r := <-resCh:
			return r.val, r.err
		case <-ticker.C:
			c.Tick()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Disconnect isolates node i from the rest of the cluster.
func (c *Cluster) Disconnect(i int) {
	for j := range c.nodes {
		if i == j {
			continue
		}
		c.net.Drop(c.ids[i], c.ids[j])
		c.net.Drop(c.ids[j], c.ids[i])
	}
}

// Reconnect restores network connectivity for node i.
func (c *Cluster) Reconnect(i int) {
	for j := range c.nodes {
		if i == j {
			continue
		}
		c.net.Restore(c.ids[i], c.ids[j])
		c.net.Restore(c.ids[j], c.ids[i])
	}
	// Restoring a link closes the channel that parks RPCs held on the dropped
	// link, which makes their goroutines runnable. Let them run before the
	// caller blocks on the clock again, so the caller's next look at the
	// cluster sees the reconnect already taken effect.
	if c.bubbled {
		synctest.Wait()
	}
}

// DropLink disables message delivery from node from to node to (one way).
func (c *Cluster) DropLink(from, to int) {
	c.net.Drop(c.ids[from], c.ids[to])
}

// RestoreLink re-enables delivery from node from to node to.
func (c *Cluster) RestoreLink(from, to int) {
	c.net.Restore(c.ids[from], c.ids[to])
}

// ----------------------------------------------------------------------------
// kvSM — a simple key-value state machine used in tests.
// Commands have the form "key=value" for Set, or "key" for Get.
// All methods are safe for concurrent use.
// ----------------------------------------------------------------------------

type kvSM struct {
	mu   sync.RWMutex
	data map[string]string
}

func (sm *kvSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	if len(e.Command) == 0 {
		return nil, nil // no-op entry
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for i, b := range e.Command {
		if b == '=' {
			key := string(e.Command[:i])
			val := string(e.Command[i+1:])
			sm.data[key] = val
			return []byte(val), nil
		}
	}
	v := sm.data[string(e.Command)]
	return []byte(v), nil
}

// Get returns the current value for key. Safe for concurrent use.
func (sm *kvSM) Get(key string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.data[key]
}

// Snapshot serialises the map as "key\tvalue\n" lines.
func (sm *kvSM) Snapshot(_ context.Context, w io.Writer) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for k, v := range sm.data {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", k, v); err != nil {
			return err
		}
	}
	return nil
}

// Restore replaces the map with the snapshot contents.
func (sm *kvSM) Restore(_ context.Context, meta raft.SnapshotMeta, r io.Reader) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data = make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 {
			sm.data[parts[0]] = parts[1]
		}
	}
	return scanner.Err()
}
