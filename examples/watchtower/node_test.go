package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/metrics/prommetrics"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// TestSaturation_SeparatesASlowStateMachineFromSlowConsensus is the question
// the metric answers.
//
// Proposal latency covers consensus and the state machine together, so a rise
// in it does not say which of the two to fix. Saturation near 1 says the apply
// loop never gets to wait: the state machine is the constraint, and faster
// consensus buys nothing. The same cluster with a fast state machine reports a
// low number under the same load.
func TestSaturation_SeparatesASlowStateMachineFromSlowConsensus(t *testing.T) {
	fast := runUnderLoad(t, 0)
	slow := runUnderLoad(t, 2*time.Millisecond)

	t.Logf("saturation: fast state machine %.3f, slow state machine %.3f", fast, slow)

	if slow < 0 || fast < 0 {
		t.Fatalf("saturation was not reported for both runs (fast %.3f, slow %.3f); "+
			"a ratio is reported once per bucket of a rolling window, so either the load "+
			"did not last long enough or ApplyMetrics is not being called", fast, slow)
	}
	// The separation is the claim, not either number on its own: the same
	// cluster under the same load driver reports a clearly higher ratio when
	// the state machine is the slow part. The margin is generous because the
	// absolute values depend on how fast the machine running this is.
	if slow <= fast+0.15 {
		t.Errorf("a state machine made 2ms-per-entry slower reported saturation %.3f "+
			"against the fast one's %.3f; the metric cannot answer the question it "+
			"exists for", slow, fast)
	}
	if slow < 0.6 {
		t.Errorf("saturation %.3f with a state machine that sleeps on every entry; "+
			"expected the apply loop to be busy for most of the window", slow)
	}
}

// runUnderLoad drives a single node with the given per-entry apply cost and
// returns the last saturation reported.
func runUnderLoad(t *testing.T, applyDelay time.Duration) float64 {
	t.Helper()

	sm := &kvSM{applyDelay: applyDelay}
	reg := prometheus.NewRegistry()
	metrics := metricsSink{Metrics: prommetrics.New(reg), sm: sm}

	net := memtransport.NewNetwork()
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = sm
	cfg.Transport = net.NewTransport("n1")
	cfg.Metrics = metrics
	cfg.TickInterval = 0
	cfg.HeartbeatInterval = 100 * time.Millisecond
	cfg.ElectionTimeoutMin = 1000 * time.Millisecond
	cfg.ElectionTimeoutMax = 2000 * time.Millisecond

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.Register("n1", node.Handler())
	node.Start()
	defer node.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && node.State() != raft.Leader {
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	if node.State() != raft.Leader {
		t.Fatal("node never became leader")
	}

	// Keep the apply loop fed for long enough that a saturation window closes.
	stop := make(chan struct{})
	ticking := make(chan struct{})
	go func() {
		defer close(ticking)
		for {
			select {
			case <-stop:
				return
			default:
				node.Tick()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Drive load for a fixed span of wall clock rather than a fixed number of
	// entries. The ratio is reported once per bucket of a rolling window, so a
	// run that finishes before a bucket closes reports nothing at all -- which
	// is what a fast state machine does if it is given a fixed, small amount
	// of work.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const loadFor = 2 * time.Second
	until := time.Now().Add(loadFor)
	proposals := 0
	for time.Now().Before(until) {
		cmd, err := json.Marshal(kvCommand{Key: fmt.Sprintf("k%d", proposals), Value: "v"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := node.Propose(ctx, cmd); err != nil {
			close(stop)
			<-ticking
			t.Fatalf("propose: %v", err)
		}
		proposals++
	}
	close(stop)
	<-ticking

	t.Logf("apply delay %v: %d proposals in %v, saturation %.3f",
		applyDelay, proposals, loadFor, sm.saturation())
	return sm.saturation()
}

// TestEvents_ReachTheObserverFromARealNode checks the wiring rather than the
// bookkeeping: that subscribing to a live node produces events the observer
// turns into state.
//
// The unit tests feed the observer directly, which cannot catch a subscription
// that is never consumed or a node that reports nothing.
func TestEvents_ReachTheObserverFromARealNode(t *testing.T) {
	sm := &kvSM{}
	net := memtransport.NewNetwork()
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = sm
	cfg.Transport = net.NewTransport("n1")
	cfg.TickInterval = 0
	cfg.HeartbeatInterval = 100 * time.Millisecond
	cfg.ElectionTimeoutMin = 1000 * time.Millisecond
	cfg.ElectionTimeoutMax = 2000 * time.Millisecond

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.Register("n1", node.Handler())

	events, stopEvents := node.Events()
	defer stopEvents()

	obs := newObserver(slog.New(slog.DiscardHandler))
	go obs.run(events)

	node.Start()
	defer node.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if state := obs.snapshotState(); state["is_leader"] == true {
			return
		}
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the observer never saw this node become leader; state = %v", obs.snapshotState())
}
