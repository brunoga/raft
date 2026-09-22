package prommetrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/metrics/prommetrics"
)

// Metrics must satisfy this one too, for the same reason as the rest: the
// engine detects it by type assertion, so a missing method costs no error and
// no log line, only a series that is always absent.
var _ raft.ClientTableMetrics = (*prommetrics.Metrics)(nil)

// tableNode is a NodeSource that also reports its client table occupancy,
// which is what *raft.Node does.
type tableNode struct {
	id        raft.NodeID
	tableSize int
}

func (n *tableNode) ID() raft.NodeID         { return n.id }
func (n *tableNode) CommitIndex() raft.Index { return 0 }
func (n *tableNode) LastApplied() raft.Index { return 0 }
func (n *tableNode) ClientTableSize() int    { return n.tableSize }

// plainNode reports only what NodeSource requires, as a caller who wrote their
// own implementation before the occupancy series existed would.
type plainNode struct{ id raft.NodeID }

func (n *plainNode) ID() raft.NodeID         { return n.id }
func (n *plainNode) CommitIndex() raft.Index { return 0 }
func (n *plainNode) LastApplied() raft.Index { return 0 }

// TestClientsForgotten_IsExported checks that the one metric here which is
// about correctness rather than performance reaches a scrape.
//
// Every increment is a client whose next retry will be executed a second time.
// Nothing downstream can detect that: the command applies cleanly, the log
// stays consistent and every replica agrees, because every replica forgot the
// same client. This counter is the only place it is ever written down.
func TestClientsForgotten_IsExported(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)

	m.ClientForgotten("n1", "some-client")
	m.ClientForgotten("n1", "another-client")

	if got := gaugeOrCounter(t, reg, "raft_clients_forgotten_total", "n1"); got != 2 {
		t.Errorf("raft_clients_forgotten_total = %v, want 2", got)
	}
}

// TestClientTableSize_IsExported checks the occupancy gauge, which is the
// signal that arrives before the guarantee lapses rather than after.
func TestClientTableSize_IsExported(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)
	m.Track(&tableNode{id: "n1", tableSize: 42})

	if got := gaugeOrCounter(t, reg, "raft_client_table_size", "n1"); got != 42 {
		t.Errorf("raft_client_table_size = %v, want 42", got)
	}
}

// TestClientTableSize_OptionalOnNodeSource checks that a caller who
// implemented NodeSource themselves still works.
//
// The occupancy series needs a method NodeSource does not require. Adding it
// to the interface would break every such caller at compile time, so it is
// detected instead, and a source without it must simply report no series
// rather than panicking or reporting a zero that looks like an empty table.
func TestClientTableSize_OptionalOnNodeSource(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := prommetrics.New(reg)
	m.Track(&plainNode{id: "n1"})

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == "raft_client_table_size" {
			t.Error("raft_client_table_size was exported for a NodeSource that cannot " +
				"report it; a fabricated zero reads as a table with room to spare")
		}
	}
}

// gaugeOrCounter returns the value of the single series named name carrying
// the given node label, failing the test if it is absent.
func gaugeOrCounter(t *testing.T, reg *prometheus.Registry, name string, node raft.NodeID) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.GetMetric() {
			var got string
			for _, l := range metric.GetLabel() {
				if l.GetName() == "node" {
					got = l.GetValue()
				}
			}
			if raft.NodeID(got) != node {
				continue
			}
			if c := metric.GetCounter(); c != nil {
				return c.GetValue()
			}
			return metric.GetGauge().GetValue()
		}
	}
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	t.Fatalf("%s was not exported; got %s", name, strings.Join(names, ", "))
	return 0
}
