package raft_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
)

// These tests are about the facts metrics cannot carry. A counter of
// configuration changes does not name the peer that joined; a replication
// histogram does not say which follower stopped answering, which is the only
// fact that decides whether the next failure costs the cluster its quorum.

// awaitEvent waits for an event of the given type and returns it.
func awaitEvent(t *testing.T, events <-chan raft.Event, want raft.EventType, timeout time.Duration) raft.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the event stream closed before %s arrived", want)
			}
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event within %s", want, timeout)
		}
	}
}

// TestEvents_ReportsAMembershipChangeWhenItCommits covers the event an
// operator needs to know a join actually took effect.
//
// Membership changes at commit time, not when the request was made, and a
// request that never commits leaves no trace anywhere a client can see. The
// event is the moment the quorum this node needs actually changed.
func TestEvents_ReportsAMembershipChangeWhenItCommits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()
		t.Cleanup(node.Stop)

		events, stop := node.Events()
		defer stop()

		tick := tickWhile(node)
		defer tick()
		deadline := time.Now().Add(5 * time.Second)
		for node.State() != raft.Leader {
			if time.Now().After(deadline) {
				t.Fatal("the node never became leader")
			}
			time.Sleep(time.Millisecond)
		}

		// Several leadership events arrive on the way: the node becomes a
		// candidate before it becomes a leader, and each is reported.
		sawLeader := false
		leaderDeadline := time.After(5 * time.Second)
		for !sawLeader {
			select {
			case ev := <-events:
				if ev.Type == raft.EventLeadershipChanged && ev.IsLeader {
					if ev.Leader != "n1" {
						t.Errorf("the leadership event names %q as leader, want n1", ev.Leader)
					}
					sawLeader = true
				}
			case <-leaderDeadline:
				t.Fatal("no leadership event reported this node as leader")
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := node.AddServer(ctx, raft.PeerConfig{ID: "n2", Voter: false}); err != nil {
			t.Fatalf("AddServer: %v", err)
		}

		ev := awaitEvent(t, events, raft.EventPeerAdded, 5*time.Second)
		if ev.Peer != "n2" {
			t.Errorf("EventPeerAdded named %q, want n2", ev.Peer)
		}
		if ev.Voter {
			t.Error("EventPeerAdded reported a voter; the peer was added as a learner")
		}
		if ev.Term == 0 {
			t.Error("the event carries no term")
		}
	})
}

// TestEvents_ReportsAFollowerThatStopsAnswering covers the fact that decides
// whether a cluster is one failure from losing its quorum.
//
// Nothing in the Raft state says a follower is unreachable. The leader carries
// on normally, committing on the remaining majority, until the moment there is
// no majority left. This is what turns that into something that can be paged
// on before it happens.
func TestEvents_ReportsAFollowerThatStopsAnswering(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		cfg.Peers = []raft.PeerConfig{{ID: "n2"}, {ID: "n3"}} // learners, so n1 leads alone
		cfg.Transport = &unreachableTransport{}

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()
		t.Cleanup(node.Stop)

		events, stop := node.Events()
		defer stop()

		tick := tickWhile(node)
		defer tick()

		ev := awaitEvent(t, events, raft.EventPeerUnresponsive, 10*time.Second)
		if ev.Peer != "n2" && ev.Peer != "n3" {
			t.Errorf("EventPeerUnresponsive named %q, want one of the peers", ev.Peer)
		}
	})
}

// unreachableTransport fails every outbound RPC, as an unplugged peer does.
type unreachableTransport struct{ echoTransport }

func (t *unreachableTransport) AppendEntries(context.Context, raft.NodeID, *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	return nil, context.DeadlineExceeded
}

// TestEvents_ASlowConsumerLosesEventsAndIsToldSo pins the trade this makes.
//
// A subscription that stops receiving must never stall the event loop, because
// that would let one observer stop consensus for every other client of the
// node. So events are dropped instead. What must not happen is dropping them
// silently: a consumer keeping state derived from the stream has to be able to
// tell that its view is no longer complete.
func TestEvents_ASlowConsumerLosesEventsAndIsToldSo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()
		t.Cleanup(node.Stop)

		events, stop := node.Events()
		defer stop()

		tick := tickWhile(node)
		defer tick()
		deadline := time.Now().Add(5 * time.Second)
		for node.State() != raft.Leader {
			if time.Now().After(deadline) {
				t.Fatal("the node never became leader")
			}
			time.Sleep(time.Millisecond)
		}

		// Overrun the subscription without receiving anything. The node must keep
		// working throughout, which it demonstrates by still accepting proposals.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for i := range 200 {
			id := raft.NodeID("learner" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
			if err := node.AddServer(ctx, raft.PeerConfig{ID: id, Voter: false}); err != nil {
				t.Fatalf("AddServer %d: %v", i, err)
			}
		}

		// The count of what was missed rides on the next event that gets through,
		// which is what lets a consumer tell a complete history from a partial
		// one. So read to make room and keep causing events until one carries it,
		// rather than assuming which event that will be.
		giveUp := time.Now().Add(10 * time.Second)
		for round := 0; time.Now().Before(giveUp); round++ {
			select {
			case ev := <-events:
				if ev.Dropped > 0 {
					return // told, as it must be
				}
				continue
			default:
			}
			id := raft.NodeID("after-the-flood-" + string(rune('a'+round%26)))
			if err := node.AddServer(ctx, raft.PeerConfig{ID: id, Voter: false}); err != nil {
				t.Fatalf("AddServer after the flood: %v", err)
			}
		}
		t.Fatal("a subscription that overran was never told it had missed anything")
	})
}

// TestEvents_StopIsSafeTwiceAndTheStreamClosesOnShutdown pins the lifecycle.
func TestEvents_StopIsSafeTwiceAndTheStreamClosesOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()

		events, stop := node.Events()
		stop()
		stop() // must not panic

		if _, ok := <-events; ok {
			t.Error("a stopped subscription delivered an event")
		}

		live, liveStop := node.Events()
		defer liveStop()
		node.Stop()

		deadline := time.After(5 * time.Second)
		for {
			select {
			case _, ok := <-live:
				if !ok {
					return // closed, as it must be
				}
			case <-deadline:
				t.Fatal("the event stream was not closed when the node stopped")
			}
		}
	})
}

// saturationMetrics records the apply-saturation ratios reported to it.
type saturationMetrics struct {
	recordingMetrics
	got chan float64
}

func (m *saturationMetrics) ApplySaturation(_ raft.NodeID, saturation float64) {
	select {
	case m.got <- saturation:
	default:
	}
}

// TestApplySaturation_ReportsABusyStateMachineAsBusy covers the number that
// says where a slow write is slow.
//
// Proposal latency covers consensus and the state machine together, so a rise
// in it does not say which of the two to fix. A saturation close to 1 says the
// apply loop never gets to wait: the state machine is the constraint, and no
// amount of faster consensus helps. Without it that distinction is guesswork.
func TestApplySaturation_ReportsABusyStateMachineAsBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reported := make(chan float64, 32)

		cfg := safeBaseConfig(t, "n1")
		cfg.StateMachine = &busySM{}
		cfg.Metrics = &saturationMetrics{got: reported}

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()
		t.Cleanup(node.Stop)

		stop := tickWhile(node)
		defer stop()
		deadline := time.Now().Add(5 * time.Second)
		for node.State() != raft.Leader {
			if time.Now().After(deadline) {
				t.Fatal("the node never became leader")
			}
			time.Sleep(time.Millisecond)
		}

		// Keep the state machine busy.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-done:
					return
				default:
				}
				if _, err := node.Propose(ctx, []byte("x")); err != nil {
					return
				}
			}
		}()

		var busiest float64
		giveUp := time.After(15 * time.Second)
		for busiest < 0.5 {
			select {
			case r := <-reported:
				if r < 0 || r > 1 {
					t.Fatalf("saturation reported as %v, want a fraction in [0,1]", r)
				}
				if r > busiest {
					busiest = r
				}
			case <-giveUp:
				cancel()
				t.Fatalf("a state machine that never stopped working reported a peak saturation of %v", busiest)
			}
		}
		cancel()
	})
}

// busySM takes long enough per entry that the apply loop has no idle time.
type busySM struct{ idleSM }

func (busySM) Apply(_ context.Context, _ raft.LogEntry) ([]byte, error) {
	time.Sleep(2 * time.Millisecond)
	return nil, nil
}

// TestEvents_AnUnreachablePeerDoesNotFloodTheEventLoop pins the cost of
// noticing that a peer is down.
//
// An unreachable peer is retried every heartbeat interval, for as long as the
// outage lasts. A design that told the event loop about each failure would put
// one message per peer per interval into its queue precisely when a
// partitioned cluster can least afford the traffic, competing with the
// snapshot the leader is trying to send to somebody else. Only the change is
// reported, in either direction, so an outage costs two messages rather than
// thousands.
//
// What this checks is the consequence: a leader with a peer it cannot reach
// keeps answering, promptly, however long the outage runs.
func TestEvents_AnUnreachablePeerDoesNotFloodTheEventLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		cfg.Peers = []raft.PeerConfig{{ID: "gone"}} // a learner, so n1 leads alone
		cfg.Transport = &unreachableTransport{}

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()
		t.Cleanup(node.Stop)

		events, stopEvents := node.Events()
		defer stopEvents()

		stop := tickWhile(node)
		defer stop()
		deadline := time.Now().Add(5 * time.Second)
		for node.State() != raft.Leader {
			if time.Now().After(deadline) {
				t.Fatal("the node never became leader")
			}
			time.Sleep(time.Millisecond)
		}

		awaitEvent(t, events, raft.EventPeerUnresponsive, 10*time.Second)

		// Now drive many heartbeat intervals past the outage and keep proposing.
		// Each proposal has to cross the event loop and come back, so a loop
		// clogged with failure reports shows up here as a timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for i := range 200 {
			propCtx, propCancel := context.WithTimeout(ctx, 3*time.Second)
			_, err := node.Propose(propCtx, []byte("x"))
			propCancel()
			if err != nil {
				t.Fatalf("proposal %d was not answered while one peer was unreachable: %v", i, err)
			}
		}

		// And the outage was reported once, not once per heartbeat.
		extra := 0
		for drained := true; drained; {
			select {
			case ev := <-events:
				if ev.Type == raft.EventPeerUnresponsive {
					extra++
				}
			default:
				drained = false
			}
		}
		if extra > 0 {
			t.Errorf("the same outage was reported %d more times; it is one event, not one per heartbeat", extra)
		}
	})
}
