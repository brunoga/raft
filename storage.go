package raft

import (
	"context"
	"io"
)

// Storage is the persistence seam between the Raft engine and any storage
// backend. All mutating methods must durably persist their data (fsync) before
// returning so that the Raft safety invariants hold across crashes.
//
// Implementations must be safe for concurrent use only to the extent that the
// Raft engine calls them: HardState and log operations are always called from
// the single Raft goroutine, but Snapshot operations may be called from a
// separate goroutine.
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
	FirstIndex(ctx context.Context) (Index, error)

	// LastIndex returns the index of the last log entry.
	// Returns 0 if the log is empty.
	LastIndex(ctx context.Context) (Index, error)

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
