package raft_test

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// barrierGate wraps a transport and can hold read-barrier heartbeats in flight,
// which is what lets a test place a second read request inside the window of a
// barrier round that is already outstanding. It also records the identity of
// every barrier round it sees.
type barrierGate struct {
	raft.Transport

	mu       sync.Mutex
	gens     map[uint64]int
	holding  bool
	released chan struct{}
}

func newBarrierGate(inner raft.Transport) *barrierGate {
	return &barrierGate{
		Transport: inner,
		gens:      make(map[uint64]int),
		released:  make(chan struct{}),
	}
}

func (g *barrierGate) AppendEntries(ctx context.Context, to raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	if req.ReadBarrier != 0 {
		g.mu.Lock()
		g.gens[req.ReadBarrier]++
		hold := g.holding
		released := g.released
		g.mu.Unlock()

		if hold {
			select {
			case <-released:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return g.Transport.AppendEntries(ctx, to, req)
}

func (g *barrierGate) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holding = true
}

func (g *barrierGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.holding {
		return
	}
	g.holding = false
	close(g.released)
	g.released = make(chan struct{})
}

// roundsSeen reports how many distinct barrier rounds have been observed. A
// round may be re-broadcast under the same identity when a heartbeat is lost,
// so counting distinct identities counts rounds, not messages.
func (g *barrierGate) roundsSeen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.gens)
}

func (g *barrierGate) sawAnyRound() bool { return g.roundsSeen() > 0 }

// readIndexCluster is a cluster whose transports can gate read barriers.
type readIndexCluster struct {
	ids   []raft.NodeID
	nodes []*raft.Node
	gates []*barrierGate
	net   *memtransport.Network
}

func newReadIndexCluster(t *testing.T, n int) *readIndexCluster {
	t.Helper()

	c := &readIndexCluster{net: memtransport.NewNetwork()}
	for i := range n {
		c.ids = append(c.ids, raft.NodeID(fmt.Sprintf("n%d", i+1)))
	}
	for i := range n {
		var peers []raft.PeerConfig
		for j := range n {
			if i != j {
				peers = append(peers, raft.PeerConfig{ID: c.ids[j], Voter: true})
			}
		}
		gate := newBarrierGate(c.net.NewTransport(c.ids[i]))

		cfg := raft.DefaultConfig()
		cfg.ID = c.ids[i]
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = &barrierSM{}
		cfg.Transport = gate
		cfg.TickInterval = 0
		cfg.SnapshotThreshold = 0
		cfg.CheckQuorum = false // barriers are held deliberately in this test

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(%s): %v", c.ids[i], err)
		}
		c.net.Register(cfg.ID, node.Handler())
		c.nodes = append(c.nodes, node)
		c.gates = append(c.gates, gate)
	}
	for _, node := range c.nodes {
		node.Start()
	}
	t.Cleanup(func() {
		for _, g := range c.gates {
			g.release()
		}
		for _, node := range c.nodes {
			node.Stop()
		}
	})
	return c
}

type barrierSM struct{}

func (barrierSM) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }
func (barrierSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("s"))
	return err
}
func (barrierSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// tickForever drives the cluster until the returned stop function is called.
func (c *readIndexCluster) tickForever() func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			for _, n := range c.nodes {
				n.Tick()
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func (c *readIndexCluster) leaderIndex() int {
	for i, n := range c.nodes {
		if n.State() == raft.Leader {
			return i
		}
	}
	return -1
}

// TestReadIndex_RequestIsConfirmedByARoundThatStartedAfterIt asserts that a
// read is answered only by a leadership confirmation that began after the read
// arrived.
//
// A ReadIndex confirmation proves the node was still leader when the heartbeat
// went out. A read that arrives after that heartbeat was sent, and is then
// answered by its replies, is being told about a moment that had already passed
// when the read was made: leadership may have moved in between, and the read
// then returns a commit index from a leader that had already been replaced.
// Confirming it requires a fresh round.
func TestReadIndex_RequestIsConfirmedByARoundThatStartedAfterIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := newReadIndexCluster(t, 3)
	stopTicking := c.tickForever()
	defer stopTicking()

	deadline := time.Now().Add(5 * time.Second)
	for c.leaderIndex() < 0 {
		if time.Now().After(deadline) {
			t.Fatal("no leader elected")
		}
		time.Sleep(time.Millisecond)
	}
	li := c.leaderIndex()
	leader := c.nodes[li]
	gate := c.gates[li]

	// A committed entry in this term, so reads are served rather than queued
	// behind the leader's initial no-op.
	if _, err := leader.Propose(ctx, []byte("entry")); err != nil {
		t.Fatalf("propose: %v", err)
	}

	gate.hold()

	// First read: starts a barrier round, which the gate holds in flight.
	firstDone := make(chan error, 1)
	go func() {
		_, err := leader.ReadIndex(ctx)
		firstDone <- err
	}()

	deadline = time.Now().Add(5 * time.Second)
	for !gate.sawAnyRound() {
		if time.Now().After(deadline) {
			t.Fatal("leader never sent a read barrier")
		}
		time.Sleep(time.Millisecond)
	}
	roundsAtFirstRead := gate.roundsSeen()

	// Second read, arriving while the first round is still outstanding. Its
	// answer must not come from that round.
	secondDone := make(chan error, 1)
	go func() {
		_, err := leader.ReadIndex(ctx)
		secondDone <- err
	}()
	time.Sleep(50 * time.Millisecond) // give it time to reach the event loop

	gate.release()

	for _, ch := range []chan error{firstDone, secondDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("ReadIndex: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("ReadIndex never returned")
		}
	}

	if got := gate.roundsSeen(); got <= roundsAtFirstRead {
		t.Errorf("both reads were answered by %d barrier round(s); the second read "+
			"arrived after the first round was already in flight and needed a round of its own",
			got)
	}
}

// TestReadIndex_ConcurrentReadsStillShareARound asserts the batching that makes
// ReadIndex cheap is intact: reads that arrive together are answered by one
// confirmation round, not one round each.
func TestReadIndex_ConcurrentReadsStillShareARound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := newReadIndexCluster(t, 3)
	stopTicking := c.tickForever()
	defer stopTicking()

	deadline := time.Now().Add(5 * time.Second)
	for c.leaderIndex() < 0 {
		if time.Now().After(deadline) {
			t.Fatal("no leader elected")
		}
		time.Sleep(time.Millisecond)
	}
	li := c.leaderIndex()
	leader := c.nodes[li]
	gate := c.gates[li]

	if _, err := leader.Propose(ctx, []byte("entry")); err != nil {
		t.Fatalf("propose: %v", err)
	}

	const readers = 16
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	before := gate.roundsSeen()
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := leader.ReadIndex(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("ReadIndex: %v", err)
	}

	if rounds := gate.roundsSeen() - before; rounds > readers {
		t.Errorf("%d concurrent reads cost %d barrier rounds; batching is not working",
			readers, rounds)
	}
}
