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

// DurableStateMachine is an optional interface a StateMachine may implement
// when it keeps its own state on durable storage rather than rebuilding it in
// memory.
//
// Without it, a node coming back from a restart has to reconstruct the state
// machine from a snapshot and then replay every entry after it, because the
// engine has no way to know what the state machine already has. For a state
// machine that is itself a database, that work has already been done and is
// sitting on the disk: what is missing is a way for it to say so.
//
// Implementing it is a promise about durability, and it is the whole of the
// contract. The index reported must be one whose effect, and the effect of
// every entry before it, is on stable storage. What must never happen is a
// gap: an index reported as applied while the effect of some earlier entry was
// lost. The engine will not replay anything at or below the reported index
// again -- that is the point -- so a gap becomes permanent divergence from
// every other replica, with nothing in the log to explain it.
//
// A state machine that batches its writes satisfies this by making them
// durable before ApplyBatch returns, which is one sync per batch rather than
// one per entry.
type DurableStateMachine interface {
	// AppliedIndex returns the highest log index whose effect is durable in
	// this state machine's own storage, or zero if it has applied nothing.
	//
	// It is called once, while the node is being constructed, before anything
	// is applied.
	AppliedIndex(ctx context.Context) (Index, error)
}

// ApplyOutcome is what a BatchApplier reports for one entry: the result to
// hand back to whoever proposed it, and the error if the state machine
// rejected it.
//
// Err here is the state machine saying no to that one command, which is an
// ordinary outcome and does not stop the node. It is delivered to the
// proposer exactly as the error from Apply would be, and must be deterministic
// across replicas for the same reason: a command that fails on one node and
// succeeds on another leaves the two with different state and, for a
// ProposeOnce command, different dedup tables.
type ApplyOutcome struct {
	// Value is the result handed back to the caller that proposed the entry.
	Value []byte
	// Err is the state machine's rejection of this command, or nil.
	Err error
}

// BatchApplier is an optional interface a StateMachine may implement to apply
// several committed entries in one call.
//
// It exists because a state machine backed by storage almost always has a way
// to group work -- one transaction, one write batch, one fsync -- and applying
// entries one at a time denies it that. The entries arrive in runs already:
// the apply loop is handed everything committed since it last looked, which
// under load is a batch of dozens.
//
// A state machine that does not implement this is called through Apply, one
// entry at a time, exactly as before.
type BatchApplier interface {
	// ApplyBatch applies entries in order and returns one outcome per entry,
	// in the same order.
	//
	// The returned error is for a failure of the batch as a whole, such as a
	// transaction that could not commit, and is reported to every entry in it.
	// A single command the state machine rejects is not that: report it as the
	// Err of its own outcome and return a nil error.
	//
	// Returning a number of outcomes that does not match the number of entries
	// is a programming error, and the node stops rather than guess which
	// result belongs to which entry.
	ApplyBatch(ctx context.Context, entries []LogEntry) ([]ApplyOutcome, error)
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
