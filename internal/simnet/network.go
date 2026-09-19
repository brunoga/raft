package simnet

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/brunoga/raft"
)

// Errors reported to the Raft node when the simulator decides an RPC does not
// complete. They are deliberately opaque: a Raft node must treat any RPC
// failure the same way, so the tests do not let it distinguish them.
var (
	// ErrDropped is returned when the request or its response was lost.
	ErrDropped = errors.New("simnet: message lost")

	// ErrUnreachable is returned when no handler is registered for the
	// destination, which models a node that is down.
	ErrUnreachable = errors.New("simnet: destination unreachable")

	// ErrClosed is returned once the Network has been closed.
	ErrClosed = errors.New("simnet: network closed")
)

// RPC identifies which Raft RPC a simulated message carries.
type RPC uint8

// The five RPCs of the raft.Transport interface.
const (
	RequestVote RPC = iota
	AppendEntries
	InstallSnapshot
	TimeoutNow
	ReadIndex
)

func (r RPC) String() string {
	switch r {
	case RequestVote:
		return "RequestVote"
	case AppendEntries:
		return "AppendEntries"
	case InstallSnapshot:
		return "InstallSnapshot"
	case TimeoutNow:
		return "TimeoutNow"
	case ReadIndex:
		return "ReadIndex"
	default:
		return "Unknown"
	}
}

// Message is a single in-flight RPC as seen by the simulator. It is passed to
// the filter installed with [Network.SetFilter] so that a test can make
// delivery depend on the content of the message — for example, dropping only
// the AppendEntries RPCs that carry an entry from the leader's own term, which
// is how the Figure 8 scenario is constructed.
type Message struct {
	// Seq is a network-wide monotonically increasing submission number.
	Seq uint64
	// From and To are the sending and receiving node identities.
	From, To raft.NodeID
	// RPC says which of the five Raft RPCs this is.
	RPC RPC
	// Req is the concrete request: *raft.RequestVoteRequest,
	// *raft.AppendEntriesRequest, *raft.InstallSnapshotRequest,
	// *raft.TimeoutNowRequest or *raft.ReadIndexRequest.
	Req any
}

func (m Message) String() string {
	var extra string
	if ae, ok := m.Req.(*raft.AppendEntriesRequest); ok {
		extra = fmt.Sprintf(" term=%d prev=%d/%d entries=%d commit=%d",
			ae.Term, ae.PrevLogIndex, ae.PrevLogTerm, len(ae.Entries), ae.LeaderCommit)
	}
	if rv, ok := m.Req.(*raft.RequestVoteRequest); ok {
		extra = fmt.Sprintf(" term=%d lastLog=%d/%d preVote=%v",
			rv.Term, rv.LastLogIndex, rv.LastLogTerm, rv.PreVote)
	}
	return fmt.Sprintf("#%d %s→%s %s%s", m.Seq, m.From, m.To, m.RPC, extra)
}

// LinkPolicy describes the behaviour of one directed link. The zero value is a
// perfect link: no loss, no duplication, no delay.
//
// Loss and Duplicate are probabilities in [0, 1]. Latency for each leg of the
// round-trip is drawn uniformly from [MinLatency, MaxLatency); with probability
// Reorder an extra ReorderDelay is added to the request leg, which is what
// actually causes two messages sent in one order to arrive in the other.
type LinkPolicy struct {
	// Loss is the probability that a request is lost. The response leg is lost
	// with the same probability, so the observable end-to-end failure rate is
	// roughly 2×Loss.
	Loss float64
	// Duplicate is the probability that a delivered request is delivered a
	// second time. The duplicate's response is discarded, exactly as a real
	// retransmission's would be by the sender's already-satisfied call.
	Duplicate float64
	// MinLatency and MaxLatency bound the per-leg delay.
	MinLatency, MaxLatency time.Duration
	// Reorder is the probability that a request is additionally held for
	// ReorderDelay, overtaking later messages on the same link.
	Reorder      float64
	ReorderDelay time.Duration
}

// Lossless returns a policy with a small uniform latency and no faults. It is
// the sensible default for a test that wants realistic message interleaving
// without any actual failures.
func Lossless() LinkPolicy {
	return LinkPolicy{MinLatency: 0, MaxLatency: 500 * time.Microsecond}
}

// Flaky returns a policy that loses, duplicates, delays and reorders messages
// at rates high enough to shake out retry and idempotency bugs while still
// letting a cluster make progress.
func Flaky() LinkPolicy {
	return LinkPolicy{
		Loss:         0.05,
		Duplicate:    0.05,
		MinLatency:   0,
		MaxLatency:   2 * time.Millisecond,
		Reorder:      0.10,
		ReorderDelay: 5 * time.Millisecond,
	}
}

type link [2]raft.NodeID

// Network is the central simulator. Every RPC sent by every [Transport] it
// creates passes through it, and every decision about that RPC's fate is drawn
// from the Network's single seeded generator.
//
// Safe for concurrent use.
type Network struct {
	seed uint64

	mu       sync.Mutex
	rng      *rand.Rand
	seq      uint64
	handlers map[raft.NodeID]raft.Handler
	def      LinkPolicy
	policies map[link]LinkPolicy
	cut      map[link]bool
	filter   func(Message) bool
	events   []string
	faults   []string
	start    time.Time

	closeOnce sync.Once
	closeCh   chan struct{}
	bgCtx     context.Context
	bgCancel  context.CancelFunc
	dupWG     sync.WaitGroup
}

// maxEvents bounds the rolling event log so a long soak run cannot exhaust
// memory. The fault schedule is kept separately and is never trimmed.
const maxEvents = 4096

// New returns a Network whose every random decision derives from seed.
func New(seed uint64) *Network {
	n := &Network{
		seed:     seed,
		rng:      rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		handlers: make(map[raft.NodeID]raft.Handler),
		def:      Lossless(),
		policies: make(map[link]LinkPolicy),
		cut:      make(map[link]bool),
		closeCh:  make(chan struct{}),
		start:    time.Now(),
	}
	n.bgCtx, n.bgCancel = context.WithCancel(context.Background())
	return n
}

// Seed returns the value the Network was constructed with. Print it on failure;
// it is the only thing a maintainer needs to replay the run.
func (n *Network) Seed() uint64 { return n.seed }

// SetDefaultPolicy sets the policy used by every link that has no specific
// policy of its own.
func (n *Network) SetDefaultPolicy(p LinkPolicy) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.def = p
	n.recordFaultLocked("default link policy = %+v", p)
}

// SetLinkPolicy overrides the policy for the directed link from → to.
func (n *Network) SetLinkPolicy(from, to raft.NodeID, p LinkPolicy) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.policies[link{from, to}] = p
	n.recordFaultLocked("link %s→%s policy = %+v", from, to, p)
}

// Cut takes down the directed link from → to. The reverse direction is left
// alone, so Cut is how an asymmetric partition is built: after Cut(a, b), a's
// RequestVote RPCs never reach b but b's heartbeats still reach a.
func (n *Network) Cut(from, to raft.NodeID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cut[link{from, to}] = true
	n.recordFaultLocked("cut %s→%s", from, to)
}

// Restore brings the directed link from → to back up.
func (n *Network) Restore(from, to raft.NodeID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.cut, link{from, to})
	n.recordFaultLocked("restore %s→%s", from, to)
}

// Isolate cuts every link into and out of id.
func (n *Network) Isolate(id raft.NodeID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for other := range n.handlers {
		if other == id {
			continue
		}
		n.cut[link{id, other}] = true
		n.cut[link{other, id}] = true
	}
	n.recordFaultLocked("isolate %s", id)
}

// Heal restores every link into and out of id.
func (n *Network) Heal(id raft.NodeID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for l := range n.cut {
		if l[0] == id || l[1] == id {
			delete(n.cut, l)
		}
	}
	n.recordFaultLocked("heal %s", id)
}

// HealAll restores every link in the network.
func (n *Network) HealAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	clear(n.cut)
	n.recordFaultLocked("heal all")
}

// SetFilter installs a content-aware delivery predicate. Returning false drops
// the message. A nil filter (the default) delivers everything the policy and
// the cut set allow. The filter runs while the Network lock is held, so it must
// not call back into the Network.
func (n *Network) SetFilter(f func(Message) bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.filter = f
}

// RecordFault appends a line to the fault schedule. Tests call it when they
// crash a node or otherwise inject a fault the Network itself cannot see, so
// that the schedule printed on failure is complete.
func (n *Network) RecordFault(format string, args ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.recordFaultLocked(format, args...)
}

func (n *Network) recordFaultLocked(format string, args ...any) {
	n.faults = append(n.faults,
		fmt.Sprintf("%8.3fms  %s", float64(time.Since(n.start).Microseconds())/1000, fmt.Sprintf(format, args...)))
}

func (n *Network) recordEventLocked(format string, args ...any) {
	if len(n.events) >= maxEvents {
		n.events = append(n.events[:0], n.events[len(n.events)/2:]...)
	}
	n.events = append(n.events,
		fmt.Sprintf("%8.3fms  %s", float64(time.Since(n.start).Microseconds())/1000, fmt.Sprintf(format, args...)))
}

// FaultSchedule returns the ordered list of faults injected so far, one per
// line. This is the second half of a reproduction: the seed says how the
// simulator will behave, the schedule says what the test did to it.
func (n *Network) FaultSchedule() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return strings.Join(n.faults, "\n")
}

// RecentEvents returns up to limit of the most recent per-message events the
// simulator considered noteworthy (losses, duplicates, reorder spikes).
func (n *Network) RecentEvents(limit int) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	ev := n.events
	if limit > 0 && len(ev) > limit {
		ev = ev[len(ev)-limit:]
	}
	return strings.Join(ev, "\n")
}

// Register makes handler reachable at id.
func (n *Network) Register(id raft.NodeID, handler raft.Handler) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers[id] = handler
}

// Unregister makes id unreachable, which is how a stopped node looks to its
// peers.
func (n *Network) Unregister(id raft.NodeID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.handlers, id)
}

// Close shuts the Network down and waits for any duplicate-delivery goroutines
// to finish, so that no simulated message can touch a node after a test has
// returned.
func (n *Network) Close() error {
	n.closeOnce.Do(func() {
		close(n.closeCh)
		n.bgCancel()
	})
	n.dupWG.Wait()
	return nil
}

// plan is the simulator's verdict on one message. Every field is decided in a
// single critical section so the draws from the generator happen in submission
// order.
type plan struct {
	seq         uint64
	dropReq     bool
	dropResp    bool
	duplicate   bool
	reqDelay    time.Duration
	respDelay   time.Duration
	dupDelay    time.Duration
	unreachable bool
}

func (n *Network) policyFor(from, to raft.NodeID) LinkPolicy {
	if p, ok := n.policies[link{from, to}]; ok {
		return p
	}
	return n.def
}

func drawLatency(rng *rand.Rand, lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(rng.Int64N(int64(hi-lo)))
}

// decide draws this message's fate. Exactly six values are taken from the
// generator for every message regardless of the outcome, so that a cut link or
// a filtered message does not shift the stream for everything that follows.
func (n *Network) decide(m *Message) plan {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.seq++
	m.Seq = n.seq

	p := n.policyFor(m.From, m.To)
	lossRoll := n.rng.Float64()
	respLossRoll := n.rng.Float64()
	dupRoll := n.rng.Float64()
	reorderRoll := n.rng.Float64()
	reqLat := drawLatency(n.rng, p.MinLatency, p.MaxLatency)
	respLat := drawLatency(n.rng, p.MinLatency, p.MaxLatency)

	pl := plan{
		seq:       m.Seq,
		dropReq:   lossRoll < p.Loss,
		dropResp:  respLossRoll < p.Loss,
		duplicate: dupRoll < p.Duplicate,
		reqDelay:  reqLat,
		respDelay: respLat,
		dupDelay:  reqLat / 2,
	}
	if reorderRoll < p.Reorder {
		pl.reqDelay += p.ReorderDelay
		n.recordEventLocked("reorder %s (+%s)", m, p.ReorderDelay)
	}
	if n.cut[link{m.From, m.To}] {
		pl.dropReq = true
		pl.duplicate = false
	} else if n.filter != nil && !n.filter(*m) {
		pl.dropReq = true
		pl.duplicate = false
		n.recordEventLocked("filtered %s", m)
	}
	if _, ok := n.handlers[m.To]; !ok {
		pl.unreachable = true
	}
	if pl.dropReq {
		n.recordEventLocked("lose-request %s", m)
	} else if pl.dropResp {
		n.recordEventLocked("lose-response %s", m)
	}
	if pl.duplicate {
		n.recordEventLocked("duplicate %s", m)
	}
	return pl
}

func (n *Network) handlerFor(id raft.NodeID) raft.Handler {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.handlers[id]
}

// sleep blocks for d, or until ctx is done or the Network closes.
func (n *Network) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-n.closeCh:
			return ErrClosed
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-n.closeCh:
		return ErrClosed
	}
}

// dispatch is the whole life of one RPC: the simulator decides its fate, the
// calling goroutine is blocked for the simulated request latency, the handler
// runs (possibly twice), and the calling goroutine is blocked again for the
// response latency.
//
// Blocking the caller is what makes a fault-injecting simulator fit behind
// raft.Transport, which is a synchronous request/response interface.
func (n *Network) dispatch(
	ctx context.Context,
	from, to raft.NodeID,
	rpc RPC,
	req any,
	call func(raft.Handler) (any, error),
) (any, error) {
	select {
	case <-n.closeCh:
		return nil, ErrClosed
	default:
	}

	m := Message{From: from, To: to, RPC: rpc, Req: req}
	pl := n.decide(&m)

	if err := n.sleep(ctx, pl.reqDelay); err != nil {
		return nil, err
	}
	if pl.dropReq {
		return nil, ErrDropped
	}
	if pl.unreachable {
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, to)
	}
	h := n.handlerFor(to)
	if h == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, to)
	}

	if pl.duplicate {
		n.deliverDuplicate(to, pl.dupDelay, call)
	}

	resp, err := call(h)
	if err != nil {
		return nil, err
	}
	if err := n.sleep(ctx, pl.respDelay); err != nil {
		return nil, err
	}
	if pl.dropResp {
		return nil, ErrDropped
	}
	return resp, nil
}

// deliverDuplicate re-delivers a request after a delay and throws the response
// away, modelling a retransmission the sender never sees. Raft must be immune
// to this: every RPC handler is required to be idempotent.
func (n *Network) deliverDuplicate(to raft.NodeID, delay time.Duration, call func(raft.Handler) (any, error)) {
	n.dupWG.Add(1)
	go func() {
		defer n.dupWG.Done()
		if err := n.sleep(n.bgCtx, delay); err != nil {
			return
		}
		if h := n.handlerFor(to); h != nil {
			_, _ = call(h)
		}
	}()
}

// NewTransport returns a raft.Transport bound to this Network under the
// identity id.
func (n *Network) NewTransport(id raft.NodeID) *Transport {
	return &Transport{id: id, net: n}
}

// Transport is one node's view of the simulated network. It satisfies
// raft.Transport.
type Transport struct {
	id  raft.NodeID
	net *Network
}

func (t *Transport) RequestVote(ctx context.Context, to raft.NodeID, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	v, err := t.net.dispatch(ctx, t.id, to, RequestVote, req, func(h raft.Handler) (any, error) {
		return h.HandleRequestVote(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return v.(*raft.RequestVoteResponse), nil
}

func (t *Transport) AppendEntries(ctx context.Context, to raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	v, err := t.net.dispatch(ctx, t.id, to, AppendEntries, req, func(h raft.Handler) (any, error) {
		return h.HandleAppendEntries(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return v.(*raft.AppendEntriesResponse), nil
}

func (t *Transport) InstallSnapshot(ctx context.Context, to raft.NodeID, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	v, err := t.net.dispatch(ctx, t.id, to, InstallSnapshot, req, func(h raft.Handler) (any, error) {
		return h.HandleInstallSnapshot(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return v.(*raft.InstallSnapshotResponse), nil
}

func (t *Transport) TimeoutNow(ctx context.Context, to raft.NodeID, req *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	v, err := t.net.dispatch(ctx, t.id, to, TimeoutNow, req, func(h raft.Handler) (any, error) {
		return h.HandleTimeoutNow(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return v.(*raft.TimeoutNowResponse), nil
}

func (t *Transport) ReadIndex(ctx context.Context, to raft.NodeID, req *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	v, err := t.net.dispatch(ctx, t.id, to, ReadIndex, req, func(h raft.Handler) (any, error) {
		return h.HandleReadIndex(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return v.(*raft.ReadIndexResponse), nil
}

// Register makes id reachable on the underlying Network.
func (t *Transport) Register(id raft.NodeID, handler raft.Handler) { t.net.Register(id, handler) }

// Unregister makes id unreachable on the underlying Network.
func (t *Transport) Unregister(id raft.NodeID) { t.net.Unregister(id) }

// Close is a no-op: the Network owns the lifetime, not the per-node transport.
func (t *Transport) Close() error { return nil }

// Compile-time interface check.
var _ raft.Transport = (*Transport)(nil)
