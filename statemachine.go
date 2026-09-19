package raft

import (
	"context"
	"io"
)

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

	// Snapshot serialises the current state machine state into writer.
	// The snapshot is taken at the given log index and term, which should
	// match the last applied entry.
	Snapshot(ctx context.Context, w io.Writer) error

	// Restore replaces the current state machine state with the snapshot
	// read from reader. Called when installing a snapshot received from
	// the leader.
	Restore(ctx context.Context, meta SnapshotMeta, r io.Reader) error
}

// SnapshotCapturer is an optional interface a StateMachine may also implement
// so that taking a snapshot does not block applying entries.
//
// Snapshot serialises the whole state, and it is called on the goroutine that
// applies entries, because the two must not run at once. Everything committed
// during that serialisation therefore waits for it: on a large state machine
// that is a pause in apply, and so in the latency of every proposal, once per
// SnapshotThreshold entries.
//
// A state machine that can capture its state cheaply -- a persistent data
// structure, a copy-on-write map, a storage engine with its own snapshots --
// can avoid that pause. Capture is called on the apply goroutine and should
// return quickly; the returned Snapshot is then written on a background
// goroutine while entries keep applying.
//
// The captured state must not change afterwards, however the state machine
// goes on to be mutated. A capture that shares mutable structure with the live
// state produces a snapshot that never existed, mixing entries from either side
// of the capture point, which is not recoverable from and not detectable.
//
// When a state machine does not implement this, Snapshot is used and apply
// waits, as before.
type SnapshotCapturer interface {
	StateMachine

	// Capture takes a point-in-time handle on the current state. It is called
	// on the apply goroutine, with no Apply in progress, and should return
	// without doing the serialisation itself.
	Capture(ctx context.Context) (Snapshot, error)
}

// Snapshot is a captured state-machine state, serialised later and off the
// apply path. See SnapshotCapturer.
type Snapshot interface {
	// Write serialises the captured state. It is called once, on a background
	// goroutine, while entries continue to apply.
	Write(ctx context.Context, w io.Writer) error

	// Release frees whatever the capture is holding. It is called exactly once
	// after Write returns, successfully or not, and also when a capture is
	// discarded without being written.
	Release()
}
