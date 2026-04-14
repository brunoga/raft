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
//	func (t *OtelTracer) StartRPC(nodeID, peer raft.NodeID, rpcType string) func(error) {
//	    _, span := t.tracer.Start(context.Background(), rpcType,
//	        trace.WithAttributes(
//	            attribute.String("raft.node", string(nodeID)),
//	            attribute.String("raft.peer", string(peer)),
//	        ))
//	    return func(err error) {
//	        if err != nil { span.RecordError(err) }
//	        span.End()
//	    }
//	}
package rpctracer

import (
	"log/slog"
	"time"

	"github.com/brunoga/raft"
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
func (t *SlogTracer) StartRPC(nodeID raft.NodeID, peer raft.NodeID, rpcType string) func(err error) {
	start := time.Now()
	return func(err error) {
		ms := time.Since(start).Milliseconds()
		if err != nil {
			t.logger.Warn("raft rpc failed",
				"node", string(nodeID),
				"peer", string(peer),
				"rpc", rpcType,
				"duration_ms", ms,
				"err", err,
			)
		} else {
			t.logger.Debug("raft rpc ok",
				"node", string(nodeID),
				"peer", string(peer),
				"rpc", rpcType,
				"duration_ms", ms,
			)
		}
	}
}
