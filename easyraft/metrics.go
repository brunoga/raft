package easyraft

import (
	"errors"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// storeMetrics are the gauges a Store reports about the state it is holding.
//
// The engine's own metrics come from the prommetrics package; these are about
// the state machine easyraft puts on top of it, which is the part with no
// bound on it and therefore the part worth watching.
type storeMetrics struct {
	group string
	node  string

	stateBytes *prometheus.GaugeVec
	stateKeys  *prometheus.GaugeVec
}

// stateMetricLabels matches the engine's, so a dashboard can join on them.
var stateMetricLabels = []string{"group", "node"}

// newStoreMetrics registers the gauges, reusing the ones already on this
// registry when several groups in one process share it.
func newStoreMetrics(reg prometheus.Registerer, groupID uint64, node string) *storeMetrics {
	if reg == nil {
		return nil
	}
	group := ""
	if groupID != 0 {
		group = strconv.FormatUint(groupID, 10)
	}
	m := &storeMetrics{
		group: group,
		node:  node,
		stateBytes: registerOrGet(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "easyraft",
			Name:      "state_bytes",
			Help: "Approximate bytes of application state held in memory: per key, " +
				"the key plus its encoded value. Undercounts the process's real " +
				"footprint, which carries Go map overhead on top, but is exact about growth.",
		}, stateMetricLabels)),
		stateKeys: registerOrGet(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "easyraft",
			Name:      "state_keys",
			Help:      "Keys held in memory across every collection, internal ones included.",
		}, stateMetricLabels)),
	}
	// Published at zero straight away, so the series exists from the moment
	// the node starts rather than from its first write. A gauge that appears
	// only once something happens cannot be alerted on when nothing does.
	m.observe(0, 0)
	return m
}

// observe publishes the current size. Called once per applied entry, outside
// the state-machine lock, so a scrape never waits on an apply and an apply
// never waits on Prometheus.
func (m *storeMetrics) observe(bytes int64, keys int) {
	if m == nil {
		return
	}
	m.stateBytes.WithLabelValues(m.group, m.node).Set(float64(bytes))
	m.stateKeys.WithLabelValues(m.group, m.node).Set(float64(keys))
}

// registerOrGet registers c, or returns the equivalent collector already on
// the registry. Several groups in one process share a registry, and the
// second one to start must use the first one's vectors rather than failing or
// silently reporting nothing.
func registerOrGet[T prometheus.Collector](reg prometheus.Registerer, c T) T {
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
