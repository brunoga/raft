// Package prommetrics provides a Prometheus implementation of the raft.Metrics
// interface.
//
// Usage:
//
//	reg := prometheus.NewRegistry()
//	m := prommetrics.New(reg)
//	cfg.Metrics = m
//
// # Labels
//
// All metrics carry a "node" label holding the Raft node ID, so several nodes
// sharing one registry (a test harness, or a process hosting more than one
// node) stay distinguishable.
//
// They also carry a "group" label holding the Raft group ID for instances
// built with [NewForGroup]. [New] leaves it empty, which is the correct value
// for a single-group deployment: existing dashboards that ignore the label
// keep working, and multi-group deployments gain a dimension to split on.
//
// # Reuse across instances
//
// Collector registration is idempotent per registry. Constructing many
// Metrics values against the same Registerer — one per Raft group, say —
// registers each collector once and shares it; it never panics with
// "duplicate metrics collector registration attempted". Instances differ only
// in the label values they write.
package prommetrics

import (
	"errors"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/brunoga/raft"
)

// Metrics implements raft.Metrics using Prometheus counters and gauges.
//
// A Metrics value is safe for concurrent use and may be shared by several
// nodes; the node ID is supplied per call and becomes a label value.
type Metrics struct {
	// group is the value written to the "group" label on every series.
	group string

	stateGauge     *prometheus.GaugeVec   // current role (0=Follower,1=Candidate,2=Leader,3=PreCandidate)
	termGauge      *prometheus.GaugeVec   // current term after each state change
	transitions    *prometheus.CounterVec // total state transitions, labelled by from/to
	commitIndex    *prometheus.GaugeVec   // latest commitIndex
	commitsTotal   *prometheus.CounterVec // total commits advanced
	snapshotsTotal *prometheus.CounterVec // total snapshots taken
	snapshotBytes  *prometheus.CounterVec // total snapshot bytes written
}

// New returns a Metrics instance whose series carry an empty "group" label.
// Use it for single-group deployments; use [NewForGroup] when one process runs
// several Raft groups against the same registry.
//
// Calling New more than once with the same Registerer is safe: the collectors
// are registered on the first call and reused thereafter.
// Passing prometheus.DefaultRegisterer uses the global registry.
func New(reg prometheus.Registerer) *Metrics {
	return newMetrics(reg, "")
}

// NewForGroup returns a Metrics instance whose series carry groupID in the
// "group" label, so the series of one Raft group can be told apart from
// another's on a shared registry.
//
// Like [New], it is safe to call repeatedly against the same Registerer.
func NewForGroup(reg prometheus.Registerer, groupID uint64) *Metrics {
	return newMetrics(reg, strconv.FormatUint(groupID, 10))
}

// commonLabels is the label set shared by every metric in this package.
// "group" comes first so a series reads group-then-node.
var commonLabels = []string{"group", "node"}

// transitionLabels extends commonLabels with the from/to states.
var transitionLabels = []string{"group", "node", "from", "to"}

func newMetrics(reg prometheus.Registerer, group string) *Metrics {
	return &Metrics{
		group: group,

		stateGauge: registerOrGet(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "raft",
			Name:      "node_state",
			Help:      "Current role of the Raft node (0=Follower,1=Candidate,2=Leader,3=PreCandidate).",
		}, commonLabels)),

		termGauge: registerOrGet(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "raft",
			Name:      "current_term",
			Help:      "Current Raft term observed at the last state transition.",
		}, commonLabels)),

		transitions: registerOrGet(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "raft",
			Name:      "state_transitions_total",
			Help:      "Total number of role transitions, labelled by from and to state.",
		}, transitionLabels)),

		commitIndex: registerOrGet(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "raft",
			Name:      "commit_index",
			Help:      "Latest commitIndex.",
		}, commonLabels)),

		commitsTotal: registerOrGet(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "raft",
			Name:      "commits_total",
			Help:      "Total number of times commitIndex has advanced.",
		}, commonLabels)),

		snapshotsTotal: registerOrGet(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "raft",
			Name:      "snapshots_total",
			Help:      "Total number of snapshots successfully persisted.",
		}, commonLabels)),

		snapshotBytes: registerOrGet(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "raft",
			Name:      "snapshot_bytes_total",
			Help:      "Total bytes written across all snapshots.",
		}, commonLabels)),
	}
}

// registerOrGet registers c with reg and returns it. If an identical collector
// is already registered — which happens whenever a second Metrics instance is
// built against the same registry — the already-registered collector is
// returned instead, so both instances write to the same series family.
//
// A nil Registerer, or a registry that rejects c for any other reason, yields
// an unregistered collector: the caller keeps working, the series are simply
// not exported.
func registerOrGet[T prometheus.Collector](reg prometheus.Registerer, c T) T {
	if reg == nil {
		return c
	}
	err := reg.Register(c)
	if err == nil {
		return c
	}
	var already prometheus.AlreadyRegisteredError
	if errors.As(err, &already) {
		if existing, ok := already.ExistingCollector.(T); ok {
			return existing
		}
	}
	return c
}

// StateChange implements raft.Metrics.
func (m *Metrics) StateChange(id raft.NodeID, from, to raft.State, term raft.Term) {
	node := string(id)
	m.stateGauge.WithLabelValues(m.group, node).Set(float64(to))
	m.termGauge.WithLabelValues(m.group, node).Set(float64(term))
	m.transitions.WithLabelValues(m.group, node, from.String(), to.String()).Inc()
}

// CommitAdvanced implements raft.Metrics.
func (m *Metrics) CommitAdvanced(id raft.NodeID, commitIndex raft.Index) {
	node := string(id)
	m.commitIndex.WithLabelValues(m.group, node).Set(float64(commitIndex))
	m.commitsTotal.WithLabelValues(m.group, node).Inc()
}

// SnapshotTaken implements raft.Metrics.
func (m *Metrics) SnapshotTaken(id raft.NodeID, lastIncludedIndex raft.Index, sizeBytes int) {
	node := string(id)
	m.snapshotsTotal.WithLabelValues(m.group, node).Inc()
	m.snapshotBytes.WithLabelValues(m.group, node).Add(float64(sizeBytes))
	// Re-use the commit gauge to track how far the snapshot covers.
	_ = lastIncludedIndex
}
