package raft_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
	"github.com/brunoga/raft/transport/memtransport"
)

// recordingMetrics implements raft.Metrics and raft.ProposalMetrics.
type recordingMetrics struct {
	mu        sync.Mutex
	latencies []time.Duration
	outcomes  []bool
	snapshots []int

	storage           map[string]int
	negativeDurations int
}

func (m *recordingMetrics) StateChange(raft.NodeID, raft.State, raft.State, raft.Term) {}
func (m *recordingMetrics) CommitAdvanced(raft.NodeID, raft.Index)                     {}

func (m *recordingMetrics) SnapshotTaken(_ raft.NodeID, _ raft.Index, sizeBytes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots = append(m.snapshots, sizeBytes)
}

func (m *recordingMetrics) StorageWrite(_ raft.NodeID, op string, d time.Duration, _ error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.storage == nil {
		m.storage = make(map[string]int)
	}
	m.storage[op]++
	if d < 0 {
		m.negativeDurations++
	}
}

func (m *recordingMetrics) storageWrites() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.storage))
	for k, v := range m.storage {
		out[k] = v
	}
	return out
}

func (m *recordingMetrics) ProposalCompleted(_ raft.NodeID, latency time.Duration, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latencies = append(m.latencies, latency)
	m.outcomes = append(m.outcomes, ok)
}

func (m *recordingMetrics) snapshot() (latencies []time.Duration, outcomes []bool, snapshotSizes []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Duration(nil), m.latencies...),
		append([]bool(nil), m.outcomes...),
		append([]int(nil), m.snapshots...)
}

// metricsSM produces a snapshot with a known, non-trivial size.
type metricsSM struct{}

func (metricsSM) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }
func (metricsSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write(make([]byte, 4096))
	return err
}
func (metricsSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

func metricsNode(t *testing.T, m raft.Metrics, tune func(*raft.Config)) *raft.Node {
	t.Helper()

	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = metricsSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport("n1")
	cfg.TickInterval = 0
	cfg.Metrics = m
	cfg.ElectionTimeoutMin = 20 * time.Millisecond
	cfg.ElectionTimeoutMax = 40 * time.Millisecond
	cfg.HeartbeatInterval = 10 * time.Millisecond
	if tune != nil {
		tune(&cfg)
	}

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	deadline := time.Now().Add(3 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("node never became leader")
		}
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	return node
}

// TestProposalMetrics_ReportsEveryProposal asserts that a proposal's duration
// is reported whether it succeeds or fails.
//
// How long a write takes from submission to being applied is the number an
// operator watches, and it is not derivable from the commit index: it covers
// the queue at the event loop, the durable append, the replication round-trip
// and the state machine, which are exactly the parts that get slow.
func TestProposalMetrics_ReportsEveryProposal(t *testing.T) {
	ctx := context.Background()
	m := &recordingMetrics{}
	node := metricsNode(t, m, nil)

	const proposals = 5
	for range proposals {
		if _, err := node.Propose(ctx, []byte("x")); err != nil {
			t.Fatalf("propose: %v", err)
		}
	}

	latencies, outcomes, _ := m.snapshot()
	if len(latencies) < proposals {
		t.Fatalf("%d proposals reported for %d made", len(latencies), proposals)
	}
	for i, d := range latencies {
		if d <= 0 {
			t.Errorf("proposal %d reported a duration of %v", i, d)
		}
		if d > time.Minute {
			t.Errorf("proposal %d reported an implausible duration of %v", i, d)
		}
	}
	for i, ok := range outcomes {
		if !ok {
			t.Errorf("proposal %d reported as failed", i)
		}
	}
}

// TestProposalMetrics_ReportsFailures asserts that a rejected proposal is
// reported too, so that a rising failure rate is visible.
func TestProposalMetrics_ReportsFailures(t *testing.T) {
	ctx := context.Background()
	m := &recordingMetrics{}

	// A follower with peers: it never wins an election, so every proposal is
	// refused.
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
	cfg.Storage = memstore.New()
	cfg.StateMachine = metricsSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport("n1")
	cfg.TickInterval = 0
	cfg.Metrics = m

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	if _, err := node.Propose(ctx, []byte("x")); err == nil {
		t.Fatal("a follower accepted a proposal")
	}

	_, outcomes, _ := m.snapshot()
	if len(outcomes) == 0 {
		t.Fatal("a refused proposal was not reported at all")
	}
	if outcomes[0] {
		t.Error("a refused proposal was reported as successful")
	}
}

// TestSnapshotMetrics_ReportTheirRealSize asserts that the size handed to
// SnapshotTaken is the size of the snapshot, not zero.
//
// Snapshot size is what decides how long a lagging follower takes to catch up,
// and reporting a constant zero makes the metric worse than absent: it reads
// like a measurement.
func TestSnapshotMetrics_ReportTheirRealSize(t *testing.T) {
	ctx := context.Background()
	m := &recordingMetrics{}
	node := metricsNode(t, m, func(cfg *raft.Config) {
		cfg.SnapshotThreshold = 4
	})

	for range 10 {
		if _, err := node.Propose(ctx, []byte("x")); err != nil {
			t.Fatalf("propose: %v", err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for node.SnapshotIndex() == 0 && time.Now().Before(deadline) {
		node.Tick()
		time.Sleep(time.Millisecond)
	}

	_, _, sizes := m.snapshot()
	if len(sizes) == 0 {
		t.Fatal("no snapshot was reported")
	}
	for i, size := range sizes {
		if size < 4096 {
			t.Errorf("snapshot %d reported %d bytes; the state machine alone writes 4096", i, size)
		}
	}
}

// TestStorageMetrics_ReportsDurableWrites asserts that the writes the event
// loop waits on are timed and reported.
//
// These are the ones that matter: a slow disk delays the term and vote write
// and the log append, and with them heartbeats and every inbound RPC. Without
// this, a node whose disk has degraded looks identical to one on a slow
// network.
func TestStorageMetrics_ReportsDurableWrites(t *testing.T) {
	ctx := context.Background()
	m := &recordingMetrics{}
	node := metricsNode(t, m, func(cfg *raft.Config) {
		cfg.SnapshotThreshold = 4
	})

	for range 8 {
		if _, err := node.Propose(ctx, []byte("x")); err != nil {
			t.Fatalf("propose: %v", err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for node.SnapshotIndex() == 0 && time.Now().Before(deadline) {
		node.Tick()
		time.Sleep(time.Millisecond)
	}

	writes := m.storageWrites()
	for _, op := range []string{"hardstate", "append", "snapshot"} {
		if writes[op] == 0 {
			t.Errorf("no %q write was reported; observed %v", op, writes)
		}
	}
}
