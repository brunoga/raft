package raft_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
)

// TestTracer_ContextReachesTheTransport is the point of giving Tracer a
// context at all.
//
// A tracer that is handed no context can only time an RPC. It cannot make its
// span a child of anything, and it cannot put trace context on the wire,
// because the context the transport is about to use never passes through it.
// The documented OpenTelemetry example in metrics/rpctracer was exactly that
// shape and produced orphan spans.
//
// The context a tracer returns is the one the RPC is made with, which this
// checks by having the tracer stamp it and the transport look for the stamp.
func TestTracer_ContextReachesTheTransport(t *testing.T) {
	seen := make(chan bool, 8)
	tr := &stampingTracer{}

	cfg := safeBaseConfig(t, "n1")
	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}}
	cfg.Tracer = tr
	cfg.Transport = &stampCheckingTransport{saw: seen}

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	stop := tickWhile(node)
	defer stop()

	select {
	case stamped := <-seen:
		if !stamped {
			t.Error("the transport was called with a context the tracer never saw")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no RPC was sent")
	}
}

type stampKey struct{}

type stampingTracer struct{}

func (stampingTracer) StartRPC(ctx context.Context, _, _ raft.NodeID, rpcType raft.RPCType) (rpcCtx context.Context, finish func(error)) {
	return context.WithValue(ctx, stampKey{}, rpcType), func(error) {}
}

// stampCheckingTransport reports whether each outbound call carried the
// tracer's stamp.
type stampCheckingTransport struct {
	echoTransport
	saw chan bool
}

func (t *stampCheckingTransport) report(ctx context.Context) {
	_, ok := ctx.Value(stampKey{}).(raft.RPCType)
	select {
	case t.saw <- ok:
	default:
	}
}

func (t *stampCheckingTransport) RequestVote(ctx context.Context, to raft.NodeID, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	t.report(ctx)
	return t.echoTransport.RequestVote(ctx, to, req)
}

func (t *stampCheckingTransport) AppendEntries(ctx context.Context, to raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	t.report(ctx)
	return t.echoTransport.AppendEntries(ctx, to, req)
}

// TestShutdown_GivesUpOnTheDeadline covers the escape hatch from a stop that
// cannot finish.
//
// Stop finishes the storage writes the node accepted before it returns, which
// is what makes an orderly restart keep the tail of its log. The cost is that
// a storage backend which has hung rather than failed holds Stop there for as
// long as it hangs, and a process coming down on a deadline needs to be able
// to say so.
func TestShutdown_GivesUpOnTheDeadline(t *testing.T) {
	node, store := newGatedFollower(t, nil)

	release := store.hold(t)
	defer release()

	// Give the node a write that will never complete.
	go func() {
		_, _ = node.Handler().HandleAppendEntries(context.Background(), appendFrom(1, 1, 3, 0))
	}()
	store.awaitHeld(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := node.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown returned %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Shutdown waited %s past its deadline", elapsed)
	}

	// Releasing the disk lets the shutdown it started finish.
	release()
	done := make(chan struct{})
	go func() { node.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("the node never finished stopping after the write was released")
	}
}

// TestShutdown_ReturnsNilWhenItCompletes pins the ordinary case.
func TestShutdown_ReturnsNilWhenItCompletes(t *testing.T) {
	node, _ := newGatedFollower(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := node.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown of a healthy node returned %v, want nil", err)
	}
}
