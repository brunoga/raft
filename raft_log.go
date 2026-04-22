package raft

import (
	"context"
	"fmt"
)

// raftLog is a thin in-memory overlay on top of Storage.  It caches the
// first/last indices and the term of the last entry so that the hot-path
// Raft logic (RequestVote, AppendEntries) avoids repeated storage reads.
//
// All methods are called from the Node's single event-loop goroutine;
// no additional locking is needed.
type raftLog struct {
	storage  Storage
	snapMeta SnapshotMeta // metadata of the last installed snapshot

	// snapClientTable holds the client dedup table loaded from the snapshot
	// during initialisation. It is consumed by Node.New() to seed n.clientTable
	// and then cleared.
	snapClientTable map[NodeID]clientEntry

	// cached positions; 0 means "no entries in storage"
	first    Index
	last     Index
	lastTerm Term // term of the entry at last (or snapMeta.LastIncludedTerm)

	// scratch is a single-element buffer reused by appendOne to avoid a heap
	// allocation per leader-propose call. Safe: raftLog is owned exclusively
	// by the event-loop goroutine.
	scratch [1]LogEntry
}

// newRaftLog initialises a raftLog by reading the current state of storage.
func newRaftLog(s Storage) (*raftLog, error) {
	rl := &raftLog{storage: s}
	ctx := context.Background()

	// Restore snapshot metadata (may not exist yet).
	meta, r, loadErr := s.LoadSnapshot(ctx)
	if loadErr == nil {
		defer func() { _ = r.Close() }()
		rl.snapMeta = meta
		// Read the framing header to extract the client dedup table.
		table, _, parseErr := readWrappedSnapshot(r)
		if parseErr != nil {
			return nil, fmt.Errorf("raftLog: read snapshot framing: %w", parseErr)
		}
		rl.snapClientTable = table
	} else if loadErr != ErrNoSnapshot {
		return nil, fmt.Errorf("raftLog: load snapshot: %w", loadErr)
	}

	first, err := s.FirstIndex()
	if err != nil {
		return nil, fmt.Errorf("raftLog: first index: %w", err)
	}
	last, err := s.LastIndex()
	if err != nil {
		return nil, fmt.Errorf("raftLog: last index: %w", err)
	}
	rl.first = first
	rl.last = last

	if last > 0 {
		e, err := s.GetLogEntry(ctx, last)
		if err != nil {
			return nil, fmt.Errorf("raftLog: read last entry: %w", err)
		}
		rl.lastTerm = e.Term
	} else {
		// No entries; last term comes from the snapshot (or 0).
		rl.lastTerm = rl.snapMeta.LastIncludedTerm
	}

	return rl, nil
}

// lastLogIndex returns the index of the most recent entry, falling back to the
// snapshot's last-included index when the log is empty.
func (rl *raftLog) lastLogIndex() Index {
	if rl.last > 0 {
		return rl.last
	}
	return rl.snapMeta.LastIncludedIndex
}

// lastLogTerm returns the term of the most recent entry (or snapshot).
func (rl *raftLog) lastLogTerm() Term {
	return rl.lastTerm
}

// termAt returns the term of the entry at index, consulting the snapshot when
// the entry has been compacted.
func (rl *raftLog) termAt(ctx context.Context, index Index) (Term, error) {
	if index == 0 {
		return 0, nil
	}
	if index == rl.snapMeta.LastIncludedIndex {
		return rl.snapMeta.LastIncludedTerm, nil
	}
	e, err := rl.storage.GetLogEntry(ctx, index)
	if err != nil {
		return 0, err
	}
	return e.Term, nil
}

// appendOne appends a single entry without allocating a temporary slice.
// It reuses a scratch buffer that is valid only until the next appendOne call.
func (rl *raftLog) appendOne(ctx context.Context, entry LogEntry) error {
	rl.scratch[0] = entry
	return rl.append(ctx, rl.scratch[:])
}

// append appends entries to the log, updating the cached last/lastTerm.
func (rl *raftLog) append(ctx context.Context, entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := rl.storage.AppendLogEntries(ctx, entries); err != nil {
		return err
	}
	last := entries[len(entries)-1]
	if rl.first == 0 {
		rl.first = entries[0].Index
	}
	rl.last = last.Index
	rl.lastTerm = last.Term
	return nil
}

// truncateSuffix removes entries at index >= fromIndex and updates the cache.
func (rl *raftLog) truncateSuffix(ctx context.Context, fromIndex Index) error {
	if err := rl.storage.TruncateSuffix(ctx, fromIndex); err != nil {
		return err
	}
	if fromIndex <= rl.first {
		rl.first = 0
		rl.last = 0
		rl.lastTerm = rl.snapMeta.LastIncludedTerm
		return nil
	}
	rl.last = fromIndex - 1
	if rl.last > 0 {
		t, err := rl.storage.GetLogEntry(ctx, rl.last)
		if err != nil {
			return err
		}
		rl.lastTerm = t.Term
	} else {
		rl.lastTerm = rl.snapMeta.LastIncludedTerm
	}
	return nil
}

// truncatePrefix removes entries at index < toIndex and updates the cache.
func (rl *raftLog) truncatePrefix(ctx context.Context, toIndex Index) error {
	if err := rl.storage.TruncatePrefix(ctx, toIndex); err != nil {
		return err
	}
	if toIndex > rl.last {
		rl.first = 0
		rl.last = 0
		rl.lastTerm = rl.snapMeta.LastIncludedTerm
	} else {
		rl.first = toIndex
	}
	return nil
}

// isUpToDate reports whether a candidate with (candidateLastIndex,
// candidateLastTerm) has a log at least as up-to-date as ours (§5.4.1).
func (rl *raftLog) isUpToDate(candidateLastIndex Index, candidateLastTerm Term) bool {
	ourTerm := rl.lastLogTerm()
	ourIndex := rl.lastLogIndex()
	if candidateLastTerm != ourTerm {
		return candidateLastTerm > ourTerm
	}
	return candidateLastIndex >= ourIndex
}
