package raft_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

// ----------------------------------------------------------------------------
// Cluster — a self-contained in-process Raft cluster for integration tests.
// ----------------------------------------------------------------------------

// Cluster manages a set of Raft nodes sharing an in-memory network.
type Cluster struct {
	t      testing.TB
	net    *memtransport.Network
	nodes  []*raft.Node
	ids    []raft.NodeID
	stores []*memstore.MemStore
	sms    []*kvSM
}

// newCluster creates and starts a cluster of n nodes with manual ticks
// (TickInterval=0). The caller drives time via cluster.Tick().
func newCluster(t testing.TB, n int) *Cluster {
	return newClusterWith(t, n, nil)
}

// newClusterWith is like newCluster but applies modify (if non-nil) to each
// node's Config before construction. Use it to override defaults such as
// SnapshotThreshold.
func newClusterWith(t testing.TB, n int, modify func(*raft.Config)) *Cluster {
	t.Helper()
	net := memtransport.NewNetwork()
	ids := make([]raft.NodeID, n)
	for i := range n {
		ids[i] = raft.NodeID(fmt.Sprintf("n%d", i+1))
	}

	c := &Cluster{t: t, net: net, ids: ids}

	for i := range n {
		peers := make([]raft.NodeID, 0, n-1)
		for j, id := range ids {
			if j != i {
				peers = append(peers, id)
			}
		}

		tr := net.NewTransport(ids[i])
		store := memstore.New()
		sm := &kvSM{data: make(map[string]string)}

		cfg := raft.DefaultConfig()
		cfg.ID = ids[i]
		cfg.Peers = peers
		cfg.Storage = store
		cfg.StateMachine = sm
		cfg.Transport = tr
		cfg.TickInterval = 0 // manual ticks

		if modify != nil {
			modify(&cfg)
		}

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("New node %s: %v", ids[i], err)
		}
		c.nodes = append(c.nodes, node)
		c.stores = append(c.stores, store)
		c.sms = append(c.sms, sm)
	}

	// Register all handlers before starting so that the first heartbeat
	// already reaches a live handler.
	for i, node := range c.nodes {
		net.Register(ids[i], node)
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

// Tick delivers one tick to every node.
func (c *Cluster) Tick() {
	for _, n := range c.nodes {
		n.Tick()
	}
}

// TickN delivers n ticks to every node with a brief yield between each.
func (c *Cluster) TickN(n int) {
	for range n {
		c.Tick()
		time.Sleep(time.Millisecond)
	}
}

// WaitLeader blocks until exactly one leader is elected, then returns its
// index in c.nodes. Fails the test after timeout.
func (c *Cluster) WaitLeader(timeout time.Duration) int {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.Tick()
		time.Sleep(time.Millisecond)
		if idx := c.leaderIndex(); idx >= 0 {
			return idx
		}
	}
	c.t.Fatalf("no leader elected within %s", timeout)
	return -1
}

// leaderIndex returns the index of the current leader, or -1 if none.
func (c *Cluster) leaderIndex() int {
	leaders := []int{}
	for i, n := range c.nodes {
		if n.StateSnapshot() == raft.Leader {
			leaders = append(leaders, i)
		}
	}
	if len(leaders) == 1 {
		return leaders[0]
	}
	return -1
}

// Leader returns the current leader node, failing the test if none.
func (c *Cluster) Leader(timeout time.Duration) *raft.Node {
	c.t.Helper()
	idx := c.WaitLeader(timeout)
	return c.nodes[idx]
}

// Propose submits a command to the current leader and awaits the result,
// ticking the cluster in the background to drive progress.
func (c *Cluster) Propose(timeout time.Duration, cmd []byte) ([]byte, error) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	leader := c.Leader(timeout / 2)

	// Run Propose in a goroutine so we can tick the manual-clock cluster while
	// waiting for the apply pipeline to complete.
	type result struct {
		val []byte
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		v, e := leader.Propose(ctx, cmd)
		resultCh <- result{v, e}
	}()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case r := <-resultCh:
			return r.val, r.err
		case <-ticker.C:
			c.Tick()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Disconnect partitions node i from the rest of the cluster.
func (c *Cluster) Disconnect(i int) {
	c.net.Partition(c.ids[i])
}

// Reconnect heals all partitions for node i.
func (c *Cluster) Reconnect(i int) {
	c.net.Heal(c.ids[i])
}

// DropLink drops messages from node from to node to (one direction only).
// This simulates an asymmetric network failure; the reverse direction is
// unaffected. Use RestoreLink to undo.
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
func (sm *kvSM) Snapshot(_ context.Context) ([]byte, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	var sb strings.Builder
	for k, v := range sm.data {
		fmt.Fprintf(&sb, "%s\t%s\n", k, v)
	}
	return []byte(sb.String()), nil
}

// Restore replaces the map with the snapshot contents.
func (sm *kvSM) Restore(_ context.Context, _ raft.SnapshotMeta, data []byte) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.data = make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 {
			sm.data[parts[0]] = parts[1]
		}
	}
	return nil
}
