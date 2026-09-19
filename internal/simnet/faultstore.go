package simnet

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/brunoga/raft"
)

// ErrDiskFailure is the error a [FaultStore] returns from every mutating call
// while write failures are armed.
var ErrDiskFailure = errors.New("simnet: simulated storage failure")

// FaultStore wraps a raft.Storage so a test can take the disk away from a node.
//
// It supports three faults, in increasing order of nastiness:
//
//   - FailWrites makes every mutating call return an error. This is a legal
//     fault: real disks fill up and fail, and a Raft node is expected to cope
//     (by refusing to acknowledge anything it could not persist) rather than to
//     lie about what it has on stable storage.
//
//   - Crash rolls the store back to the last durable point and is the basis of
//     crash-restart testing. With the default write-through behaviour every
//     returned write is durable, so Crash is a clean power-cut: the node is
//     stopped, the store keeps exactly what it acknowledged, and a fresh Node
//     is built on top of it.
//
//   - SetBuffered(true) makes the store acknowledge writes it has not made
//     durable, so a subsequent Crash loses the tail. This models storage that
//     lies about fsync. Raft's safety proof assumes it never does, so enabling
//     this is a way to demonstrate what a lying disk costs, not a way to test
//     Raft: a safety violation found with buffering on is a property of the
//     disk, not of the consensus implementation.
//
// Snapshot writes and prefix truncation are always treated as durable. Both are
// checkpoints of state that is already durable in the log, so rolling them back
// would only model a fault that cannot lose committed data.
//
// Safe for concurrent use.
type FaultStore struct {
	inner raft.Storage

	mu sync.Mutex
	// failErr, when non-nil, is returned by every mutating call.
	failErr error
	// buffered reports whether acknowledged writes are allowed to be lost on a
	// crash.
	buffered bool
	// durableLast is the highest log index known to have reached stable
	// storage; durableHS is the hard state known to have reached it.
	durableLast raft.Index
	durableHS   raft.HardState
	// counters for reporting.
	writes, failures int
}

// NewFaultStore wraps inner. The wrapper assumes inner is in a fully durable
// state at construction time, which is true both for a fresh store and for one
// being reopened after a crash.
func NewFaultStore(inner raft.Storage) *FaultStore {
	f := &FaultStore{inner: inner}
	last, err := inner.LastIndex()
	if err == nil {
		f.durableLast = last
	}
	if hs, err := inner.LoadHardState(context.Background()); err == nil {
		f.durableHS = hs
	}
	return f
}

// Inner returns the wrapped store. Tests read log contents through it, which
// deliberately bypasses the injected faults: the invariant checker wants to
// know what is actually on disk, not what the node is currently allowed to see.
func (f *FaultStore) Inner() raft.Storage { return f.inner }

// FailWrites arms write failures. Every subsequent SaveHardState,
// AppendLogEntries, TruncateSuffix, TruncatePrefix and SaveSnapshot returns
// ErrDiskFailure until AllowWrites is called. Reads keep working, which is what
// a full disk or a read-only remount looks like.
func (f *FaultStore) FailWrites() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failErr = ErrDiskFailure
}

// AllowWrites disarms write failures.
func (f *FaultStore) AllowWrites() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failErr = nil
}

// SetBuffered controls whether acknowledged writes are durable. See the type
// documentation for why turning this on changes what a test result means.
func (f *FaultStore) SetBuffered(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !b {
		f.syncLocked()
	}
	f.buffered = b
}

// Sync makes everything written so far durable.
func (f *FaultStore) Sync() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncLocked()
}

func (f *FaultStore) syncLocked() {
	if last, err := f.inner.LastIndex(); err == nil {
		f.durableLast = last
	}
	if hs, err := f.inner.LoadHardState(context.Background()); err == nil {
		f.durableHS = hs
	}
}

// Crash discards everything that was written but not made durable, leaving the
// store exactly as a power cut would. The caller must have stopped the Node
// first; restarting means building a new Node on this same store.
//
// With write-through behaviour (the default) nothing is lost and Crash only
// asserts that fact.
func (f *FaultStore) Crash(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failErr = nil
	last, err := f.inner.LastIndex()
	if err != nil {
		return err
	}
	if last > f.durableLast {
		if err := f.inner.TruncateSuffix(ctx, f.durableLast+1); err != nil {
			return err
		}
	}
	hs, err := f.inner.LoadHardState(ctx)
	if err != nil {
		return err
	}
	if hs != f.durableHS {
		if err := f.inner.SaveHardState(ctx, f.durableHS); err != nil {
			return err
		}
	}
	return nil
}

// Stats returns the number of mutating calls made and the number of those that
// were failed by injection.
func (f *FaultStore) Stats() (writes, failures int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes, f.failures
}

// beginWrite records the call and reports the injected error, if any.
func (f *FaultStore) beginWrite() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.failErr != nil {
		f.failures++
		return f.failErr
	}
	return nil
}

// endWrite advances the durable point unless the store is buffering.
func (f *FaultStore) endWrite() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.buffered {
		f.syncLocked()
	}
}

// --- raft.Storage -----------------------------------------------------------

func (f *FaultStore) SaveHardState(ctx context.Context, hs raft.HardState) error {
	if err := f.beginWrite(); err != nil {
		return err
	}
	if err := f.inner.SaveHardState(ctx, hs); err != nil {
		return err
	}
	f.endWrite()
	return nil
}

func (f *FaultStore) LoadHardState(ctx context.Context) (raft.HardState, error) {
	return f.inner.LoadHardState(ctx)
}

func (f *FaultStore) AppendLogEntries(ctx context.Context, entries []raft.LogEntry) error {
	if err := f.beginWrite(); err != nil {
		return err
	}
	if err := f.inner.AppendLogEntries(ctx, entries); err != nil {
		return err
	}
	f.endWrite()
	return nil
}

func (f *FaultStore) GetLogEntry(ctx context.Context, index raft.Index) (raft.LogEntry, error) {
	return f.inner.GetLogEntry(ctx, index)
}

func (f *FaultStore) GetLogEntries(ctx context.Context, lo, hi raft.Index) ([]raft.LogEntry, error) {
	return f.inner.GetLogEntries(ctx, lo, hi)
}

func (f *FaultStore) FirstIndex() (raft.Index, error) { return f.inner.FirstIndex() }

func (f *FaultStore) LastIndex() (raft.Index, error) { return f.inner.LastIndex() }

func (f *FaultStore) TruncateSuffix(ctx context.Context, fromIndex raft.Index) error {
	if err := f.beginWrite(); err != nil {
		return err
	}
	if err := f.inner.TruncateSuffix(ctx, fromIndex); err != nil {
		return err
	}
	f.mu.Lock()
	// A truncation that removes durable entries moves the durable point back;
	// it has itself been made durable by the time it returns.
	if fromIndex > 0 && fromIndex-1 < f.durableLast {
		f.durableLast = fromIndex - 1
	}
	f.mu.Unlock()
	f.endWrite()
	return nil
}

func (f *FaultStore) TruncatePrefix(ctx context.Context, toIndex raft.Index) error {
	if err := f.beginWrite(); err != nil {
		return err
	}
	if err := f.inner.TruncatePrefix(ctx, toIndex); err != nil {
		return err
	}
	f.Sync()
	return nil
}

func (f *FaultStore) SaveSnapshot(ctx context.Context, meta raft.SnapshotMeta, r io.Reader) error {
	if err := f.beginWrite(); err != nil {
		return err
	}
	if err := f.inner.SaveSnapshot(ctx, meta, r); err != nil {
		return err
	}
	f.Sync()
	return nil
}

func (f *FaultStore) LoadSnapshot(ctx context.Context) (raft.SnapshotMeta, io.ReadCloser, error) {
	return f.inner.LoadSnapshot(ctx)
}

// SnapshotIndex returns the last index covered by the store's snapshot, or 0
// when there is none. It exists so the invariant checker can tell "this node
// never had that entry" from "this node compacted that entry away".
func (f *FaultStore) SnapshotIndex() raft.Index {
	meta, rc, err := f.inner.LoadSnapshot(context.Background())
	if err != nil {
		return 0
	}
	_ = rc.Close()
	return meta.LastIncludedIndex
}

// Close does not close the wrapped store: a crash-restart cycle reuses it, and
// the test that created it owns its lifetime.
func (f *FaultStore) Close() error { return nil }

// Compile-time interface check.
var _ raft.Storage = (*FaultStore)(nil)
