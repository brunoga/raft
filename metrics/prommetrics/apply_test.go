package prommetrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/metrics/prommetrics"
)

// Metrics has to satisfy every optional interface the engine looks for, or the
// engine silently stops reporting through it: detection is a type assertion,
// so losing one produces no error, no log line, and a metric that is simply
// always absent.
var (
	_ raft.Metrics         = (*prommetrics.Metrics)(nil)
	_ raft.StorageMetrics  = (*prommetrics.Metrics)(nil)
	_ raft.ProposalMetrics = (*prommetrics.Metrics)(nil)
	_ raft.ApplyMetrics    = (*prommetrics.Metrics)(nil)
)

// TestApplySaturation_IsExported checks the metric reaches a scrape.
//
// It is the number that says where a slow write is slow: proposal latency
// covers consensus and the state machine together, and a rise in it does not
// say which of the two to fix. Saturation near 1 says the apply loop never
// gets to wait, so the state machine is the constraint and faster consensus
// buys nothing.
func TestApplySaturation_IsExported(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)

	m.ApplySaturation("n1", 0.75)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "raft_apply_saturation" {
			continue
		}
		for _, metric := range f.GetMetric() {
			if got := metric.GetGauge().GetValue(); got != 0.75 {
				t.Errorf("raft_apply_saturation = %v, want 0.75", got)
			}
			var node string
			for _, l := range metric.GetLabel() {
				if l.GetName() == "node" {
					node = l.GetValue()
				}
			}
			if node != "n1" {
				t.Errorf("node label = %q, want n1", node)
			}
			return
		}
	}

	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	t.Errorf("raft_apply_saturation was not exported; got %s", strings.Join(names, ", "))
}
