package raft

import "context"

// StateMachine is the application-level state machine driven by Raft.
// Implementations must be deterministic: applying the same sequence of
// commands must always produce the same state.
//
// All methods receive a context so that long-running operations respect
// cancellation and deadlines.
type StateMachine interface {
	// Apply applies a committed log entry to the state machine and returns
	// the result that will be delivered to the client that proposed it.
	Apply(ctx context.Context, entry LogEntry) (result []byte, err error)

	// Snapshot serialises the current state machine state into a byte slice.
	// The snapshot is taken at the given log index and term, which should
	// match the last applied entry.
	Snapshot(ctx context.Context) (data []byte, err error)

	// Restore replaces the current state machine state with the supplied
	// snapshot. Called when installing a snapshot received from the leader.
	Restore(ctx context.Context, meta SnapshotMeta, data []byte) error
}
