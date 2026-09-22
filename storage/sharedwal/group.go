package sharedwal

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/brunoga/raft/v2"
)

// GroupStore is one group's view of the shared log. It implements
// raft.Storage, raft.BatchWriter and raft.CommitRecorder.
//
// Its methods are safe for the concurrency raft.Storage requires: the engine
// issues log and hard-state operations one at a time per group, and snapshot
// operations may overlap them. Different groups' stores may be used from any
// goroutines at once; that is the point.
type GroupStore struct {
	w  *WAL
	id uint64
}

// GroupID returns the group this store serves.
func (s *GroupStore) GroupID() uint64 { return s.id }

// Close is a no-op. The log is shared, and closing it is WAL.Close's job.
func (s *GroupStore) Close() error { return nil }

var (
	_ raft.Storage        = (*GroupStore)(nil)
	_ raft.BatchWriter    = (*GroupStore)(nil)
	_ raft.CommitRecorder = (*GroupStore)(nil)
)

// ---- Hard state ---------------------------------------------------------------

// SaveHardState implements raft.Storage.
func (s *GroupStore) SaveHardState(_ context.Context, hs raft.HardState) error {
	return s.SaveState(context.Background(), &hs, nil)
}

// LoadHardState implements raft.Storage.
func (s *GroupStore) LoadHardState(_ context.Context) (raft.HardState, error) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	if s.w.closed {
		return raft.HardState{}, ErrClosed
	}
	if g, ok := s.w.groups[s.id]; ok {
		return g.hs, nil
	}
	return raft.HardState{}, nil
}

// SaveState implements raft.BatchWriter: the hard state and the entries go
// into the log as consecutive records and are synced together, so the
// engine's pairing of a term with the entries it received in that term costs
// one sync rather than two.
func (s *GroupStore) SaveState(_ context.Context, hs *raft.HardState, entries []raft.LogEntry) error {
	if hs == nil && len(entries) == 0 {
		return nil
	}
	if err := checkContiguous(entries); err != nil {
		return err
	}
	var buf []byte
	if hs != nil {
		buf = appendRecord(buf, s.id, kindHardState, encodeHardStateBody(*hs))
	}
	entriesAt := int64(-1)
	var offsets []int64
	if len(entries) > 0 {
		body, offs := encodeEntriesBody(entries)
		entriesAt = int64(len(buf))
		offsets = offs
		buf = appendRecord(buf, s.id, kindEntries, body)
	}
	// The entries slice is the caller's; only the shape is kept.
	shape := make([]raft.LogEntry, len(entries))
	for i := range entries {
		shape[i] = raft.LogEntry{Index: entries[i].Index, Term: entries[i].Term}
	}
	bodyLen := 0
	if entriesAt >= 0 {
		bodyLen = len(buf) - int(entriesAt) - recordHeaderSize
	}
	var hsCopy *raft.HardState
	if hs != nil {
		c := *hs
		hsCopy = &c
	}
	return s.w.submit(&writeRequest{
		records: buf,
		apply: func(seg *segment, base int64) {
			g := s.w.group(s.id)
			if hsCopy != nil {
				s.w.installHardState(g, seg, *hsCopy)
			}
			if entriesAt >= 0 {
				s.w.installEntries(g, seg, base+entriesAt+recordHeaderSize, shape, offsets, bodyLen)
			}
		},
	}, true)
}

// checkContiguous rejects a run of entries whose indices do not follow one
// another, which the engine never sends and which the in-memory index could
// not describe.
func checkContiguous(entries []raft.LogEntry) error {
	for i := 1; i < len(entries); i++ {
		if entries[i].Index != entries[i-1].Index+1 {
			return fmt.Errorf("sharedwal: entries are not contiguous at %d", entries[i].Index)
		}
	}
	return nil
}

// ---- Log ----------------------------------------------------------------------

// AppendLogEntries implements raft.Storage.
func (s *GroupStore) AppendLogEntries(_ context.Context, entries []raft.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.SaveState(context.Background(), nil, entries)
}

// FirstIndex implements raft.Storage.
func (s *GroupStore) FirstIndex() (raft.Index, error) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	if s.w.closed {
		return 0, ErrClosed
	}
	if g, ok := s.w.groups[s.id]; ok && len(g.locs) > 0 {
		return g.first, nil
	}
	return 0, nil
}

// LastIndex implements raft.Storage.
func (s *GroupStore) LastIndex() (raft.Index, error) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	if s.w.closed {
		return 0, ErrClosed
	}
	if g, ok := s.w.groups[s.id]; ok {
		return g.last(), nil
	}
	return 0, nil
}

// GetLogEntry implements raft.Storage.
func (s *GroupStore) GetLogEntry(_ context.Context, index raft.Index) (raft.LogEntry, error) {
	entries, err := s.readRange(index, index+1)
	if err != nil {
		return raft.LogEntry{}, err
	}
	return entries[0], nil
}

// GetLogEntries implements raft.Storage.
func (s *GroupStore) GetLogEntries(_ context.Context, lo, hi raft.Index) ([]raft.LogEntry, error) {
	if lo >= hi {
		return nil, nil
	}
	return s.readRange(lo, hi)
}

// readRange reads entries [lo, hi). Contiguous entries in one segment -- a
// run written in one record -- are read with a single positioned read.
func (s *GroupStore) readRange(lo, hi raft.Index) ([]raft.LogEntry, error) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	if s.w.closed {
		return nil, ErrClosed
	}
	g, ok := s.w.groups[s.id]
	if !ok || len(g.locs) == 0 || lo < g.first || hi-1 > g.last() {
		return nil, raft.ErrNotFound
	}
	out := make([]raft.LogEntry, 0, hi-lo)
	i := int(lo - g.first)
	end := int(hi - g.first)
	for i < end {
		// Extend the run while the next entry follows on disk.
		j := i + 1
		size := int64(g.locs[i].size)
		for j < end && g.locs[j].seg == g.locs[i].seg && g.locs[j].off == g.locs[i].off+size {
			size += int64(g.locs[j].size)
			j++
		}
		buf := make([]byte, size)
		if _, err := g.locs[i].seg.f.ReadAt(buf, g.locs[i].off); err != nil {
			return nil, fmt.Errorf("sharedwal: read entries: %w", err)
		}
		off := 0
		for k := i; k < j; k++ {
			e, n, err := decodeEntryAt(buf, off)
			if err != nil {
				return nil, fmt.Errorf("sharedwal: entry %d: %w", g.first+raft.Index(k), err)
			}
			if e.Index != g.first+raft.Index(k) {
				return nil, fmt.Errorf("sharedwal: entry at index %d reads back as %d",
					g.first+raft.Index(k), e.Index)
			}
			out = append(out, e)
			off += n
		}
		i = j
	}
	return out, nil
}

// TruncateSuffix implements raft.Storage.
func (s *GroupStore) TruncateSuffix(_ context.Context, fromIndex raft.Index) error {
	return s.w.submit(&writeRequest{
		records: appendRecord(nil, s.id, kindTruncateSuffix, encodeIndexBody(fromIndex)),
		apply: func(*segment, int64) {
			s.w.dropSuffix(s.w.group(s.id), fromIndex)
		},
	}, true)
}

// TruncatePrefix implements raft.Storage. It is the operation that frees
// space, so it is followed by a reclaim pass over the segments.
func (s *GroupStore) TruncatePrefix(_ context.Context, toIndex raft.Index) error {
	err := s.w.submit(&writeRequest{
		records: appendRecord(nil, s.id, kindTruncatePrefix, encodeIndexBody(toIndex)),
		apply: func(*segment, int64) {
			s.w.dropPrefix(s.w.group(s.id), toIndex)
		},
	}, true)
	if err != nil {
		return err
	}
	// Best effort: a reclaim failure leaks space, and failing the truncation
	// for it would stop the node over a space problem. See WAL.Reclaim.
	_ = s.w.maybeReclaim()
	return nil
}

// ---- Commit index -------------------------------------------------------------

// SaveCommitIndex implements raft.CommitRecorder. The record is queued and
// written with the next batch; the caller does not wait for it, since the
// contract asks for no sync and losing the newest value costs nothing but a
// wider recovery band.
func (s *GroupStore) SaveCommitIndex(_ context.Context, index raft.Index) error {
	return s.w.submit(&writeRequest{
		records: appendRecord(nil, s.id, kindCommitIndex, encodeIndexBody(index)),
		apply: func(seg *segment, _ int64) {
			s.w.installCommit(s.w.group(s.id), seg, index)
		},
	}, false)
}

// LoadCommitIndex implements raft.CommitRecorder.
func (s *GroupStore) LoadCommitIndex(_ context.Context) (raft.Index, error) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	if s.w.closed {
		return 0, ErrClosed
	}
	if g, ok := s.w.groups[s.id]; ok {
		return g.commit, nil
	}
	return 0, nil
}

// ---- Snapshot -----------------------------------------------------------------

// SaveSnapshot implements raft.Storage. The data goes to a file of its own,
// synced and renamed into place, and only then does a record in the log make
// it the group's snapshot; the previous snapshot file is removed after that
// record is durable.
func (s *GroupStore) SaveSnapshot(_ context.Context, meta raft.SnapshotMeta, r io.Reader) error {
	s.w.mu.Lock()
	closed := s.w.closed
	s.w.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if err := writeSnapshotFile(s.w.dir, s.id, meta, r); err != nil {
		return err
	}
	var previous *raft.SnapshotMeta
	err := s.w.submit(&writeRequest{
		records: appendRecord(nil, s.id, kindSnapshot, encodeSnapshotBody(meta)),
		apply: func(seg *segment, _ int64) {
			g := s.w.group(s.id)
			if g.hasSnap && g.snap != meta {
				prev := g.snap
				previous = &prev
			}
			s.w.installSnapshot(g, seg, meta)
		},
	}, true)
	if err != nil {
		return err
	}
	if previous != nil {
		if rerr := removeSnapshotFile(s.w.dir, s.id, *previous); rerr != nil {
			return rerr
		}
	}
	return nil
}

// LoadSnapshot implements raft.Storage.
func (s *GroupStore) LoadSnapshot(_ context.Context) (raft.SnapshotMeta, io.ReadCloser, error) {
	s.w.mu.Lock()
	if s.w.closed {
		s.w.mu.Unlock()
		return raft.SnapshotMeta{}, nil, ErrClosed
	}
	g, ok := s.w.groups[s.id]
	var meta raft.SnapshotMeta
	has := ok && g.hasSnap
	if has {
		meta = g.snap
	}
	s.w.mu.Unlock()
	if !has {
		return raft.SnapshotMeta{}, nil, raft.ErrNoSnapshot
	}
	rc, err := openSnapshotFile(s.w.dir, s.id, meta)
	if err != nil {
		return raft.SnapshotMeta{}, nil, err
	}
	return meta, rc, nil
}

// ---- Removal ------------------------------------------------------------------

// Remove forgets everything the log holds for a group, durably, so that the
// space it used can be reclaimed. It is for a group that has been
// decommissioned on this host. The group's store must not be in use.
func (w *WAL) Remove(groupID uint64) error {
	w.mu.Lock()
	g, ok := w.groups[groupID]
	var snap *raft.SnapshotMeta
	if ok && g.hasSnap {
		s := g.snap
		snap = &s
	}
	w.mu.Unlock()
	if !ok {
		return nil
	}
	err := w.submit(&writeRequest{
		records: appendRecord(nil, groupID, kindRemoveGroup, nil),
		apply: func(*segment, int64) {
			if g, ok := w.groups[groupID]; ok {
				w.dropGroup(g)
			}
		},
	}, true)
	if err != nil {
		return err
	}
	var errs []error
	if snap != nil {
		errs = append(errs, removeSnapshotFile(w.dir, groupID, *snap))
	}
	errs = append(errs, w.maybeReclaim())
	return errors.Join(errs...)
}
