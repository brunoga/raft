// Package prommetrics provides a Prometheus implementation of the raft.Metrics
// interface.
//
// Usage:
//
//	reg := prometheus.NewRegistry()
//	m := prommetrics.New(reg)
//	cfg.Metrics = m
//
// All metrics are labelled with the node ID so multiple nodes sharing the
// same registry (e.g. in tests) are disambiguated.
package prommetrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/brunoga/raft"
)

// Metrics implements raft.Metrics using Prometheus counters and gauges.
type Metrics struct {
	stateGauge    *prometheus.GaugeVec   // current role (0=Follower,1=Candidate,2=Leader,3=PreCandidate)
	termGauge     *prometheus.GaugeVec   // current term after each state change
	transitions   *prometheus.CounterVec // total state transitions, labelled by from/to
	commitIndex   *prometheus.GaugeVec   // latest commitIndex
	commitsTotal  *prometheus.CounterVec // total commits advanced
	snapshotsTotal *prometheus.CounterVec // total snapshots taken
	snapshotBytes *prometheus.CounterVec // total snapshot bytes written
}

// New registers all Prometheus metrics with reg and returns a Metrics instance.
// Passing prometheus.DefaultRegisterer uses the global registry.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		stateGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "raft",
			Name:      "node_state",
			Help:      "Current role of the Raft node (0=Follower,1=Candidate,2=Leader,3=PreCandidate).",
		}, []string{"node"}),

		termGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "raft",
			Name:      "current_term",
			Help:      "Current Raft term observed at the last state transition.",
		}, []string{"node"}),

		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "raft",
			Name:      "state_transitions_total",
			Help:      "Total number of role transitions, labelled by from and to state.",
		}, []string{"node", "from", "to"}),

		commitIndex: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "raft",
			Name:      "commit_index",
			Help:      "Latest commitIndex.",
		}, []string{"node"}),

		commitsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "raft",
			Name:      "commits_total",
			Help:      "Total number of times commitIndex has advanced.",
		}, []string{"node"}),

		snapshotsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "raft",
			Name:      "snapshots_total",
			Help:      "Total number of snapshots successfully persisted.",
		}, []string{"node"}),

		snapshotBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "raft",
			Name:      "snapshot_bytes_total",
			Help:      "Total bytes written across all snapshots.",
		}, []string{"node"}),
	}

	reg.MustRegister(
		m.stateGauge,
		m.termGauge,
		m.transitions,
		m.commitIndex,
		m.commitsTotal,
		m.snapshotsTotal,
		m.snapshotBytes,
	)

	return m
}

// StateChange implements raft.Metrics.
func (m *Metrics) StateChange(id raft.NodeID, from, to raft.State, term raft.Term) {
	node := string(id)
	m.stateGauge.WithLabelValues(node).Set(float64(to))
	m.termGauge.WithLabelValues(node).Set(float64(term))
	m.transitions.WithLabelValues(node, from.String(), to.String()).Inc()
}

// CommitAdvanced implements raft.Metrics.
func (m *Metrics) CommitAdvanced(id raft.NodeID, commitIndex raft.Index) {
	node := string(id)
	m.commitIndex.WithLabelValues(node).Set(float64(commitIndex))
	m.commitsTotal.WithLabelValues(node).Inc()
}

// SnapshotTaken implements raft.Metrics.
func (m *Metrics) SnapshotTaken(id raft.NodeID, lastIncludedIndex raft.Index, sizeBytes int) {
	node := string(id)
	m.snapshotsTotal.WithLabelValues(node).Inc()
	m.snapshotBytes.WithLabelValues(node).Add(float64(sizeBytes))
	// Re-use the commit gauge to track how far the snapshot covers.
	_ = lastIncludedIndex
}
