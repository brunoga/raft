package easyraft

import (
	"testing"

	"github.com/brunoga/raft/v2"
)

// storeFSM is what the Raft layer is given, and it has to satisfy the
// interface.
var _ raft.StateMachine = storeFSM{}

// TestStore_DoesNotImplementStateMachine pins the reason storeFSM exists.
//
// raft.StateMachine requires Apply, Snapshot and Restore to be exported. On
// Store they would sit among the methods an application is meant to call, with
// nothing to tell them apart -- and calling any of them directly applies a
// change consensus never agreed to, on this replica alone, leaving this node's
// state permanently different from every other node's with nothing in the log
// to explain it. Divergence with no error and no trace is the worst failure
// this package can produce, and the only defence against it in Go's type
// system is not to have the method.
//
// Implementing the interface on Store again would compile, pass every other
// test, and quietly restore that method to the autocomplete list.
func TestStore_DoesNotImplementStateMachine(t *testing.T) {
	var s any = &Store{}
	if _, ok := s.(raft.StateMachine); ok {
		t.Error("*Store implements raft.StateMachine: Apply, Snapshot and Restore are reachable " +
			"on the type applications hold, and calling one diverges this replica silently. " +
			"Keep them on storeFSM.")
	}
}
