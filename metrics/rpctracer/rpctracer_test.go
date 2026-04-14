package rpctracer_test

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/brunoga/raft/metrics/rpctracer"
	"github.com/brunoga/raft"
)

func TestSlogTracer_FinishCalledOnce(t *testing.T) {
	tr := rpctracer.New(slog.Default())
	finish := tr.StartRPC(raft.NodeID("n1"), raft.NodeID("n2"), "AppendEntries")
	finish(nil) // success path
}

func TestSlogTracer_FinishWithError(t *testing.T) {
	tr := rpctracer.New(slog.Default())
	finish := tr.StartRPC(raft.NodeID("n1"), raft.NodeID("n2"), "RequestVote")
	finish(errors.New("connection refused")) // error path
}

func TestSlogTracer_NilLoggerUsesDefault(t *testing.T) {
	tr := rpctracer.New(nil) // should not panic
	finish := tr.StartRPC(raft.NodeID("n1"), raft.NodeID("n2"), "TimeoutNow")
	finish(nil)
}

// Compile-time check: SlogTracer implements raft.Tracer.
var _ raft.Tracer = (*rpctracer.SlogTracer)(nil)
