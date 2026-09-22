package rpctracer_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/metrics/rpctracer"
)

func TestSlogTracer_FinishCalledOnce(t *testing.T) {
	tr := rpctracer.New(slog.Default())
	_, finish := tr.StartRPC(context.Background(), raft.NodeID("n1"), raft.NodeID("n2"), raft.RPCAppendEntries)
	finish(nil) // success path
}

func TestSlogTracer_FinishWithError(t *testing.T) {
	tr := rpctracer.New(slog.Default())
	_, finish := tr.StartRPC(context.Background(), raft.NodeID("n1"), raft.NodeID("n2"), raft.RPCRequestVote)
	finish(errors.New("connection refused")) // error path
}

func TestSlogTracer_NilLoggerUsesDefault(t *testing.T) {
	tr := rpctracer.New(nil) // should not panic
	_, finish := tr.StartRPC(context.Background(), raft.NodeID("n1"), raft.NodeID("n2"), raft.RPCTimeoutNow)
	finish(nil)
}

// Compile-time check: SlogTracer implements raft.Tracer.
var _ raft.Tracer = (*rpctracer.SlogTracer)(nil)
