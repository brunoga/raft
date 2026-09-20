package raft

import (
	"context"
	"io"
)

// BatchWriter is an optional interface a Storage may implement to make the
// hard state and a run of log entries durable in a single operation.
//
// It exists because the two arrive together. A follower that learns of a new
// term and receives entries in the same message records the term and appends
// the entries back to back, and a store that keeps both in one log can write
// them as one record and pay one fsync instead of two. The engine cannot use
// such a store without a way to say "these belong together", and adding that
// to Storage itself later would break every implementation that exists by
// then, so the seam is here from the start.
//
// A Storage that does not implement it is called as before, one method at a
// time, in the same order.
type BatchWriter interface {
	// SaveState persists hs and appends entries as one durable operation, and
	// must fsync before returning.
	//
	// hs is nil when the batch carries no hard state, and entries is empty
	// when it carries none; both are never empty at once. When both are
	// present the hard state must be made durable no later than the entries,
	// so that a log recovered after a crash never contains entries from a term
	// the node does not believe it reached.
	//
	// The same contiguity rule as AppendLogEntries applies to entries.
	SaveState(ctx context.Context, hs *HardState, entries []LogEntry) error
}

// CommitRecorder is an optional interface a Storage may implement to remember
// how far the log had committed.
//
// Raft treats the commit index as volatile, and this engine agrees: a
// restarting node learns it again from its leader, so nothing in normal
// operation needs it on disk. Disaster recovery does. A node whose cluster
// lost its quorum for good has a log that splits at the highest index it can
// prove was committed, and everything above that split is a coin toss --
// either it committed on the majority that died, or it was still in flight.
// Without this interface the only proof left on disk is the snapshot's last
// included index, so a node that snapshots every few thousand entries leaves
// a band that wide for an operator to guess about. See RecoverCluster.
//
// The value recorded is always one the node could prove at the time: an index
// that had committed and that this node's own log held durably. It is a lower
// bound, never an estimate, so a value that is stale or lost costs nothing
// beyond a wider band.
//
// A Storage that does not implement it works exactly as before.
type CommitRecorder interface {
	// SaveCommitIndex records index as committed and durable on this node.
	//
	// It need not fsync, and it need not be immediate: losing the most recent
	// value to a crash only widens the band recovery has to guess about,
	// whereas an fsync here would put a synchronous disk write on a path that
	// runs every time the commit index moves. What it must never do is report
	// a value it did not receive, or one larger.
	SaveCommitIndex(ctx context.Context, index Index) error

	// LoadCommitIndex returns the last recorded index, or 0 if none was ever
	// recorded or the record did not survive.
	LoadCommitIndex(ctx context.Context) (Index, error)
}

// Storage is the persistence seam between the Raft engine and any storage
// backend. All mutating methods must durably persist their data (fsync) before
// returning so that the Raft safety invariants hold across crashes.
//
// Implementations must be safe for concurrent use only to the extent that the
// Raft engine calls them: HardState and log operations are always issued from
// one goroutine and never overlap each other, but Snapshot operations may be
// called from a separate goroutine and may overlap them.
//
// That one goroutine is not the goroutine that runs Raft itself. Log and hard
// state writes are queued and carried out behind it, so that a slow disk
// delays only what depends on the disk rather than stopping the node from
// counting election ticks and answering its peers. Nothing about the order the
// calls arrive in changes: they are made one at a time, in the order the
// engine issued them.
//
// Log indices are 1-based. Index 0 is reserved as a sentinel meaning "no
// entry".
type Storage interface {
	// --- Hard state ---------------------------------------------------------

	// SaveHardState atomically persists currentTerm and votedFor.
	// Must fsync before returning.
	SaveHardState(ctx context.Context, hs HardState) error

	// LoadHardState returns the last saved HardState.
	// Returns a zero-value HardState (term=0, votedFor="") if nothing has
	// been saved yet.
	LoadHardState(ctx context.Context) (HardState, error)

	// --- Log ----------------------------------------------------------------

	// AppendLogEntries appends entries to the log in order.
	// The caller guarantees entries are contiguous and follow the current
	// last entry. Must fsync before returning.
	AppendLogEntries(ctx context.Context, entries []LogEntry) error

	// GetLogEntry returns the single entry at the given index.
	// Returns ErrNotFound if the index is out of range.
	GetLogEntry(ctx context.Context, index Index) (LogEntry, error)

	// GetLogEntries returns all entries in the half-open range [lo, hi).
	// Returns ErrNotFound if any index in the range is out of range.
	GetLogEntries(ctx context.Context, lo, hi Index) ([]LogEntry, error)

	// FirstIndex returns the index of the first available log entry.
	// Returns 0 if the log is empty (or fully compacted).
	// This is a synchronous, non-blocking call and does not require a context.
	FirstIndex() (Index, error)

	// LastIndex returns the index of the last log entry.
	// Returns 0 if the log is empty.
	// This is a synchronous, non-blocking call and does not require a context.
	LastIndex() (Index, error)

	// TruncateSuffix deletes all entries with index >= fromIndex.
	// Used when a follower discovers its log conflicts with the leader's.
	// Must fsync before returning.
	TruncateSuffix(ctx context.Context, fromIndex Index) error

	// TruncatePrefix deletes all entries with index < toIndex.
	// Used after a snapshot is taken or installed to reclaim space.
	// Must fsync before returning.
	TruncatePrefix(ctx context.Context, toIndex Index) error

	// --- Snapshot -----------------------------------------------------------

	// SaveSnapshot durably stores a snapshot and its metadata. The snapshot
	// data is read from r. The previous snapshot (if any) may be discarded
	// after this returns. Must fsync before returning.
	SaveSnapshot(ctx context.Context, meta SnapshotMeta, r io.Reader) error

	// LoadSnapshot returns the most recently saved snapshot metadata and
	// a reader for its data. The caller is responsible for closing the
	// reader. Returns ErrNoSnapshot if no snapshot exists yet.
	LoadSnapshot(ctx context.Context) (SnapshotMeta, io.ReadCloser, error)

	// --- Lifecycle ----------------------------------------------------------

	// Close releases all resources held by the storage backend.
	Close() error
}
