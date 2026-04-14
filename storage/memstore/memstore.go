// Package memstore provides an in-memory implementation of raft.Storage.
// It is intended for use in unit tests and simulations only; data is not
// persisted across process restarts.
package memstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/brunoga/raft"
)

// MemStore implements raft.Storage entirely in memory.
type MemStore struct {
	mu        sync.RWMutex
	hardState raft.HardState
	log       []raft.LogEntry // entries[i] is the entry at index (firstIndex+i)
	firstIdx  raft.Index      // index of log[0]; 0 means log is empty
	snapMeta  raft.SnapshotMeta
	snapData  []byte
	hasSnap   bool
}

// New returns an empty MemStore ready for use.
func New() *MemStore {
	return &MemStore{}
}

// --- Hard state -------------------------------------------------------------

func (m *MemStore) SaveHardState(_ context.Context, hs raft.HardState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hardState = hs
	return nil
}

func (m *MemStore) LoadHardState(_ context.Context) (raft.HardState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hardState, nil
}

// --- Log --------------------------------------------------------------------

func (m *MemStore) AppendLogEntries(_ context.Context, entries []raft.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.log) == 0 {
		// First entries ever (or after a full truncation).
		m.firstIdx = entries[0].Index
	}
	m.log = append(m.log, entries...)
	return nil
}

func (m *MemStore) GetLogEntry(_ context.Context, index raft.Index) (raft.LogEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getEntry(index)
}

func (m *MemStore) GetLogEntries(_ context.Context, lo, hi raft.Index) ([]raft.LogEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if lo >= hi {
		return nil, nil
	}
	if err := m.checkRange(lo, hi-1); err != nil {
		return nil, err
	}
	loOff := int(lo - m.firstIdx)
	hiOff := int(hi - m.firstIdx)
	result := make([]raft.LogEntry, hiOff-loOff)
	copy(result, m.log[loOff:hiOff])
	return result, nil
}

func (m *MemStore) FirstIndex(_ context.Context) (raft.Index, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.log) == 0 {
		return 0, nil
	}
	return m.firstIdx, nil
}

func (m *MemStore) LastIndex(_ context.Context) (raft.Index, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.log) == 0 {
		return 0, nil
	}
	return m.firstIdx + raft.Index(len(m.log)) - 1, nil
}

func (m *MemStore) TruncateSuffix(_ context.Context, fromIndex raft.Index) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.log) == 0 || fromIndex > m.lastIndex() {
		return nil
	}
	if fromIndex < m.firstIdx {
		return fmt.Errorf("%w: TruncateSuffix(%d) is before firstIndex(%d)",
			raft.ErrCompacted, fromIndex, m.firstIdx)
	}
	off := int(fromIndex - m.firstIdx)
	m.log = m.log[:off]
	if len(m.log) == 0 {
		m.firstIdx = 0
	}
	return nil
}

func (m *MemStore) TruncatePrefix(_ context.Context, toIndex raft.Index) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.log) == 0 || toIndex <= m.firstIdx {
		return nil
	}
	if toIndex > m.lastIndex()+1 {
		toIndex = m.lastIndex() + 1
	}
	off := int(toIndex - m.firstIdx)
	m.log = m.log[off:]
	if len(m.log) == 0 {
		m.firstIdx = 0
	} else {
		m.firstIdx = toIndex
	}
	return nil
}

// --- Snapshot ---------------------------------------------------------------

func (m *MemStore) SaveSnapshot(_ context.Context, meta raft.SnapshotMeta, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapMeta = meta
	m.snapData = make([]byte, len(data))
	copy(m.snapData, data)
	m.hasSnap = true
	return nil
}

func (m *MemStore) LoadSnapshot(_ context.Context) (raft.SnapshotMeta, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.hasSnap {
		return raft.SnapshotMeta{}, nil, raft.ErrNoSnapshot
	}
	out := make([]byte, len(m.snapData))
	copy(out, m.snapData)
	return m.snapMeta, out, nil
}

// --- Lifecycle --------------------------------------------------------------

func (m *MemStore) Close() error { return nil }

// --- Internal helpers -------------------------------------------------------

// lastIndex returns the last log index. Caller must hold mu.
func (m *MemStore) lastIndex() raft.Index {
	if len(m.log) == 0 {
		return 0
	}
	return m.firstIdx + raft.Index(len(m.log)) - 1
}

// getEntry returns the entry at index. Caller must hold mu (at least read).
func (m *MemStore) getEntry(index raft.Index) (raft.LogEntry, error) {
	if len(m.log) == 0 {
		return raft.LogEntry{}, raft.ErrNotFound
	}
	if err := m.checkRange(index, index); err != nil {
		return raft.LogEntry{}, err
	}
	return m.log[int(index-m.firstIdx)], nil
}

// checkRange verifies [lo, hi] is within the available log. Caller must hold mu.
func (m *MemStore) checkRange(lo, hi raft.Index) error {
	if lo < m.firstIdx {
		return fmt.Errorf("%w: index %d < firstIndex %d", raft.ErrCompacted, lo, m.firstIdx)
	}
	last := m.lastIndex()
	if hi > last {
		return fmt.Errorf("%w: index %d > lastIndex %d", raft.ErrNotFound, hi, last)
	}
	return nil
}
