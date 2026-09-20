package raft

import (
	"maps"
	"slices"
	"sync"
)

// observerQueueDepth is how many events one subscription may fall behind
// before it starts losing them, and being told so by Event.Dropped.
//
// It is large enough that a consumer doing ordinary work between receives
// never loses anything, since these events are rare, and small enough that a
// consumer which has stopped receiving altogether costs this node a bounded
// amount of memory instead of an unbounded one.
const observerQueueDepth = 64

// EventType says what a delivered Event describes. The zero value is
// EventLeadershipChanged, so an Event is never ambiguous by omission.
type EventType uint8

const (
	// EventLeadershipChanged reports that this node's role changed, or that
	// its view of who the leader is did. It is the signal to start or stop the
	// work only a leader may do, and to point a client at the node that can
	// accept writes. The same transition is reported by LeadershipChanges;
	// this is it delivered in the same stream as everything else, for a
	// consumer that wants one ordered view rather than two.
	EventLeadershipChanged EventType = iota
	// EventPeerAdded reports that a committed configuration change brought a
	// peer into the cluster. It is what tells an operator that a join actually
	// took effect, as opposed to having been proposed: membership changes at
	// commit time, not when the request was made.
	EventPeerAdded
	// EventPeerRemoved reports that a committed configuration change took a
	// peer out of the cluster. It is the point at which it is safe to
	// decommission that machine, and the point at which the quorum this node
	// needs got smaller. Peer is this node's own ID when it was itself removed.
	EventPeerRemoved
	// EventPeerPromoted reports that a peer that was a learner is now a voter,
	// and so now counts towards every quorum. A promotion that arrives before
	// the learner has caught up is the classic way to lose availability with a
	// voter that cannot vote, which is why the moment it takes effect is worth
	// seeing.
	EventPeerPromoted
	// EventPeerDemoted reports that a peer that was a voter is now a learner.
	// It still receives the log but no longer counts towards a quorum, which
	// is usually the first half of removing it.
	EventPeerDemoted
	// EventPeerUnresponsive reports that a follower has failed to answer
	// several AppendEntries RPCs in a row, so this leader cannot replicate to
	// it. One unresponsive follower in a three-node cluster means the next one
	// costs the cluster its quorum, and nothing in the Raft state says so:
	// the leader carries on normally until the moment it cannot commit at all.
	EventPeerUnresponsive
	// EventPeerResponsive reports that a follower reported unresponsive is
	// answering again. It exists so that a consumer can close the incident it
	// opened on EventPeerUnresponsive rather than having to poll for recovery.
	EventPeerResponsive
	// EventSnapshotStarted reports that a snapshot has begun, either of this
	// node's own state machine or of one a leader is sending it. Origin says
	// which. Snapshots are the expensive thing a Raft node does, and pairing a
	// start with its completion is what turns "snapshots are slow" into a
	// duration.
	EventSnapshotStarted
	// EventSnapshotCompleted reports that a snapshot finished successfully.
	// Index is the last log index it covers, which is the point the log was
	// compacted to and the point a restarting node would resume from.
	EventSnapshotCompleted
	// EventSnapshotFailed reports that a snapshot did not finish, with Err
	// saying why. A node whose snapshots keep failing is one whose log grows
	// without bound and whose restart gets slower every time, and neither
	// shows up as an error anywhere a client can see.
	EventSnapshotFailed
	// EventNodeFailed reports that this node stopped because a write Raft's
	// safety argument depends on did not reach stable storage. Err carries the
	// failure. The node is gone from the cluster's point of view from here on,
	// so this is the event that should page somebody: nothing else about it
	// will recover on its own.
	EventNodeFailed
)

// String returns the event type's name, as it appears in logs and dashboards.
func (t EventType) String() string {
	switch t {
	case EventLeadershipChanged:
		return "LeadershipChanged"
	case EventPeerAdded:
		return "PeerAdded"
	case EventPeerRemoved:
		return "PeerRemoved"
	case EventPeerPromoted:
		return "PeerPromoted"
	case EventPeerDemoted:
		return "PeerDemoted"
	case EventPeerUnresponsive:
		return "PeerUnresponsive"
	case EventPeerResponsive:
		return "PeerResponsive"
	case EventSnapshotStarted:
		return "SnapshotStarted"
	case EventSnapshotCompleted:
		return "SnapshotCompleted"
	case EventSnapshotFailed:
		return "SnapshotFailed"
	case EventNodeFailed:
		return "NodeFailed"
	default:
		return "Unknown"
	}
}

// SnapshotOrigin says which of the two very different things called a snapshot
// an Event describes: the one this node produces, or the one it is given.
type SnapshotOrigin uint8

const (
	// SnapshotLocal is a snapshot of this node's own state machine, taken
	// because the log grew past SnapshotThreshold. Its cost is this node's
	// disk and its state machine's serialisation.
	SnapshotLocal SnapshotOrigin = iota
	// SnapshotReceived is a snapshot a leader sent to this node because the
	// entries it needed had already been compacted away. It says this node
	// fell far enough behind that catching up from the log was no longer
	// possible, which is a different problem from a slow local snapshot and
	// wants a different response.
	SnapshotReceived
)

// String returns the origin's name, as it appears in logs and dashboards.
func (o SnapshotOrigin) String() string {
	switch o {
	case SnapshotLocal:
		return "Local"
	case SnapshotReceived:
		return "Received"
	default:
		return "Unknown"
	}
}

// Event is one thing that happened to a node, delivered to the subscriptions
// created by Node.Events.
//
// Which fields carry anything depends on Type; each is documented with the
// types that set it. The rest are left at their zero value.
type Event struct {
	// Type says what happened. Read it first: it decides which of the fields
	// below mean anything.
	Type EventType
	// Term is the term this node was in when the event happened. It is set for
	// every event, and it is what orders two events that describe the same
	// peer: a report from an older term has been overtaken.
	Term Term
	// IsLeader is true when this node is the leader.
	// Set by EventLeadershipChanged.
	IsLeader bool
	// Leader is the node this node believes is the leader, empty when it does
	// not know. Set by EventLeadershipChanged.
	Leader NodeID
	// Peer is the peer the event is about. Set by every EventPeer event; it is
	// this node's own ID on an EventPeerRemoved reporting that this node was
	// removed from the cluster.
	Peer NodeID
	// Voter is the peer's role after the change: true for a voter, false for a
	// learner. Set by EventPeerAdded, EventPeerPromoted and EventPeerDemoted.
	Voter bool
	// Index is the last log index the snapshot covers. Set by the snapshot
	// events; on EventSnapshotStarted it is the index the snapshot is being
	// taken at, which is the index it will cover if it succeeds.
	Index Index
	// Origin says whether the snapshot is this node's own or one a leader sent
	// it. Set by the snapshot events.
	Origin SnapshotOrigin
	// Err is the failure being reported. Set by EventSnapshotFailed and
	// EventNodeFailed, and nil everywhere else.
	Err error
	// Dropped is how many events were discarded between the previous event
	// this subscription received and this one, because it was not keeping up.
	// It is normally zero.
	//
	// A non-zero value means the stream is not a complete history any more:
	// the events behind it are gone and cannot be recovered, so a consumer
	// that keeps state derived from them has to rebuild it from Members,
	// State and the other accessors rather than carry on from what it has.
	Dropped uint64
}

// observer is one registered subscription plus the bookkeeping needed to tell
// it that it missed something.
type observer struct {
	ch chan Event
	// dropped counts the events discarded since this subscription last
	// received one. Guarded by watchersMu; reported as Event.Dropped on the
	// next event that does get through.
	dropped uint64
}

// Events returns a channel reporting significant changes in this node's
// operation, and a function that ends the subscription.
//
// Metrics say how often something happened; these say that a particular thing
// happened, when, and to whom. That difference is the reason to subscribe: a
// counter of configuration changes does not name the peer that joined, and a
// replication-latency histogram does not say which follower stopped answering,
// which is the only fact that decides whether the next failure costs the
// cluster its quorum. A node that stopped because a durable write failed
// reports it here too, since it will not report anything else afterwards.
//
// Delivery is bounded and lossy, deliberately. The channel holds 64 events and
// the node never blocks on it: a consumer that stops receiving loses events
// rather than stalling consensus for every other client of this node. Nothing
// is lost silently, though. The next event a lagging consumer does receive
// carries the number of events discarded before it in Event.Dropped, so a
// consumer can tell a complete history from a partial one and rebuild its view
// from the accessors when it has to.
//
// Events are delivered from the event loop in the order they happened, so two
// events about the same peer never arrive reversed. The channel is closed when
// the node stops, which is what makes a range over it end.
//
// The returned stop function may be called more than once, from any goroutine,
// and must be called to release the subscription. It never blocks.
//
//	events, stop := node.Events()
//	defer stop()
//	for ev := range events {
//		switch ev.Type {
//		case raft.EventPeerUnresponsive:
//			openIncident(ev.Peer)
//		case raft.EventPeerResponsive:
//			closeIncident(ev.Peer)
//		}
//	}
func (n *Node) Events() (events <-chan Event, stop func()) {
	ch := make(chan Event, observerQueueDepth)

	n.watchersMu.Lock()
	select {
	case <-n.stopCh:
		// Already stopped: hand back a closed channel so a range over it ends
		// immediately rather than blocking for ever.
		n.watchersMu.Unlock()
		close(ch)
		return ch, func() {}
	default:
	}
	if n.observers == nil {
		n.observers = make(map[uint64]*observer)
	}
	n.nextObserver++
	id := n.nextObserver
	n.observers[id] = &observer{ch: ch}
	n.watchersMu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			n.watchersMu.Lock()
			defer n.watchersMu.Unlock()
			if existing, ok := n.observers[id]; ok {
				delete(n.observers, id)
				close(existing.ch)
			}
		})
	}
}

// emit delivers ev to every subscription. Event-loop only, because it stamps
// the event with the current term.
func (n *Node) emit(ev *Event) {
	n.watchersMu.Lock()
	defer n.watchersMu.Unlock()
	n.emitLocked(ev)
}

// emitLocked is emit for a caller that already holds watchersMu, which is how
// a leadership change is announced to the watchers and the observers under one
// acquisition rather than two.
//
// The send is non-blocking and the lock is held only for its duration, so a
// subscriber that has stopped receiving costs the event loop the time of a
// failed channel send and nothing else. That is the trade this makes: the
// event is dropped, counted, and reported to that subscriber later.
func (n *Node) emitLocked(ev *Event) {
	if len(n.observers) == 0 {
		return
	}
	ev.Term = n.currentTerm
	for _, o := range n.observers {
		e := *ev
		e.Dropped = o.dropped
		select {
		case o.ch <- e:
			o.dropped = 0
		default:
			o.dropped++
		}
	}
}

// closeObservers ends every subscription. Called once during shutdown.
//
// Buffered events survive the close, so a consumer that ranges over the
// channel still sees everything delivered before the node stopped, including
// the failure that stopped it.
func (n *Node) closeObservers() {
	n.watchersMu.Lock()
	defer n.watchersMu.Unlock()
	for id, o := range n.observers {
		delete(n.observers, id)
		close(o.ch)
	}
}

// peerUnresponsiveFailures is how many AppendEntries RPCs to one peer must
// fail in a row before it is reported unresponsive.
//
// It is not one, because a single failed RPC is ordinary: a peer restarting,
// a connection being re-established, a packet lost. Three consecutive
// failures span several heartbeat intervals and mean the peer is not there,
// which is the thing worth waking somebody for. Recovery is reported on the
// first RPC that succeeds, because a peer that answers is back whatever it was
// doing before.
const peerUnresponsiveFailures = 3

// peerHealth is what the leader remembers about one peer in order to report
// when it stops and resumes answering. Event-loop only.
type peerHealth struct {
	// failures counts AppendEntries RPCs that failed outright since the last
	// one that got an answer.
	failures int
	// down is true once EventPeerUnresponsive has been emitted for this peer
	// and EventPeerResponsive has not, so that each transition is reported
	// once rather than once per failed RPC.
	down bool
}

// peerReachability tells the event loop that a heartbeat pump's view of
// whether it can reach its peer has changed.
//
// Only the change is sent, in either direction. A peer that is simply up costs
// no messages, and -- which is what matters -- neither does a peer that is
// simply down: an unreachable peer is retried every heartbeat interval, so a
// message per failure would put one message per peer per interval into the
// event loop's queue for as long as the outage lasted, which on a partitioned
// cluster is the moment it can least afford the traffic.
type peerReachability struct {
	peer      NodeID
	reachable bool
}

// notePeerUnreachable records an AppendEntries RPC to peer that never got an
// answer, and reports the peer unresponsive once enough of them have failed in
// a row. Event-loop only.
func (n *Node) notePeerUnreachable(peer NodeID) {
	if n.peerHealth == nil {
		n.peerHealth = make(map[NodeID]peerHealth)
	}
	h := n.peerHealth[peer]
	h.failures++
	n.peerHealth[peer] = h
	if h.failures >= peerUnresponsiveFailures {
		n.markPeerDown(peer)
	}
}

// markPeerDown records that peer cannot be reached and reports it once.
// Event-loop only.
func (n *Node) markPeerDown(peer NodeID) {
	if n.peerHealth == nil {
		n.peerHealth = make(map[NodeID]peerHealth)
	}
	h := n.peerHealth[peer]
	if h.down {
		return
	}
	h.down = true
	n.peerHealth[peer] = h
	n.emit(&Event{Type: EventPeerUnresponsive, Peer: peer})
}

// markPeerUp records that peer can be reached again and reports it if it had
// been reported unreachable. Event-loop only.
func (n *Node) markPeerUp(peer NodeID) {
	h, ok := n.peerHealth[peer]
	if !ok {
		return
	}
	delete(n.peerHealth, peer)
	if h.down {
		n.emit(&Event{Type: EventPeerResponsive, Peer: peer})
	}
}

// notePeerResponded records that peer answered an AppendEntries RPC, and
// reports it responsive again if it had been reported unresponsive. A
// rejection counts: what is being tracked is whether the peer is reachable,
// not whether it agreed. Event-loop only.
func (n *Node) notePeerResponded(peer NodeID) {
	n.markPeerUp(peer)
}

// resetPeerHealth forgets what was known about every peer's reachability.
//
// Called when this node becomes leader, because the state describes RPCs this
// node made as the previous leader, possibly terms ago. Keeping it would make
// the first successful heartbeat of a new term report a recovery that did not
// happen in it; a peer that is still down is reported again, from the failures
// this term produces.
func (n *Node) resetPeerHealth() {
	n.peerHealth = nil
}

// membershipRoles returns the cluster's membership as a map from node ID to
// whether that node is a voter, this node included. Event-loop only.
func (n *Node) membershipRoles() map[NodeID]bool {
	roles := make(map[NodeID]bool, len(n.cfg.Peers)+1)
	for _, p := range n.cfg.Peers {
		roles[p.ID] = p.Voter
	}
	roles[n.cfg.ID] = n.cfg.Voter
	return roles
}

// emitMembershipChanges reports what a committed configuration change did to
// the membership, by comparing the two views of it.
//
// Diffing rather than emitting from each branch that edits the membership is
// what keeps the four ways a role can change -- a direct add, a direct remove,
// a joint-consensus entry and the finalise entry that ends it -- reporting the
// same events. A change that does nothing, which a replayed entry is, reports
// nothing. Event-loop only.
func (n *Node) emitMembershipChanges(before, after map[NodeID]bool) {
	for _, id := range slices.Sorted(maps.Keys(before)) {
		wasVoter := before[id]
		isVoter, stillThere := after[id]
		switch {
		case !stillThere:
			n.emit(&Event{Type: EventPeerRemoved, Peer: id, Voter: wasVoter})
		case isVoter && !wasVoter:
			n.emit(&Event{Type: EventPeerPromoted, Peer: id, Voter: true})
		case !isVoter && wasVoter:
			n.emit(&Event{Type: EventPeerDemoted, Peer: id, Voter: false})
		}
	}
	for _, id := range slices.Sorted(maps.Keys(after)) {
		if _, existed := before[id]; !existed {
			n.emit(&Event{Type: EventPeerAdded, Peer: id, Voter: after[id]})
		}
	}
}
