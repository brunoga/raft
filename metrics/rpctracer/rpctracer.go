// Package rpctracer provides a structured-logging implementation of the
// raft.Tracer interface. It records each outbound Raft RPC with its type,
// destination peer, duration, and outcome using the standard library's
// log/slog package — no external dependencies required.
//
// Usage:
//
//	cfg.Tracer = rpctracer.New(nil)          // uses slog.Default()
//	cfg.Tracer = rpctracer.New(myLogger)     // custom slog.Logger
//
// Successful RPCs are logged at Debug level; failed RPCs (network errors,
// timeouts) are logged at Warn level so they surface in production without
// drowning out normal traffic.
//
// To plug in OpenTelemetry or another tracing backend, implement raft.Tracer
// directly using the same StartRPC / finish-func pattern:
//
//	type OtelTracer struct{ tracer trace.Tracer }
//
//	func (t *OtelTracer) StartRPC(ctx context.Context, nodeID, peer raft.NodeID,
//	    rpcType raft.RPCType) (context.Context, func(error)) {
//
//	    ctx, span := t.tracer.Start(ctx, string(rpcType),
//	        trace.WithAttributes(
//	            attribute.String("raft.node", string(nodeID)),
//	            attribute.String("raft.peer", string(peer)),
//	        ))
//	    // Returning ctx is what makes the span a parent of anything the
//	    // transport does, and what lets a propagator put the trace context
//	    // on the wire.
//	    return ctx, func(err error) {
//	        if err != nil { span.RecordError(err) }
//	        span.End()
//	    }
//	}
package rpctracer

import (
	"context"
	"log/slog"
	"time"

	"github.com/brunoga/raft/v2"
)

// SlogTracer is a raft.Tracer that logs RPC calls via slog.
type SlogTracer struct {
	logger *slog.Logger
}

// New returns a SlogTracer. If logger is nil, slog.Default() is used.
func New(logger *slog.Logger) *SlogTracer {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogTracer{logger: logger}
}

// StartRPC implements raft.Tracer. It records the start time and returns a
// finish func that logs the completed RPC with its duration and outcome.
//
// The context is returned unchanged: this tracer measures, and has nothing to
// attach to the outbound call.
func (t *SlogTracer) StartRPC(ctx context.Context, nodeID, peer raft.NodeID, rpcType raft.RPCType) (rpcCtx context.Context, finish func(err error)) {
	start := time.Now()
	return ctx, func(err error) {
		ms := time.Since(start).Milliseconds()
		if err != nil {
			t.logger.Warn("raft rpc failed",
				"node", string(nodeID),
				"peer", string(peer),
				"rpc", string(rpcType),
				"duration_ms", ms,
				"err", err,
			)
		} else {
			t.logger.Debug("raft rpc ok",
				"node", string(nodeID),
				"peer", string(peer),
				"rpc", string(rpcType),
				"duration_ms", ms,
			)
		}
	}
}
