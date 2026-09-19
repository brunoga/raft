package prommetrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/metrics/prommetrics"
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
