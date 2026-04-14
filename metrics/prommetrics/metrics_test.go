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
raft_state_transitions_total{from="Candidate",node="n1",to="Leader"} 1
raft_state_transitions_total{from="Follower",node="n1",to="Candidate"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_state_transitions_total"); err != nil {
		t.Error(err)
	}

	// State gauge should reflect the latest role (Leader == 2).
	expected = `
# HELP raft_node_state Current role of the Raft node (0=Follower,1=Candidate,2=Leader,3=PreCandidate).
# TYPE raft_node_state gauge
raft_node_state{node="n1"} 2
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
raft_commits_total{node="n1"} 2
raft_commits_total{node="n2"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_commits_total"); err != nil {
		t.Error(err)
	}

	expected = `
# HELP raft_commit_index Latest commitIndex.
# TYPE raft_commit_index gauge
raft_commit_index{node="n1"} 10
raft_commit_index{node="n2"} 3
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
raft_snapshots_total{node="n1"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_snapshots_total"); err != nil {
		t.Error(err)
	}

	expected = `
# HELP raft_snapshot_bytes_total Total bytes written across all snapshots.
# TYPE raft_snapshot_bytes_total counter
raft_snapshot_bytes_total{node="n1"} 12288
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
raft_node_state{node="n1"} 2
raft_node_state{node="n2"} 1
raft_node_state{node="n3"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"raft_node_state"); err != nil {
		t.Error(err)
	}
}
