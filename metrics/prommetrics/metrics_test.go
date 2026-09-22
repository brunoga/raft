package prommetrics_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/metrics/prommetrics"
)

func TestMetrics_StateChange(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)

	m.StateChange("n1", raft.Follower, raft.Candidate, 1)
	m.StateChange("n1", raft.Candidate, raft.Leader, 1)

	// Verify transition counters.
	expected := `
# HELP raft_state_transitions_total Total number of role transitions, labelled by from and to state.
# TYPE raft_state_transitions_total counter
raft_state_transitions_total{from="Candidate",group="",node="n1",to="Leader"} 1
raft_state_transitions_total{from="Follower",group="",node="n1",to="Candidate"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_state_transitions_total"); err != nil {
		t.Error(err)
	}

	// State gauge should reflect the latest role (Leader == 2).
	expected = `
# HELP raft_node_state Current role of the Raft node (0=Follower,1=Candidate,2=Leader,3=PreCandidate).
# TYPE raft_node_state gauge
raft_node_state{group="",node="n1"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_node_state"); err != nil {
		t.Error(err)
	}
}

func TestMetrics_CommitAdvanced(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)

	m.CommitAdvanced("n1", 5)
	m.CommitAdvanced("n1", 10)
	m.CommitAdvanced("n2", 3)

	expected := `
# HELP raft_commits_total Total number of times commitIndex has advanced.
# TYPE raft_commits_total counter
raft_commits_total{group="",node="n1"} 2
raft_commits_total{group="",node="n2"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_commits_total"); err != nil {
		t.Error(err)
	}

	expected = `
# HELP raft_commit_index Latest commitIndex.
# TYPE raft_commit_index gauge
raft_commit_index{group="",node="n1"} 10
raft_commit_index{group="",node="n2"} 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_commit_index"); err != nil {
		t.Error(err)
	}
}

func TestMetrics_SnapshotTaken(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)

	m.SnapshotTaken("n1", 100, 4096)
	m.SnapshotTaken("n1", 200, 8192)

	expected := `
# HELP raft_snapshots_total Total number of snapshots successfully persisted.
# TYPE raft_snapshots_total counter
raft_snapshots_total{group="",node="n1"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_snapshots_total"); err != nil {
		t.Error(err)
	}

	expected = `
# HELP raft_snapshot_bytes_total Total bytes written across all snapshots.
# TYPE raft_snapshot_bytes_total counter
raft_snapshot_bytes_total{group="",node="n1"} 12288
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_snapshot_bytes_total"); err != nil {
		t.Error(err)
	}
}

func TestMetrics_MultipleNodes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)

	m.StateChange("n1", raft.Follower, raft.Leader, 2)
	m.StateChange("n2", raft.Follower, raft.Candidate, 2)
	m.StateChange("n3", raft.Follower, raft.Candidate, 2)

	// Each node should have its own gauge value.
	expected := `
# HELP raft_node_state Current role of the Raft node (0=Follower,1=Candidate,2=Leader,3=PreCandidate).
# TYPE raft_node_state gauge
raft_node_state{group="",node="n1"} 2
raft_node_state{group="",node="n2"} 1
raft_node_state{group="",node="n3"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_node_state"); err != nil {
		t.Error(err)
	}
}

// TestNew_RepeatedOnSameRegistry pins the invariant that building many Metrics
// instances against one registry is safe. A process that runs several Raft
// groups does exactly that, once per group, and must not be brought down by a
// duplicate-collector registration.
func TestNew_RepeatedOnSameRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()

	const instances = 16
	for i := 0; i < instances; i++ {
		if m := prommetrics.New(reg); m == nil {
			t.Fatalf("instance %d: New returned nil", i)
		}
	}

	// All instances share one collector family, so the registry still gathers
	// a single metric family per name.
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := make(map[string]int, len(families))
	for _, f := range families {
		seen[f.GetName()]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("metric family %q gathered %d times, want 1", name, count)
		}
	}
}

// TestNewForGroup_SeriesAreSeparatedByGroup verifies that groups sharing one
// registry each get their own series rather than overwriting one another.
func TestNewForGroup_SeriesAreSeparatedByGroup(t *testing.T) {
	reg := prometheus.NewRegistry()

	groups := []struct {
		id    uint64
		node  raft.NodeID
		state raft.State
	}{
		{id: 1, node: "n1", state: raft.Leader},
		{id: 2, node: "n1", state: raft.Follower},
		{id: 3, node: "n1", state: raft.Candidate},
	}

	for _, g := range groups {
		m := prommetrics.NewForGroup(reg, g.id)
		m.StateChange(g.node, raft.Follower, g.state, 7)
		m.CommitAdvanced(g.node, raft.Index(g.id*10))
	}

	expected := `
# HELP raft_node_state Current role of the Raft node (0=Follower,1=Candidate,2=Leader,3=PreCandidate).
# TYPE raft_node_state gauge
raft_node_state{group="1",node="n1"} 2
raft_node_state{group="2",node="n1"} 0
raft_node_state{group="3",node="n1"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_node_state"); err != nil {
		t.Error(err)
	}

	expected = `
# HELP raft_commit_index Latest commitIndex.
# TYPE raft_commit_index gauge
raft_commit_index{group="1",node="n1"} 10
raft_commit_index{group="2",node="n1"} 20
raft_commit_index{group="3",node="n1"} 30
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_commit_index"); err != nil {
		t.Error(err)
	}
}

// TestNew_NilRegisterer documents that a nil Registerer yields a usable,
// non-exporting Metrics rather than a panic.
func TestNew_NilRegisterer(t *testing.T) {
	m := prommetrics.New(nil)
	m.StateChange("n1", raft.Follower, raft.Leader, 1)
	m.CommitAdvanced("n1", 3)
	m.SnapshotTaken("n1", 3, 128)
}

// TestStorageWrite_RecordsDurationAndOutcome asserts that durable writes are
// timed and counted, separately per operation and outcome.
//
// A disk that has become slow shows up here before anywhere else, and it is
// what distinguishes "the network is slow" from "this node's disk is slow"
// when proposal latency rises with nothing in the Raft state to explain it.
func TestStorageWrite_RecordsDurationAndOutcome(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := prommetrics.New(reg)

	m.StorageWrite("n1", "append", 5*time.Millisecond, nil)
	m.StorageWrite("n1", "append", 7*time.Millisecond, nil)
	m.StorageWrite("n1", "hardstate", 2*time.Millisecond, errors.New("disk gone"))

	if got := counterValue(t, reg, "raft_storage_writes_total",
		map[string]string{"node": "n1", "op": "append", "outcome": "ok"}); got != 2 {
		t.Errorf("successful appends counted %v, want 2", got)
	}
	if got := counterValue(t, reg, "raft_storage_writes_total",
		map[string]string{"node": "n1", "op": "hardstate", "outcome": "failed"}); got != 1 {
		t.Errorf("failed hard-state writes counted %v, want 1", got)
	}
	// A failed write must not be filed under the successful operation.
	if got := counterValue(t, reg, "raft_storage_writes_total",
		map[string]string{"node": "n1", "op": "hardstate", "outcome": "ok"}); got != 0 {
		t.Errorf("a failed write was counted as successful (%v)", got)
	}
}

// fakeNode reports fixed indices.
type fakeNode struct {
	id      raft.NodeID
	commit  raft.Index
	applied raft.Index
}

func (f fakeNode) ID() raft.NodeID         { return f.id }
func (f fakeNode) CommitIndex() raft.Index { return f.commit }
func (f fakeNode) LastApplied() raft.Index { return f.applied }

// TestTrack_ReportsApplyLagAtScrapeTime asserts that a tracked node's progress
// is read when the registry is scraped.
//
// The gap between committing and applying is the one number that says whether
// a node is keeping up, and it cannot be derived from the event-driven metrics:
// those say what happened, not how far behind the state machine is now. A node
// that commits happily while its state machine falls further behind looks
// healthy in every other series.
func TestTrack_ReportsApplyLagAtScrapeTime(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := prommetrics.New(reg)

	m.Track(fakeNode{id: "n1", commit: 100, applied: 60})

	if got := gaugeValue(t, reg, "raft_apply_lag", map[string]string{"node": "n1"}); got != 40 {
		t.Errorf("raft_apply_lag = %v, want 40", got)
	}
	if got := gaugeValue(t, reg, "raft_progress_commit_index", map[string]string{"node": "n1"}); got != 100 {
		t.Errorf("raft_progress_commit_index = %v, want 100", got)
	}
	if got := gaugeValue(t, reg, "raft_progress_last_applied", map[string]string{"node": "n1"}); got != 60 {
		t.Errorf("raft_progress_last_applied = %v, want 60", got)
	}
}

// TestTrack_LagIsNeverNegative guards the moment between the apply loop
// advancing and the commit index being read, where applied can briefly read
// higher than commit.
func TestTrack_LagIsNeverNegative(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := prommetrics.New(reg)

	m.Track(fakeNode{id: "n1", commit: 10, applied: 12})

	if got := gaugeValue(t, reg, "raft_apply_lag", map[string]string{"node": "n1"}); got != 0 {
		t.Errorf("raft_apply_lag = %v, want 0", got)
	}
}

// TestTrack_SeveralGroupsOnOneRegistry asserts that tracking nodes from more
// than one group works, which is the case that makes these collectors
// unchecked.
func TestTrack_SeveralGroupsOnOneRegistry(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	g1 := prommetrics.NewForGroup(reg, 1)
	g2 := prommetrics.NewForGroup(reg, 2)

	g1.Track(fakeNode{id: "n1", commit: 10, applied: 4})
	g2.Track(fakeNode{id: "n1", commit: 20, applied: 20})

	if got := gaugeValue(t, reg, "raft_apply_lag",
		map[string]string{"node": "n1", "group": "1"}); got != 6 {
		t.Errorf("group 1 apply lag = %v, want 6", got)
	}
	if got := gaugeValue(t, reg, "raft_apply_lag",
		map[string]string{"node": "n1", "group": "2"}); got != 0 {
		t.Errorf("group 2 apply lag = %v, want 0", got)
	}
}

// counterValue returns the value of a counter series matching labels.
func counterValue(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	return seriesValue(t, g, name, labels, func(m *dto.Metric) float64 {
		return m.GetCounter().GetValue()
	})
}

// gaugeValue returns the value of a gauge series matching labels.
func gaugeValue(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	return seriesValue(t, g, name, labels, func(m *dto.Metric) float64 {
		return m.GetGauge().GetValue()
	})
}

// seriesValue gathers and finds the one series of name whose labels are a
// superset of labels. A missing series reads as 0, which lets a test assert
// that nothing was recorded under a label set.
func seriesValue(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string, read func(*dto.Metric) float64) float64 {
	t.Helper()

	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			have := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				have[l.GetName()] = l.GetValue()
			}
			matched := true
			for k, v := range labels {
				if have[k] != v {
					matched = false
					break
				}
			}
			if matched {
				return read(m)
			}
		}
	}
	return 0
}
