package raft_test

// The cluster harness used by the fault-injection tests: a set of Raft nodes
// wired to a seeded simulated network (internal/simnet), each on a storage
// wrapper that can be crashed, and all of them watched by the invariant checker
// in siminvariant_test.go.
//
// It is deliberately separate from the Cluster type in cluster_test.go. That
// one exists to make well-behaved integration tests read clearly; this one
// exists to make a cluster misbehave in specific ways and to notice when the
// result is not merely slow but wrong.
//
// Reproducing a failure
//
//	RAFT_SIM_SEED=<seed printed by the failure> go test -run <TestName> -count=1 .
//
// See the package documentation of internal/simnet for what a seed does and
// does not guarantee.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/internal/simnet"
	"github.com/brunoga/raft/storage/memstore"
)

// simSeed returns the seed for this run: the value of RAFT_SIM_SEED when set,
// otherwise a fresh random one. Tests print whichever they used on failure.
func simSeed(t testing.TB) uint64 {
	t.Helper()
	if s := os.Getenv("RAFT_SIM_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 0, 64)
		if err != nil {
			t.Fatalf("RAFT_SIM_SEED=%q: %v", s, err)
		}
		return v
	}
	return rand.Uint64()
}

// ---- State machine ---------------------------------------------------------

// encodePut builds the command for key = value.
func encodePut(key, value string) []byte {
	return fmt.Appendf(nil, "put\x00%s\x00%s", key, value)
}

func decodePut(cmd []byte) (key, value string, ok bool) {
	parts := strings.SplitN(string(cmd), "\x00", 3)
	if len(parts) != 3 || parts[0] != "put" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// simKV is the key-value state machine the fault-injection tests run on. Beyond
// holding the data it reports every entry it applies to the invariant checker,
// which is what makes State Machine Safety directly checkable instead of
// inferred from logs.
type simKV struct {
	id raft.NodeID
	ck *invariantChecker

	mu   sync.RWMutex
	data map[string]string
}

func newSimKV(id raft.NodeID, ck *invariantChecker) *simKV {
	return &simKV{id: id, ck: ck, data: make(map[string]string)}
}

func (s *simKV) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	if s.ck != nil {
		s.ck.recordApply(s.id, e)
	}
	key, value, ok := decodePut(e.Command)
	if !ok {
		return nil, nil // no-op entry
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return []byte(value), nil
}

// Get returns the locally applied value for key.
func (s *simKV) Get(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[key]
}

func (s *simKV) Snapshot(_ context.Context, w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.data {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *simKV) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = make(map[string]string)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			if k, v, found := strings.Cut(line, "\t"); found {
				s.data[k] = v
			}
		}
	}
	return sc.Err()
}

// reset clears the in-memory state, as a power cut would. The node rebuilds it
// by replaying its log from the last snapshot, which is the behaviour a
// crash-restart is supposed to exercise.
func (s *simKV) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = make(map[string]string)
}

// ---- Cluster ---------------------------------------------------------------

// simNode is one member: the parts that survive a restart (identity, storage,
// state machine) plus the current Node, which does not.
type simNode struct {
	id    raft.NodeID
	store *simnet.FaultStore
	sm    *simKV
	peers []raft.PeerConfig

	mu      sync.Mutex
	node    *raft.Node
	running bool
}

func (sn *simNode) get() (*raft.Node, bool) {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	return sn.node, sn.running
}

// simConfig configures a simCluster. The zero value is not useful; use
// defaultSimConfig and override.
type simConfig struct {
	// nodes is the number of cluster members.
	nodes int
	// seed drives the simulated network.
	seed uint64
	// policy is the default per-link behaviour.
	policy simnet.LinkPolicy
	// tickEvery is the wall-clock period of one logical tick. Nodes are
	// configured with TickInterval 0 and driven by the harness. Set it to 0 to
	// drive ticks by hand, which is how a test decides which node times out
	// first and therefore which node wins an election.
	tickEvery time.Duration
	// snapshotThreshold is copied into each node's Config; 0 disables
	// automatic snapshots, which keeps the checker's log comparisons total.
	snapshotThreshold uint64
	// mutate gets a last look at each node's Config before it is created.
	mutate func(i int, cfg *raft.Config)
	// preseed writes a starting log and hard state into member i's storage
	// before its Node is built. It is how the scenario tests construct a
	// cluster that is already halfway through a history, which is far more
	// reliable than hoping random faults produce the state of interest.
	preseed func(i int, store raft.Storage)
}

func defaultSimConfig(seed uint64) simConfig {
	return simConfig{
		nodes:     3,
		seed:      seed,
		policy:    simnet.Lossless(),
		tickEvery: time.Millisecond,
	}
}

// simCluster is a running cluster on a simulated network, under observation.
type simCluster struct {
	t    testing.TB
	cfg  simConfig
	net  *simnet.Network
	ck   *invariantChecker
	seed uint64

	nodes []*simNode
	ids   []raft.NodeID

	stopTick chan struct{}
	tickDone chan struct{}
	stopOnce sync.Once
}

// newSimCluster builds and starts a cluster. Everything is torn down and the
// final invariant sweep is run through t.Cleanup.
func newSimCluster(t testing.TB, cfg simConfig) *simCluster {
	t.Helper()
	c := &simCluster{
		t:        t,
		cfg:      cfg,
		net:      simnet.New(cfg.seed),
		seed:     cfg.seed,
		stopTick: make(chan struct{}),
		tickDone: make(chan struct{}),
	}
	c.ck = newInvariantChecker(t, c.diagnostics)
	c.net.SetDefaultPolicy(cfg.policy)

	for i := range cfg.nodes {
		c.ids = append(c.ids, raft.NodeID(fmt.Sprintf("s%d", i+1)))
	}
	for i := range cfg.nodes {
		id := c.ids[i]
		peers := make([]raft.PeerConfig, 0, cfg.nodes-1)
		for j := range cfg.nodes {
			if i != j {
				peers = append(peers, raft.PeerConfig{ID: c.ids[j], Voter: true})
			}
		}
		inner := memstore.New()
		if cfg.preseed != nil {
			cfg.preseed(i, inner)
		}
		store := simnet.NewFaultStore(inner)
		sn := &simNode{id: id, store: store, sm: newSimKV(id, c.ck), peers: peers}
		c.nodes = append(c.nodes, sn)
		c.ck.addNode(id, store)
	}

	for i := range c.nodes {
		c.start(i)
	}

	// Logged rather than printed: `go test` shows it only when the test fails
	// or runs verbosely, which is exactly when it is wanted.
	t.Logf("simulated network seed %d (replay with RAFT_SIM_SEED=%d)", cfg.seed, cfg.seed)

	c.ck.start(5 * time.Millisecond)
	if cfg.tickEvery > 0 {
		go c.tickLoop()
	} else {
		close(c.tickDone)
	}

	t.Cleanup(c.shutdown)
	return c
}

// diagnostics is the footer printed with every failure: enough to replay.
func (c *simCluster) diagnostics() string {
	var b strings.Builder
	fmt.Fprintf(&b, "reproduce with: RAFT_SIM_SEED=%d go test -run %s -count=1 .\n", c.seed, c.t.Name())
	b.WriteString("fault schedule:\n")
	if s := c.net.FaultSchedule(); s != "" {
		b.WriteString(s)
		b.WriteString("\n")
	}
	if s := c.net.RecentEvents(40); s != "" {
		b.WriteString("recent network events:\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	return b.String()
}

// buildConfig assembles the Config for member i. It is called again on every
// restart, because a Node cannot be restarted — only replaced.
func (c *simCluster) buildConfig(i int) raft.Config {
	sn := c.nodes[i]
	cfg := raft.DefaultConfig()
	cfg.ID = sn.id
	cfg.Peers = append([]raft.PeerConfig(nil), sn.peers...)
	cfg.Storage = sn.store
	cfg.StateMachine = sn.sm
	cfg.Transport = c.net.NewTransport(sn.id)
	cfg.TickInterval = 0
	cfg.SnapshotThreshold = c.cfg.snapshotThreshold
	cfg.Metrics = c.ck.metrics()
	cfg.Logger = discardLogger()
	if c.cfg.mutate != nil {
		c.cfg.mutate(i, &cfg)
	}
	return cfg
}

// start creates and starts member i.
func (c *simCluster) start(i int) {
	c.t.Helper()
	sn := c.nodes[i]
	cfg := c.buildConfig(i)
	n, err := raft.New(&cfg)
	if err != nil {
		c.t.Fatalf("raft.New(%s): %v", sn.id, err)
	}
	sn.mu.Lock()
	sn.node = n
	sn.running = true
	sn.mu.Unlock()
	c.ck.attach(sn.id, n)
	n.Start()
}

// crash stops member i the way a power cut would: the node goes away, the
// storage keeps exactly what it had made durable, and the in-memory state
// machine is lost.
func (c *simCluster) crash(i int) {
	c.t.Helper()
	sn := c.nodes[i]
	sn.mu.Lock()
	n, running := sn.node, sn.running
	sn.running = false
	sn.node = nil
	sn.mu.Unlock()
	if !running || n == nil {
		return
	}
	c.net.RecordFault("crash %s", sn.id)
	c.ck.detach(sn.id)
	n.Stop()
	// Record the membership as the node last understood it so a restart comes
	// back with the right peer set after a configuration change.
	sn.peers = peersExcluding(n.Members(), sn.id)
	if err := sn.store.Crash(context.Background()); err != nil {
		c.t.Fatalf("store.Crash(%s): %v", sn.id, err)
	}
	sn.sm.reset()
}

// restart brings member i back on the storage it crashed with.
func (c *simCluster) restart(i int) {
	c.t.Helper()
	if _, running := c.nodes[i].get(); running {
		return
	}
	c.net.RecordFault("restart %s", c.nodes[i].id)
	c.start(i)
}

func peersExcluding(members []raft.PeerConfig, self raft.NodeID) []raft.PeerConfig {
	out := make([]raft.PeerConfig, 0, len(members))
	for _, p := range members {
		if p.ID != self {
			out = append(out, p)
		}
	}
	return out
}

// tickLoop drives every running node's logical clock.
func (c *simCluster) tickLoop() {
	defer close(c.tickDone)
	tk := time.NewTicker(c.cfg.tickEvery)
	defer tk.Stop()
	for {
		select {
		case <-c.stopTick:
			return
		case <-tk.C:
			for _, sn := range c.nodes {
				if n, running := sn.get(); running && n != nil {
					n.Tick()
				}
			}
		}
	}
}

// shutdown stops the world in the order that keeps the final sweep meaningful:
// ticking first, then the nodes, then the checker's last look at the durable
// logs, then the network.
func (c *simCluster) shutdown() {
	c.stopOnce.Do(func() {
		close(c.stopTick)
		<-c.tickDone
		for i := range c.nodes {
			sn := c.nodes[i]
			sn.mu.Lock()
			n, running := sn.node, sn.running
			sn.running = false
			sn.node = nil
			sn.mu.Unlock()
			if running && n != nil {
				n.Stop()
			}
			c.ck.detach(sn.id)
		}
		c.ck.stop()
		c.ck.finish()
		_ = c.net.Close()
	})
}

// tick advances member i's logical clock by one, if it is running.
func (c *simCluster) tick(i int) {
	if n, running := c.nodes[i].get(); running && n != nil {
		n.Tick()
	}
}

// tickAll advances every running member's clock by one.
func (c *simCluster) tickAll() {
	for i := range c.nodes {
		c.tick(i)
	}
}

// setFilters installs the conjunction of rules as the network's delivery
// predicate: a message is delivered only if every rule accepts it.
func (c *simCluster) setFilters(rules ...func(simnet.Message) bool) {
	if len(rules) == 0 {
		c.net.SetFilter(nil)
		return
	}
	c.net.SetFilter(func(m simnet.Message) bool {
		for _, r := range rules {
			if !r(m) {
				return false
			}
		}
		return true
	})
}

// onlyCampaigner is a filter rule that delivers RequestVote RPCs only when they
// come from id.
//
// It is how these tests decide who wins an election. The library seeds its
// election timeouts from an unseeded generator, so which follower times out
// first is not something a test can choose; silencing everybody else's
// campaign is. Nothing else is affected: the chosen node still has to win the
// vote on the merits of its log.
func onlyCampaigner(id raft.NodeID) func(simnet.Message) bool {
	return func(m simnet.Message) bool {
		if _, ok := m.Req.(*raft.RequestVoteRequest); ok {
			return m.From == id
		}
		return true
	}
}

// electLeader drives member i to leadership by suppressing every other node's
// campaign until it wins, then leaves rules in place as the standing filter.
func (c *simCluster) electLeader(i int, timeout time.Duration, rules ...func(simnet.Message) bool) {
	c.t.Helper()
	c.setFilters(append([]func(simnet.Message) bool{onlyCampaigner(c.ids[i])}, rules...)...)
	defer c.setFilters(rules...)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n := c.node(i); n != nil && n.State() == raft.Leader {
			return
		}
		if c.cfg.tickEvery == 0 {
			c.tickAll()
		}
		time.Sleep(time.Millisecond)
	}
	c.t.Fatalf("%s did not become leader within %s\n%s", c.ids[i], timeout, c.diagnostics())
}

// tickFor keeps the cluster running for d, ticking by hand when the harness is
// in manual-tick mode.
func (c *simCluster) tickFor(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.cfg.tickEvery == 0 {
			c.tickAll()
		}
		time.Sleep(time.Millisecond)
	}
}

// ---- Observation -----------------------------------------------------------

// leaderIndex returns the index of a node currently claiming leadership, or -1.
func (c *simCluster) leaderIndex() int {
	for i, sn := range c.nodes {
		if n, running := sn.get(); running && n != nil && n.State() == raft.Leader {
			return i
		}
	}
	return -1
}

// waitLeader blocks until some node claims leadership, or the deadline passes.
// It returns -1 on timeout rather than failing, because several tests are
// specifically about periods when there is no leader.
func (c *simCluster) waitLeader(timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if i := c.leaderIndex(); i >= 0 {
			return i
		}
		time.Sleep(time.Millisecond)
	}
	return -1
}

// node returns member i's current Node, or nil if it is down.
func (c *simCluster) node(i int) *raft.Node {
	n, running := c.nodes[i].get()
	if !running {
		return nil
	}
	return n
}

func (c *simCluster) indexOf(id raft.NodeID) int {
	for i, cid := range c.ids {
		if cid == id {
			return i
		}
	}
	return -1
}

// seedLog writes a starting hard state and log into a storage backend. The
// entries must be contiguous and start at index 1.
func seedLog(t testing.TB, s raft.Storage, hs raft.HardState, entries ...raft.LogEntry) {
	t.Helper()
	ctx := context.Background()
	if err := s.SaveHardState(ctx, hs); err != nil {
		t.Fatalf("seedLog: SaveHardState: %v", err)
	}
	if len(entries) > 0 {
		if err := s.AppendLogEntries(ctx, entries); err != nil {
			t.Fatalf("seedLog: AppendLogEntries: %v", err)
		}
	}
}

// entry builds a log entry.
func entry(index raft.Index, term raft.Term, cmd []byte) raft.LogEntry {
	return raft.LogEntry{Index: index, Term: term, Command: cmd}
}

// storeHas reports whether a member's durable log holds an entry at index with
// the given term.
func (c *simCluster) storeHas(i int, index raft.Index, term raft.Term) bool {
	e, err := c.nodes[i].store.GetLogEntry(context.Background(), index)
	return err == nil && e.Term == term
}

// countStoresWith returns how many members durably hold (index, term).
func (c *simCluster) countStoresWith(index raft.Index, term raft.Term) int {
	count := 0
	for i := range c.nodes {
		if c.storeHas(i, index, term) {
			count++
		}
	}
	return count
}

// ---- Client ----------------------------------------------------------------

// opStatus is the client's knowledge of what happened to a write.
type opStatus int

const (
	// opOK means the write was applied and the result was returned.
	opOK opStatus = iota
	// opUnknown means the client never learned the outcome: the call failed,
	// timed out, or the node went away. The write may or may not be in the log.
	opUnknown
)

// put writes key = value through whichever node is currently the leader,
// following NotLeaderError hints and retrying until the deadline.
//
// The same value is used for every attempt, so a retry that lands alongside an
// earlier attempt that also landed is indistinguishable from a single write.
// That is what lets the caller collapse a whole retry loop into one
// linearizability history operation.
func (c *simCluster) put(ctx context.Context, key, value string) opStatus {
	cmd := encodePut(key, value)
	start := c.leaderIndex()
	if start < 0 {
		start = 0
	}
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return opUnknown
		}
		idx := c.pickTarget(start, attempt)
		n := c.node(idx)
		if n == nil {
			time.Sleep(time.Millisecond)
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		_, err := n.Propose(callCtx, cmd)
		cancel()
		if err == nil {
			return opOK
		}
		// Any failure leaves the outcome open: the entry may already be in the
		// leader's log and may yet commit. Recording the operation as unknown
		// rather than dropping it is the whole point — a dropped operation is
		// a linearizability violation the checker will never see.
		if hint := leaderHint(err); hint != "" {
			if i := c.indexOf(hint); i >= 0 {
				start = i
			}
		}
		select {
		case <-ctx.Done():
			return opUnknown
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// get performs a linearizable read: a ReadIndex round-trip to establish a
// commit fence, then a read of the local state machine once it has caught up.
func (c *simCluster) get(ctx context.Context, key string) (string, bool) {
	start := c.leaderIndex()
	if start < 0 {
		start = 0
	}
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return "", false
		}
		idx := c.pickTarget(start, attempt)
		n := c.node(idx)
		if n == nil {
			time.Sleep(time.Millisecond)
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		_, err := n.ReadIndex(callCtx)
		cancel()
		if err == nil {
			return c.nodes[idx].sm.Get(key), true
		}
		if hint := leaderHint(err); hint != "" {
			if i := c.indexOf(hint); i >= 0 {
				start = i
			}
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// pickTarget returns the node to try on a given attempt: the believed leader
// first, then every other member in turn.
func (c *simCluster) pickTarget(start, attempt int) int {
	if attempt == 0 {
		return start
	}
	if i := c.leaderIndex(); i >= 0 {
		return i
	}
	return (start + attempt) % len(c.nodes)
}

// leaderHint extracts the leader identity a NotLeaderError carries, if any.
func leaderHint(err error) raft.NodeID {
	var nle *raft.NotLeaderError
	if errors.As(err, &nle) && nle != nil {
		return nle.Leader
	}
	return ""
}

// discardLogger silences a node's own structured logging. Chaos runs generate
// tens of thousands of routine lines; the invariant checker's diagnostics carry
// the context that actually matters when one of these tests fails.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
