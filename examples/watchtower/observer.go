package main

import (
	"log/slog"
	"sync"
	"time"

	"github.com/brunoga/raft/v2"
)

// ---- Turning events into something an operator can act on ------------------
//
// raft.Node.Events reports what happened, to whom, and when. Metrics cannot:
// a counter of configuration changes does not name the peer that joined, and a
// replication-latency histogram does not say which follower went quiet -- which
// is the only fact that decides whether the next failure costs the cluster its
// quorum.
//
// The subscription is bounded and lossy on purpose. The node never blocks on
// it, so a consumer that stops receiving loses events rather than stalling
// consensus for everyone else. Nothing is lost silently: the next event a
// lagging consumer receives carries the number discarded before it, and an
// observer that ignores that field reports a clean history it does not have.

// observation is one event with the time it was seen.
type observation struct {
	At      time.Time   `json:"at"`
	Type    string      `json:"type"`
	Term    raft.Term   `json:"term"`
	Peer    raft.NodeID `json:"peer,omitempty"`
	Leader  raft.NodeID `json:"leader,omitempty"`
	Index   raft.Index  `json:"index,omitempty"`
	Voter   bool        `json:"voter,omitempty"`
	Err     string      `json:"error,omitempty"`
	Dropped uint64      `json:"dropped,omitempty"`
}

// peerState is what the observer believes about one peer.
type peerState struct {
	ID          raft.NodeID `json:"id"`
	Reachable   bool        `json:"reachable"`
	Voter       bool        `json:"voter"`
	LastChange  time.Time   `json:"last_change"`
	Unreachable int         `json:"times_unreachable"`
}

// observer consumes the node's event stream and keeps the answers to the
// questions an operator actually asks: who is up, who leads, what happened
// recently, and how much of the history is missing.
type observer struct {
	logger *slog.Logger

	mu           sync.RWMutex
	peers        map[raft.NodeID]*peerState
	leader       raft.NodeID
	isLeader     bool
	term         raft.Term
	recent       []observation
	dropped      uint64
	failed       string
	subscribers  map[chan observation]struct{}
	maxRecent    int
	lastSnapshot *observation
}

func newObserver(logger *slog.Logger) *observer {
	return &observer{
		logger:      logger,
		peers:       make(map[raft.NodeID]*peerState),
		subscribers: make(map[chan observation]struct{}),
		maxRecent:   200,
	}
}

// run consumes events until the subscription ends.
func (o *observer) run(events <-chan raft.Event) {
	for ev := range events {
		o.record(&ev)
	}
}

func (o *observer) record(ev *raft.Event) {
	obs := observation{
		At:      time.Now(),
		Type:    ev.Type.String(),
		Term:    ev.Term,
		Peer:    ev.Peer,
		Leader:  ev.Leader,
		Index:   ev.Index,
		Voter:   ev.Voter,
		Dropped: ev.Dropped,
	}
	if ev.Err != nil {
		obs.Err = ev.Err.Error()
	}

	o.mu.Lock()
	// Dropped is the number of events discarded before this one reached us.
	// Counting it is the difference between "nothing happened" and "we stopped
	// listening for a while", which are not the same thing to report.
	if ev.Dropped > 0 {
		o.dropped += ev.Dropped
		o.logger.Warn("watchtower: events were dropped; this history has gaps",
			"dropped", ev.Dropped, "total_dropped", o.dropped)
	}
	o.term = ev.Term

	switch ev.Type {
	case raft.EventLeadershipChanged:
		o.leader, o.isLeader = ev.Leader, ev.IsLeader

	case raft.EventPeerAdded, raft.EventPeerPromoted, raft.EventPeerDemoted:
		p := o.peerLocked(ev.Peer)
		p.Voter = ev.Voter
		p.LastChange = obs.At
		// A peer is assumed reachable until it is reported otherwise; the node
		// only reports transitions.
		if ev.Type == raft.EventPeerAdded {
			p.Reachable = true
		}

	case raft.EventPeerRemoved:
		delete(o.peers, ev.Peer)

	case raft.EventPeerUnresponsive:
		p := o.peerLocked(ev.Peer)
		p.Reachable = false
		p.Unreachable++
		p.LastChange = obs.At

	case raft.EventPeerResponsive:
		p := o.peerLocked(ev.Peer)
		p.Reachable = true
		p.LastChange = obs.At

	case raft.EventSnapshotStarted, raft.EventSnapshotCompleted, raft.EventSnapshotFailed:
		snap := obs
		o.lastSnapshot = &snap

	case raft.EventNodeFailed:
		// The last thing this node will ever report: it stopped because a
		// durable write failed, and nothing else is coming.
		o.failed = obs.Err
		o.logger.Error("watchtower: the node has stopped and will not recover on its own",
			"err", obs.Err)
	}

	o.recent = append(o.recent, obs)
	if len(o.recent) > o.maxRecent {
		o.recent = o.recent[len(o.recent)-o.maxRecent:]
	}

	subs := make([]chan observation, 0, len(o.subscribers))
	for ch := range o.subscribers {
		subs = append(subs, ch)
	}
	o.mu.Unlock()

	// Fan out without blocking: a slow SSE client must not stop the observer,
	// for the same reason the node does not block on the observer.
	for _, ch := range subs {
		select {
		case ch <- obs:
		default:
		}
	}
}

func (o *observer) peerLocked(id raft.NodeID) *peerState {
	p, ok := o.peers[id]
	if !ok {
		p = &peerState{ID: id, Reachable: true}
		o.peers[id] = p
	}
	return p
}

// subscribe returns a channel of observations and a function to end it.
func (o *observer) subscribe() (events <-chan observation, unsubscribe func()) {
	ch := make(chan observation, 64)
	o.mu.Lock()
	o.subscribers[ch] = struct{}{}
	o.mu.Unlock()

	return ch, func() {
		o.mu.Lock()
		delete(o.subscribers, ch)
		o.mu.Unlock()
	}
}

// snapshotState returns what the observer currently believes.
func (o *observer) snapshotState() map[string]any {
	o.mu.RLock()
	defer o.mu.RUnlock()

	peers := make([]peerState, 0, len(o.peers))
	for _, p := range o.peers {
		peers = append(peers, *p)
	}
	recent := make([]observation, len(o.recent))
	copy(recent, o.recent)

	state := map[string]any{
		"term":           o.term,
		"leader":         o.leader,
		"is_leader":      o.isLeader,
		"peers":          peers,
		"recent":         recent,
		"events_dropped": o.dropped,
	}
	if o.lastSnapshot != nil {
		state["last_snapshot"] = *o.lastSnapshot
	}
	if o.failed != "" {
		state["node_failed"] = o.failed
	}
	return state
}

// unreachablePeers is the answer to the question worth alerting on.
func (o *observer) unreachablePeers() []raft.NodeID {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var out []raft.NodeID
	for id, p := range o.peers {
		if !p.Reachable {
			out = append(out, id)
		}
	}
	return out
}
