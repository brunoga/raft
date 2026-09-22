package main

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
)

// feed pushes events through an observer the way the node would.
func feed(o *observer, events ...raft.Event) {
	for i := range events {
		o.record(&events[i])
	}
}

func quiet() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestObserver_TracksPeerReachability is the question an operator actually
// asks: which peers are down right now.
//
// The node reports transitions, not state, so an observer that does not keep
// the state cannot answer it. And the answer is what decides whether the next
// failure costs the cluster its quorum -- a counter of unreachable events does
// not, because it cannot say whether the peer came back.
func TestObserver_TracksPeerReachability(t *testing.T) {
	o := newObserver(quiet())

	feed(o,
		raft.Event{Type: raft.EventPeerAdded, Peer: "n2", Voter: true, Term: 1},
		raft.Event{Type: raft.EventPeerAdded, Peer: "n3", Voter: true, Term: 1},
		raft.Event{Type: raft.EventPeerUnresponsive, Peer: "n3", Term: 1},
	)

	down := o.unreachablePeers()
	if len(down) != 1 || down[0] != "n3" {
		t.Fatalf("unreachable = %v, want [n3]", down)
	}

	feed(o, raft.Event{Type: raft.EventPeerResponsive, Peer: "n3", Term: 1})
	if down := o.unreachablePeers(); len(down) != 0 {
		t.Errorf("unreachable = %v after the peer came back, want none", down)
	}

	// The history is still there: a peer that flaps is not the same as one
	// that has been healthy all along, and only the count distinguishes them.
	state := o.snapshotState()
	peers, _ := state["peers"].([]peerState)
	for _, p := range peers {
		if p.ID == "n3" && p.Unreachable != 1 {
			t.Errorf("n3 recorded %d outages, want 1", p.Unreachable)
		}
	}
}

// TestObserver_CountsDroppedEvents checks that a gap in the history is
// reported as a gap.
//
// The subscription is bounded and the node never blocks on it, so a consumer
// that falls behind loses events. The next event it receives carries how many
// were discarded before it. An observer that ignores that field presents a
// clean history it does not have, which is worse than presenting no history.
func TestObserver_CountsDroppedEvents(t *testing.T) {
	o := newObserver(quiet())

	feed(o,
		raft.Event{Type: raft.EventPeerUnresponsive, Peer: "n2", Term: 3},
		raft.Event{Type: raft.EventPeerResponsive, Peer: "n2", Term: 3, Dropped: 17},
	)

	if got := o.snapshotState()["events_dropped"]; got != uint64(17) {
		t.Errorf("events_dropped = %v, want 17; a history with holes has to say so", got)
	}
}

// TestObserver_RecordsLeadershipAndFailure checks the two events that change
// what the node is rather than what it knows.
func TestObserver_RecordsLeadershipAndFailure(t *testing.T) {
	o := newObserver(quiet())

	feed(o, raft.Event{Type: raft.EventLeadershipChanged, Leader: "n2", IsLeader: false, Term: 4})
	state := o.snapshotState()
	if state["leader"] != raft.NodeID("n2") || state["is_leader"] != false {
		t.Errorf("leader = %v, is_leader = %v; want n2 and false", state["leader"], state["is_leader"])
	}

	feed(o, raft.Event{Type: raft.EventLeadershipChanged, Leader: "n1", IsLeader: true, Term: 5})
	state = o.snapshotState()
	if state["is_leader"] != true {
		t.Error("is_leader did not follow the node becoming leader")
	}

	// A node that stopped because a durable write failed reports it here and
	// then reports nothing else, ever. Surfacing it is the difference between
	// a node that is quiet and one that is dead.
	feed(o, raft.Event{Type: raft.EventNodeFailed, Term: 5, Err: errors.New("disk is full")})
	if got := o.snapshotState()["node_failed"]; got != "disk is full" {
		t.Errorf("node_failed = %v, want the error the node stopped with", got)
	}
}

// TestObserver_BoundsItsHistory checks that the recent-event list cannot grow
// without limit. An observer that keeps every event it has ever seen is a leak
// in a long-lived process, and the oldest events are the least useful.
func TestObserver_BoundsItsHistory(t *testing.T) {
	o := newObserver(quiet())
	o.maxRecent = 10

	for i := range 100 {
		o.record(&raft.Event{Type: raft.EventPeerUnresponsive, Peer: "n2", Term: raft.Term(i)})
	}

	recent, _ := o.snapshotState()["recent"].([]observation)
	if len(recent) != 10 {
		t.Fatalf("kept %d events, want the last 10", len(recent))
	}
	// And it keeps the newest, not the oldest.
	if recent[len(recent)-1].Term != 99 {
		t.Errorf("newest kept event is from term %d, want 99", recent[len(recent)-1].Term)
	}
}

// TestObserver_SlowSubscriberDoesNotBlockTheObserver checks the property the
// node relies on, one level down.
//
// The node does not block on the observer, and the observer must not block on
// an SSE client that has stopped reading -- otherwise a browser tab left open
// on a laptop that went to sleep stops the event history for everyone else.
func TestObserver_SlowSubscriberDoesNotBlockTheObserver(t *testing.T) {
	o := newObserver(quiet())

	_, unsubscribe := o.subscribe()
	defer unsubscribe()

	// Far more than the subscriber channel holds, with nothing reading it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 1000 {
			o.record(&raft.Event{Type: raft.EventPeerUnresponsive, Peer: "n2", Term: raft.Term(i)})
		}
	}()

	select {
	case <-done:
	case <-testTimeout():
		t.Fatal("the observer blocked on a subscriber that stopped reading")
	}
}

func testTimeout() <-chan time.Time { return time.After(10 * time.Second) }
