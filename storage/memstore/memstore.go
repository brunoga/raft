package memstore

import (
	"bytes"
	"context"
	"io"
	"slices"
	"sync"

	"github.com/brunoga/raft"
)

// MemStore is a strictly in-memory implementation of raft.Storage.
// It is primarily intended for tests and simulations; all state is lost
// when the process exits.
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

func (m *MemStore) AppendLogEntries(_ context.Context, entries []raft.LogEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(entries) == 0 {
		return nil
	}

	m.entries = append(m.entries, entries...)
	return nil
}

func (m *MemStore) GetLogEntry(_ context.Context, index raft.Index) (raft.LogEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.entries) == 0 {
		return raft.LogEntry{}, raft.ErrNotFound
	}
	first := m.entries[0].Index
	last := m.entries[len(m.entries)-1].Index
	if index < first || index > last {
		return raft.LogEntry{}, raft.ErrNotFound
	}
	return m.entries[index-first], nil
}

func (m *MemStore) GetLogEntries(_ context.Context, lo, hi raft.Index) ([]raft.LogEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if lo >= hi {
		return nil, nil
	}
	if len(m.entries) == 0 {
		return nil, raft.ErrNotFound
	}
	first := m.entries[0].Index
	last := m.entries[len(m.entries)-1].Index
	if lo < first || hi > last+1 {
		return nil, raft.ErrNotFound
	}
	return slices.Clone(m.entries[lo-first : hi-first]), nil
}

func (m *MemStore) FirstIndex(_ context.Context) (raft.Index, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.entries) == 0 {
		return 0, nil
	}
	return m.entries[0].Index, nil
}

func (m *MemStore) LastIndex(_ context.Context) (raft.Index, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.entries) == 0 {
		return 0, nil
	}
	return m.entries[len(m.entries)-1].Index, nil
}

func (m *MemStore) TruncateSuffix(_ context.Context, fromIndex raft.Index) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) == 0 {
		return nil
	}
	first := m.entries[0].Index
	if fromIndex <= first {
		m.entries = nil
		return nil
	}
	m.entries = m.entries[:fromIndex-first]
	return nil
}

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
	m.entries = m.entries[toIndex-first:]
	return nil
}

// --- Snapshot ---------------------------------------------------------------

func (m *MemStore) SaveSnapshot(_ context.Context, meta raft.SnapshotMeta, r io.Reader) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return err
	}
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
	return m.snapMeta, io.NopCloser(bytes.NewReader(m.snapData)), nil
}

// --- Lifecycle --------------------------------------------------------------

func (m *MemStore) Close() error { return nil }

// --- Internal helpers -------------------------------------------------------

// Compile-time interface check.
var _ raft.Storage = (*MemStore)(nil)
