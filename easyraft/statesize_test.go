package easyraft_test

import (
	"strings"
	"testing"
	"testing/synctest"

	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/easyrafttest"
	"github.com/prometheus/client_golang/prometheus"
)

// gaugeValue reads one gauge out of a registry, or fails.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		metrics := family.GetMetric()
		if len(metrics) != 1 {
			t.Fatalf("%s has %d series, want 1", name, len(metrics))
		}
		if metrics[0].GetGauge() == nil {
			t.Fatalf("%s is not a gauge", name)
		}
		return metrics[0].GetGauge().GetValue()
	}
	var names []string
	for _, family := range families {
		names = append(names, family.GetName())
	}
	t.Fatalf("%s was not exported; the registry has %v", name, names)
	return 0
}

// labelsOf returns the label names and values of a metric's single series.
func labelsOf(t *testing.T, reg *prometheus.Registry, name string) map[string]string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		out := map[string]string{}
		for _, pair := range family.GetMetric()[0].GetLabel() {
			out[pair.GetName()] = pair.GetValue()
		}
		return out
	}
	t.Fatalf("%s was not exported", name)
	return nil
}

// TestStateSize_IsExportedAndGrows checks that the numbers reach Prometheus
// and move with the state, which is the whole reason to keep them.
func TestStateSize_IsExportedAndGrows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := prometheus.NewRegistry()
		c := easyrafttest.NewCluster(t, 1, easyrafttest.Options{
			Store: []easyraft.Option{easyraft.WithPrometheus(reg)},
		})
		items := easyrafttest.AddCollection[string](c, "items")
		ctx := c.Context()

		if got := gaugeValue(t, reg, "easyraft_state_keys"); got != 0 {
			t.Errorf("a fresh store exports %v keys", got)
		}

		const big = 400
		for i := range 10 {
			if err := items.Leader().Create(ctx, strings.Repeat("k", 10)+string(rune('a'+i)),
				strings.Repeat("v", big)); err != nil {
				t.Fatalf("Create %d: %v", i, err)
			}
		}
		c.WaitApplied()

		store := c.Node(0)
		if got := store.KeyCount(); got != 10 {
			t.Errorf("the store holds %d keys, want 10", got)
		}
		if got := store.StateBytes(); got < 10*big {
			t.Errorf("the store reports %d bytes for ten %d-byte values", got, big)
		}
		if got := gaugeValue(t, reg, "easyraft_state_keys"); got != float64(store.KeyCount()) {
			t.Errorf("the exported key count is %v, the store says %d", got, store.KeyCount())
		}
		if got := gaugeValue(t, reg, "easyraft_state_bytes"); got != float64(store.StateBytes()) {
			t.Errorf("the exported size is %v, the store says %d", got, store.StateBytes())
		}

		labels := labelsOf(t, reg, "easyraft_state_bytes")
		if labels["node"] != "n1" {
			t.Errorf("the series carries node=%q, want n1", labels["node"])
		}
		if _, ok := labels["group"]; !ok {
			t.Error("the series carries no group label, so a Manager's groups could not be told apart")
		}

		// Deleting brings both back down, which a gauge that only ever rose
		// would not.
		before := store.StateBytes()
		for i := range 10 {
			if err := items.Leader().Delete(ctx, strings.Repeat("k", 10)+string(rune('a'+i))); err != nil {
				t.Fatalf("Delete %d: %v", i, err)
			}
		}
		c.WaitApplied()
		if got := store.KeyCount(); got != 0 {
			t.Errorf("after deleting everything the store holds %d keys", got)
		}
		if got := store.StateBytes(); got >= before {
			t.Errorf("after deleting everything the store reports %d bytes, was %d", got, before)
		}
		if got := gaugeValue(t, reg, "easyraft_state_keys"); got != 0 {
			t.Errorf("the exported key count is %v after deleting everything", got)
		}
	})
}

// TestStateSize_EveryReplicaAgrees pins that the number describes replicated
// state rather than one node's history: every replica holds the same state,
// so every replica reports the same size.
func TestStateSize_EveryReplicaAgrees(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := easyrafttest.NewCluster(t, 3)
		items := easyrafttest.AddCollection[string](c, "items")
		ctx := c.Context()

		for i := range 20 {
			if err := items.Leader().Create(ctx, string(rune('a'+i)), strings.Repeat("v", i*10)); err != nil {
				t.Fatalf("Create %d: %v", i, err)
			}
		}
		c.WaitApplied()

		want := c.Node(0).StateBytes()
		wantKeys := c.Node(0).KeyCount()
		for i := 1; i < 3; i++ {
			if got := c.Node(i).StateBytes(); got != want {
				t.Errorf("node %d reports %d bytes, node 0 reports %d", i, got, want)
			}
			if got := c.Node(i).KeyCount(); got != wantKeys {
				t.Errorf("node %d holds %d keys, node 0 holds %d", i, got, wantKeys)
			}
		}
	})
}

// TestStateSize_SurvivesARestart checks that a node restarted from its own
// storage reports the state it loaded rather than starting from zero.
func TestStateSize_SurvivesARestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := easyrafttest.NewCluster(t, 3)
		items := easyrafttest.AddCollection[string](c, "items")
		ctx := c.Context()

		for i := range 15 {
			if err := items.Leader().Create(ctx, string(rune('a'+i)), strings.Repeat("v", 30)); err != nil {
				t.Fatalf("Create %d: %v", i, err)
			}
		}
		c.WaitApplied()

		victim := (c.LeaderIndex() + 1) % 3
		want := c.Node(victim).StateBytes()
		wantKeys := c.Node(victim).KeyCount()

		c.StopNode(victim)
		c.RestartNode(victim)
		items.Rebind(victim)
		c.WaitApplied()

		if got := c.Node(victim).StateBytes(); got != want {
			t.Errorf("after a restart the node reports %d bytes, held %d before", got, want)
		}
		if got := c.Node(victim).KeyCount(); got != wantKeys {
			t.Errorf("after a restart the node holds %d keys, held %d before", got, wantKeys)
		}
	})
}
