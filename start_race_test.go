package raft_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

type raceSM struct{}

func (raceSM) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }
func (raceSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("s"))
	return err
}
func (raceSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// TestStart_IsRaceFreeAgainstQueuedRequests asserts that Start does not touch
// state the event loop owns once that loop is running.
//
// The window is narrow in ordinary use, so the test widens it: requests that
// raise the term are queued before Start, which means the event loop begins
// writing the term in the same instant Start is finishing its own bookkeeping.
// The race detector reports only races it observes, and this is what makes it
// observe this one.
func TestStart_IsRaceFreeAgainstQueuedRequests(t *testing.T) {
	const queued = 64

	net := memtransport.NewNetwork()
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
	cfg.Storage = memstore.New()
	cfg.StateMachine = raceSM{}
	cfg.Transport = net.NewTransport("n1")
	cfg.TickInterval = 0

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.Register("n1", node.Handler())
	t.Cleanup(node.Stop)

	// Queue work that will make the event loop write the term as soon as it
	// starts. The request channel is buffered, so these calls park waiting for
	// a response rather than for the loop to exist.
	var wg sync.WaitGroup
	for i := range queued {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = node.Handler().HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
				Term:        raft.Term(i + 2),
				CandidateID: "n2",
			})
		}(i)
	}
	time.Sleep(20 * time.Millisecond) // let the requests queue up

	node.Start()
	wg.Wait()
}
