// Package easyrafttest runs easyraft clusters inside a test process.
//
// Testing a service built on a consensus library usually means either mocking
// the library away -- which tests the mock -- or standing up real nodes on
// real ports, which is slow, flaky under parallel test runs, and gives no way
// to cut the network. This package gives the real thing without either cost:
// every node is a real [easyraft.Store] running the real engine, and the
// network between them is an in-memory one a test can partition and heal.
//
// A cluster in three lines:
//
//	c := easyrafttest.NewCluster(t, 3)
//	users := easyrafttest.AddCollection[User](c, "users")
//	err := users.Leader().Create(ctx, "alice", User{Name: "Alice"})
//
// [NewCluster] builds the nodes, starts them, waits for a leader, and
// registers the shutdown with the test. Nothing needs cleaning up by hand.
//
// When mutations have to be registered before the nodes start -- and they do,
// because a mutation must exist on every replica before any entry can call it
// -- build the cluster in two steps:
//
//	c := easyrafttest.New(t, 3)
//	counters := easyrafttest.AddCollection[Counter](c, "counters")
//	counters.RegisterMutation("increment", increment)
//	c.Start()
//
// # Time
//
// Clusters use short Raft timings by default (see [Options.Timing]) so an
// election takes tens of milliseconds rather than seconds. That is a real
// election with real messages, not a simulated one.
package easyrafttest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// DefaultTimeout is how long the waiting helpers give the cluster before
// failing the test. Generous on purpose: what they wait for is milliseconds
// of work, and a failure should mean stuck rather than busy.
const DefaultTimeout = 30 * time.Second

// Options configures a [Cluster]. The zero value is the default in every
// field.
type Options struct {
	// Timing overrides the Raft tick, heartbeat and election timeouts, in the
	// order [easyraft.WithRaftTiming] takes them. Zero uses timings short
	// enough that an election is tens of milliseconds.
	Timing [4]time.Duration

	// Store is applied to every node, after everything the harness sets. Use
	// it for the options under test -- [easyraft.WithLeaseReads], a commit
	// quorum, a client-table bound.
	Store []easyraft.Option

	// PerNode is applied to node i after Store, for the cases where nodes
	// must differ: one witness, one preferred leader, one configured wrong on
	// purpose.
	PerNode func(i int) []easyraft.Option

	// Logger receives the nodes' logs. Nil discards them, which is what a
	// passing test wants; pass one when a failing test needs to say why.
	Logger *slog.Logger

	// Timeout overrides [DefaultTimeout] for this cluster's waiting helpers.
	Timeout time.Duration
}

// Cluster is a set of easyraft nodes on one in-memory network.
//
// Every exported method fails the test rather than returning an error: a
// harness that hands back errors for the harness's own failures turns every
// test into error-checking for things the test was not about.
type Cluster struct {
	// Stores are the nodes, in the order they were created. Their IDs are
	// "n1", "n2", and so on.
	Stores []*easyraft.Store
	// IDs are the node IDs, positionally matching Stores.
	IDs []raft.NodeID

	t       *testing.T
	net     *memtransport.Network
	dirs    []string
	peers   map[raft.NodeID]string
	opts    Options
	started bool
	stopped []bool
}

// NewCluster builds an n-node cluster, starts it, and waits for a leader.
// The cluster is stopped when the test ends.
func NewCluster(t *testing.T, n int, opts ...Options) *Cluster {
	t.Helper()
	c := New(t, n, opts...)
	c.Start()
	return c
}

// New builds an n-node cluster without starting it, so that mutations and
// change handlers can be registered first. Call [Cluster.Start] after.
func New(t *testing.T, n int, opts ...Options) *Cluster {
	t.Helper()
	if n < 1 {
		t.Fatalf("easyrafttest: a cluster needs at least one node, got %d", n)
	}
	if len(opts) > 1 {
		t.Fatalf("easyrafttest: pass at most one Options, got %d", len(opts))
	}

	c := &Cluster{
		t:       t,
		net:     memtransport.NewNetwork(),
		peers:   make(map[raft.NodeID]string, n),
		stopped: make([]bool, n),
	}
	if len(opts) == 1 {
		c.opts = opts[0]
	}
	if c.opts.Timing == [4]time.Duration{} {
		// Short enough that an election is tens of milliseconds, and still
		// far enough apart that a heartbeat comfortably beats the election
		// timeout -- otherwise a cluster with nothing wrong with it elects
		// over and over and the test measures election storms.
		c.opts.Timing = [4]time.Duration{
			10 * time.Millisecond,  // tick
			20 * time.Millisecond,  // heartbeat
			150 * time.Millisecond, // election timeout, low
			300 * time.Millisecond, // election timeout, high
		}
	}
	if c.opts.Timeout <= 0 {
		c.opts.Timeout = DefaultTimeout
	}

	root := t.TempDir()
	for i := range n {
		id := raft.NodeID(fmt.Sprintf("n%d", i+1))
		c.IDs = append(c.IDs, id)
		c.dirs = append(c.dirs, filepath.Join(root, string(id)))
		// The in-memory network routes by node ID, so the address is one --
		// it goes into the peer table and is never dialled.
		c.peers[id] = string(id)
	}
	for i := range n {
		c.Stores = append(c.Stores, c.build(i))
	}

	t.Cleanup(c.Stop)
	return c
}

// build constructs node i. Used by New and again by Restart, so a restarted
// node is configured exactly like the one it replaces.
func (c *Cluster) build(i int) *easyraft.Store {
	c.t.Helper()

	logger := c.opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	opts := []easyraft.Option{
		easyraft.WithID(c.IDs[i]),
		easyraft.WithDataDir(c.dirs[i]),
		easyraft.WithPeers(c.peers),
		easyraft.WithTransport(c.net.NewTransport(c.IDs[i])),
		easyraft.WithRaftTiming(c.opts.Timing[0], c.opts.Timing[1], c.opts.Timing[2], c.opts.Timing[3]),
		easyraft.WithLogger(logger),
	}
	opts = append(opts, c.opts.Store...)
	if c.opts.PerNode != nil {
		opts = append(opts, c.opts.PerNode(i)...)
	}

	store, err := easyraft.NewStore(opts...)
	if err != nil {
		c.t.Fatalf("easyrafttest: build node %s: %v", c.IDs[i], err)
	}
	return store
}

// Start starts every node and waits for a leader.
func (c *Cluster) Start() {
	c.t.Helper()
	if c.started {
		c.t.Fatal("easyrafttest: the cluster is already started")
	}
	c.started = true
	for i, s := range c.Stores {
		if err := s.Start(); err != nil {
			c.t.Fatalf("easyrafttest: start node %s: %v", c.IDs[i], err)
		}
	}
	c.WaitLeader()
}

// Stop stops every node that is still running. Registered with the test by
// [New], so a test rarely calls it.
func (c *Cluster) Stop() {
	for i, s := range c.Stores {
		if c.stopped[i] || s == nil {
			continue
		}
		c.stopped[i] = true
		if err := s.Stop(); err != nil {
			// Reported, not fatal: Stop runs during cleanup, where failing the
			// test would mask whatever the test itself found.
			c.t.Errorf("easyrafttest: stop node %s: %v", c.IDs[i], err)
		}
	}
}

// Node returns node i, failing the test if there is no such node.
func (c *Cluster) Node(i int) *easyraft.Store {
	c.t.Helper()
	if i < 0 || i >= len(c.Stores) {
		c.t.Fatalf("easyrafttest: node %d does not exist in a cluster of %d", i, len(c.Stores))
	}
	return c.Stores[i]
}

// StopNode stops one node and leaves it stopped. The rest of the cluster
// carries on, which is what a test of a failure is for.
func (c *Cluster) StopNode(i int) {
	c.t.Helper()
	c.Node(i)
	if c.stopped[i] {
		return
	}
	c.stopped[i] = true
	if err := c.Stores[i].Stop(); err != nil {
		c.t.Fatalf("easyrafttest: stop node %s: %v", c.IDs[i], err)
	}
	c.net.Unregister(c.IDs[i])
}

// RestartNode brings a stopped node back from its own data directory, as a
// process restarting would. It returns the new [easyraft.Store]: the old one
// is finished, and any handle a test still holds to it is stale.
//
// Typed collections have to be added again on the new store; see
// [Collections.Rebind].
func (c *Cluster) RestartNode(i int) *easyraft.Store {
	c.t.Helper()
	c.Node(i)
	if !c.stopped[i] {
		c.t.Fatalf("easyrafttest: node %s is running; stop it before restarting it", c.IDs[i])
	}
	c.Stores[i] = c.build(i)
	if err := c.Stores[i].Start(); err != nil {
		c.t.Fatalf("easyrafttest: restart node %s: %v", c.IDs[i], err)
	}
	c.stopped[i] = false
	return c.Stores[i]
}

// Partition cuts node i off from every other node, in both directions. It
// keeps running and keeps its state; it simply cannot be heard or heard from.
func (c *Cluster) Partition(i int) {
	c.t.Helper()
	c.Node(i)
	c.net.Partition(c.IDs[i])
}

// Heal restores the links [Cluster.Partition] cut.
func (c *Cluster) Heal(i int) {
	c.t.Helper()
	c.Node(i)
	c.net.Heal(c.IDs[i])
}

// Drop discards messages from node i to node j, one direction only. Use it
// for the asymmetric failures that a whole-node partition cannot express --
// the ones where a follower can hear the leader but cannot answer.
func (c *Cluster) Drop(i, j int) {
	c.t.Helper()
	c.Node(i)
	c.Node(j)
	c.net.Drop(c.IDs[i], c.IDs[j])
}

// Restore undoes one [Cluster.Drop].
func (c *Cluster) Restore(i, j int) {
	c.t.Helper()
	c.Node(i)
	c.Node(j)
	c.net.Restore(c.IDs[i], c.IDs[j])
}

// Leader returns the node that is currently leader, or nil if none is. Use
// [Cluster.WaitLeader] unless the absence of a leader is the thing being
// checked.
func (c *Cluster) Leader() *easyraft.Store {
	for i, s := range c.Stores {
		if c.stopped[i] || s == nil {
			continue
		}
		if s.Status().State == raft.Leader {
			return s
		}
	}
	return nil
}

// LeaderIndex returns the index of the current leader, or -1 if there is none.
func (c *Cluster) LeaderIndex() int {
	for i, s := range c.Stores {
		if c.stopped[i] || s == nil {
			continue
		}
		if s.Status().State == raft.Leader {
			return i
		}
	}
	return -1
}

// WaitLeader waits until one node is leader and returns it, failing the test
// if none appears.
func (c *Cluster) WaitLeader() *easyraft.Store {
	c.t.Helper()
	var leader *easyraft.Store
	c.wait("a leader to be elected", func() bool {
		leader = c.Leader()
		return leader != nil
	})
	return leader
}

// WaitNoLeader waits until no node considers itself leader. It is how a test
// checks that a minority really has lost the ability to write, rather than
// assuming it from the partition it just created.
func (c *Cluster) WaitNoLeader() {
	c.t.Helper()
	c.wait("the cluster to lose its leader", func() bool {
		return c.Leader() == nil
	})
}

// Followers returns every running node that is not the leader.
func (c *Cluster) Followers() []*easyraft.Store {
	var out []*easyraft.Store
	for i, s := range c.Stores {
		if c.stopped[i] || s == nil || s.Status().State == raft.Leader {
			continue
		}
		out = append(out, s)
	}
	return out
}

// WaitApplied waits until every running node has applied at least the
// revision the leader has, so that a test can read from a follower and know
// it is not reading the past.
func (c *Cluster) WaitApplied() {
	c.t.Helper()
	c.wait("every node to catch up with the leader", func() bool {
		leader := c.Leader()
		if leader == nil {
			return false
		}
		want := leader.Revision()
		for i, s := range c.Stores {
			if c.stopped[i] || s == nil {
				continue
			}
			if s.Revision() < want {
				return false
			}
		}
		return true
	})
}

// Ready waits until the leader can serve a linearizable read, which is the
// point at which the cluster is fully usable.
func (c *Cluster) Ready() {
	c.t.Helper()
	leader := c.WaitLeader()
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()
	if err := leader.Ready(ctx); err != nil {
		c.t.Fatalf("easyrafttest: the cluster did not become ready: %v", err)
	}
}

// Context returns a context carrying this cluster's timeout, cancelled when
// the test ends. It saves every test writing the same four lines.
func (c *Cluster) Context() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	c.t.Cleanup(cancel)
	return ctx
}

// wait polls until done reports true, or fails the test naming what it was
// waiting for.
func (c *Cluster) wait(what string, done func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(c.opts.Timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	c.t.Fatalf("easyrafttest: timed out after %v waiting for %s; %s",
		c.opts.Timeout, what, c.describe())
}

// describe reports what each node thinks, so a timeout says something more
// useful than that it timed out.
func (c *Cluster) describe() string {
	out := "node states:"
	for i, s := range c.Stores {
		switch {
		case c.stopped[i] || s == nil:
			out += fmt.Sprintf(" %s=stopped", c.IDs[i])
		default:
			status := s.Status()
			out += fmt.Sprintf(" %s=%v(term %d, applied %d)",
				c.IDs[i], status.State, status.Term, s.Revision())
		}
	}
	return out
}
