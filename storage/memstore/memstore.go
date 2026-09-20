// Package memstore provides an in-memory implementation of raft.Storage.
//
// Nothing it holds survives the process. It exists for tests and for
// deployments whose state can be rebuilt from somewhere else, and it is the
// wrong choice anywhere the Raft safety argument is being relied on: that
// argument assumes a node's term, vote and log entries outlive a crash, and
// here they do not. Use storage/filestore for anything durable.
//
// Safe for concurrent use.
package memstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/brunoga/raft"
)

// MemStore is a strictly in-memory implementation of raft.Storage.
// It is primarily intended for tests and simulations; all state is lost
// when the process exits.
//
// MemStore is the reference implementation of the raft.Storage contract, so it
// deliberately models the same ownership and error semantics as a real
// persistent backend: entries handed to it are copied on the way in, entries
// handed back are copies on the way out, and compacted entries are released
// rather than kept alive by a shared backing array.
type MemStore struct {
	mu sync.RWMutex

	// Hard state
	hs raft.HardState

	// Log
	entries []raft.LogEntry

	// Snapshot
	hasSnap  bool
	snapMeta raft.SnapshotMeta
	snapData []byte
}

// New creates an empty MemStore.
func New() *MemStore {
	return &MemStore{}
}

// --- Hard state -------------------------------------------------------------

func (m *MemStore) SaveHardState(_ context.Context, hs raft.HardState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hs = hs
	return nil
}

func (m *MemStore) LoadHardState(_ context.Context) (raft.HardState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hs, nil
}

// --- Log --------------------------------------------------------------------

// AppendLogEntries appends entries to the log. Each entry's Command is copied,
// so a caller that reuses its buffers cannot corrupt the stored log — the same
// guarantee a backend that serialises to disk provides for free.
func (m *MemStore) AppendLogEntries(_ context.Context, entries []raft.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, e := range entries {
		m.entries = append(m.entries, cloneEntry(e))
	}
	return nil
}

func (m *MemStore) GetLogEntry(_ context.Context, index raft.Index) (raft.LogEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.entries) == 0 {
		return raft.LogEntry{}, fmt.Errorf("%w: index %d (log is empty)", raft.ErrNotFound, index)
	}
	first := m.entries[0].Index
	last := m.entries[len(m.entries)-1].Index
	if index < first || index > last {
		return raft.LogEntry{}, fmt.Errorf("%w: index %d not in [%d,%d]",
			raft.ErrNotFound, index, first, last)
	}
	return cloneEntry(m.entries[index-first]), nil
}

func (m *MemStore) GetLogEntries(_ context.Context, lo, hi raft.Index) ([]raft.LogEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if lo >= hi {
		return nil, nil
	}
	if len(m.entries) == 0 {
		return nil, fmt.Errorf("%w: range [%d,%d) (log is empty)", raft.ErrNotFound, lo, hi)
	}
	first := m.entries[0].Index
	last := m.entries[len(m.entries)-1].Index
	if lo < first || hi > last+1 {
		return nil, fmt.Errorf("%w: range [%d,%d) not within [%d,%d]",
			raft.ErrNotFound, lo, hi, first, last)
	}

	src := m.entries[lo-first : hi-first]
	out := make([]raft.LogEntry, len(src))
	for i, e := range src {
		out[i] = cloneEntry(e)
	}
	return out, nil
}

func (m *MemStore) FirstIndex() (raft.Index, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.entries) == 0 {
		return 0, nil
	}
	return m.entries[0].Index, nil
}

func (m *MemStore) LastIndex() (raft.Index, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.entries) == 0 {
		return 0, nil
	}
	return m.entries[len(m.entries)-1].Index, nil
}

// TruncateSuffix deletes all entries with index >= fromIndex.
// It returns ErrCompacted if fromIndex precedes the first available entry,
// matching the file-backed backend.
func (m *MemStore) TruncateSuffix(_ context.Context, fromIndex raft.Index) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.entries) == 0 {
		return nil
	}
	first := m.entries[0].Index
	if fromIndex < first {
		return fmt.Errorf("%w: TruncateSuffix(%d) < firstIndex(%d)",
			raft.ErrCompacted, fromIndex, first)
	}
	if fromIndex == first {
		m.entries = nil
		return nil
	}
	last := m.entries[len(m.entries)-1].Index
	if fromIndex > last {
		return nil
	}
	// Re-slicing alone would leave the discarded entries reachable through the
	// backing array, so clip the slice and let them be collected.
	m.entries = slices.Clip(m.entries[:fromIndex-first])
	return nil
}

// TruncatePrefix deletes all entries with index < toIndex.
func (m *MemStore) TruncatePrefix(_ context.Context, toIndex raft.Index) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.entries) == 0 {
		return nil
	}
	first := m.entries[0].Index
	if toIndex <= first {
		return nil
	}
	last := m.entries[len(m.entries)-1].Index
	if toIndex > last {
		m.entries = nil
		return nil
	}
	// Copy the kept tail into a fresh slice. Re-slicing from the front would
	// keep every compacted entry (and its Command) alive for as long as the
	// store lives, which is exactly what compaction is supposed to release.
	m.entries = slices.Clone(m.entries[toIndex-first:])
	return nil
}

// --- Snapshot ---------------------------------------------------------------

func (m *MemStore) SaveSnapshot(_ context.Context, meta raft.SnapshotMeta, r io.Reader) error {
	// Drain the reader before taking the lock: it may be driven by the Raft
	// loop one chunk at a time, and holding the lock across it would stall the
	// very goroutine that has to supply the data.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return fmt.Errorf("memstore: read snapshot data: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapMeta = meta
	m.snapData = buf.Bytes()
	m.hasSnap = true
	return nil
}

func (m *MemStore) LoadSnapshot(_ context.Context) (raft.SnapshotMeta, io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.hasSnap {
		return raft.SnapshotMeta{}, nil, raft.ErrNoSnapshot
	}
	return m.snapMeta, io.NopCloser(bytes.NewReader(slices.Clone(m.snapData))), nil
}

// --- Lifecycle --------------------------------------------------------------

func (m *MemStore) Close() error { return nil }

// --- Internal helpers -------------------------------------------------------

// cloneEntry returns a copy of e that shares no memory with it.
func cloneEntry(e raft.LogEntry) raft.LogEntry {
	e.Command = slices.Clone(e.Command)
	return e
}

// Compile-time interface check.
var _ raft.Storage = (*MemStore)(nil)
