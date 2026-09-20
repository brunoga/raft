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
	"sync"
	"time"

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
	snapshotIndex  *prometheus.GaugeVec   // last index covered by a snapshot

	proposalLatency *prometheus.HistogramVec // submission to applied, by outcome
	proposalsTotal  *prometheus.CounterVec   // total proposals, by outcome

	storageLatency *prometheus.HistogramVec // durable write duration, by op and outcome
	storageWrites  *prometheus.CounterVec   // durable writes, by op and outcome

	applySaturation *prometheus.GaugeVec // fraction of the apply loop spent working

	// tracked holds the nodes whose live indices are read at scrape time.
	// Gauges like the apply lag have no natural event to hang off: they are a
	// question about the present, so they are answered when asked.
	trackedMu sync.Mutex
	tracked   []NodeSource
}

// NodeSource is what the collector needs from a node to report its progress.
// *raft.Node satisfies it.
type NodeSource interface {
	ID() raft.NodeID
	CommitIndex() raft.Index
	LastApplied() raft.Index
}

// Track reports n's progress on every scrape: its commit index, its applied
// index, and the gap between them.
//
// That gap is the one number that says whether a node is keeping up. It cannot
// be derived from the event-driven metrics -- those say what happened, not how
// far behind the state machine is right now -- and a node that commits happily
// while its state machine falls further behind looks healthy in every other
// series.
//
// Track may be called for several nodes; each reports under its own node label.
func (m *Metrics) Track(n NodeSource) {
	if n == nil {
		return
	}
	m.trackedMu.Lock()
	defer m.trackedMu.Unlock()
	m.tracked = append(m.tracked, n)
}

// Describe implements prometheus.Collector. It deliberately sends nothing,
// which registers this as an unchecked collector.
//
// Several Metrics instances share one registry when a process runs several
// groups, and they all report the same three descriptors, differing only in a
// label value. Describing them would make the second registration a duplicate
// and fail. An unchecked collector skips that check, at the cost of the
// registry not being able to police these series -- an acceptable trade for
// metrics whose label values are known only at scrape time.
func (m *Metrics) Describe(chan<- *prometheus.Desc) {}

// Collect implements prometheus.Collector.
func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	m.trackedMu.Lock()
	tracked := make([]NodeSource, len(m.tracked))
	copy(tracked, m.tracked)
	m.trackedMu.Unlock()

	for _, n := range tracked {
		node := string(n.ID())
		commit := n.CommitIndex()
		applied := n.LastApplied()
		var lag raft.Index
		if commit > applied {
			lag = commit - applied
		}
		ch <- prometheus.MustNewConstMetric(commitIndexDesc, prometheus.GaugeValue, float64(commit), m.group, node)
		ch <- prometheus.MustNewConstMetric(lastAppliedDesc, prometheus.GaugeValue, float64(applied), m.group, node)
		ch <- prometheus.MustNewConstMetric(applyLagDesc, prometheus.GaugeValue, float64(lag), m.group, node)
	}
}

var (
	commitIndexDesc = prometheus.NewDesc("raft_progress_commit_index",
		"Highest log index known to be committed, read at scrape time.", commonLabels, nil)
	lastAppliedDesc = prometheus.NewDesc("raft_progress_last_applied",
		"Highest log index applied to the state machine, read at scrape time.", commonLabels, nil)
	applyLagDesc = prometheus.NewDesc("raft_apply_lag",
		"Entries committed but not yet applied to the state machine.", commonLabels, nil)
)

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

// outcomeLabels extends commonLabels with whether the operation succeeded, so
// that a rise in failures is visible without a second metric.
var outcomeLabels = []string{"group", "node", "outcome"}

// storageLabels separates the durable writes from one another, since a slow
// log append and a slow hard-state write point at different problems.
var storageLabels = []string{"group", "node", "op", "outcome"}

// transitionLabels extends commonLabels with the from/to states.
var transitionLabels = []string{"group", "node", "from", "to"}

func newMetrics(reg prometheus.Registerer, group string) *Metrics {
	m := newMetricVecs(reg, group)
	if reg != nil {
		// Registered so that the gauges read at scrape time (see Collect) are
		// exported. An error here means an identical collector is already
		// present, which is not possible for a freshly built value.
		_ = reg.Register(m)
	}
	return m
}

func newMetricVecs(reg prometheus.Registerer, group string) *Metrics {
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

		snapshotIndex: registerOrGet(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "raft",
			Name:      "snapshot_index",
			Help:      "Last log index covered by the most recent snapshot.",
		}, commonLabels)),

		// Buckets span a fast local commit (a millisecond) to a cluster in
		// trouble (tens of seconds), since the interesting question is usually
		// which end of that range the tail has moved to.
		proposalLatency: registerOrGet(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "raft",
			Name:      "proposal_duration_seconds",
			Help:      "Time from a proposal being submitted to its entry being applied.",
			Buckets: []float64{
				0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1,
				0.25, 0.5, 1, 2.5, 5, 10, 30,
			},
		}, outcomeLabels)),

		proposalsTotal: registerOrGet(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "raft",
			Name:      "proposals_total",
			Help:      "Total proposals resolved, labelled by outcome.",
		}, outcomeLabels)),

		// The number that says where a slow write is slow. Proposal latency
		// covers consensus and the state machine together, so a rise in it
		// does not say which to fix; this does. Near 1 the apply loop never
		// gets to wait, so the state machine is the constraint and faster
		// consensus buys nothing.
		applySaturation: registerOrGet(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "raft",
			Name:      "apply_saturation",
			Help: "Fraction of the recent window the apply loop spent applying rather " +
				"than waiting for committed entries, in [0,1].",
		}, commonLabels)),

		// A durable write that has become slow is the usual explanation for a
		// rise in proposal latency that nothing in the Raft state accounts
		// for, so the buckets start far below a healthy fsync and run well
		// past an unhealthy one.
		storageLatency: registerOrGet(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "raft",
			Name:      "storage_write_duration_seconds",
			Help:      "Time spent in a durable storage write, by operation.",
			Buckets: []float64{
				0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01,
				0.025, 0.05, 0.1, 0.25, 0.5, 1, 5,
			},
		}, storageLabels)),

		storageWrites: registerOrGet(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "raft",
			Name:      "storage_writes_total",
			Help:      "Total durable storage writes, labelled by operation and outcome.",
		}, storageLabels)),
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
	m.snapshotIndex.WithLabelValues(m.group, node).Set(float64(lastIncludedIndex))
}

// StorageWrite implements raft.StorageMetrics.
func (m *Metrics) StorageWrite(id raft.NodeID, op string, d time.Duration, err error) {
	node := string(id)
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	m.storageLatency.WithLabelValues(m.group, node, op, outcome).Observe(d.Seconds())
	m.storageWrites.WithLabelValues(m.group, node, op, outcome).Inc()
}

// ApplySaturation implements raft.ApplyMetrics.
func (m *Metrics) ApplySaturation(id raft.NodeID, saturation float64) {
	m.applySaturation.WithLabelValues(m.group, string(id)).Set(saturation)
}

// ProposalCompleted implements raft.ProposalMetrics.
func (m *Metrics) ProposalCompleted(id raft.NodeID, latency time.Duration, ok bool) {
	node := string(id)
	outcome := "ok"
	if !ok {
		outcome = "failed"
	}
	m.proposalLatency.WithLabelValues(m.group, node, outcome).Observe(latency.Seconds())
	m.proposalsTotal.WithLabelValues(m.group, node, outcome).Inc()
}
