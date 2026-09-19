package raft

import (
	"context"
	"fmt"
)

// raftLog is a thin in-memory overlay on top of Storage. It caches the
// first/last indices and the term of the last entry so that the hot-path
// Raft logic (RequestVote, AppendEntries) avoids repeated storage reads.
//
// It also holds the entries that have been appended to the log but are not
// yet known to be on stable storage. Every mutating storage call is handed to
// a [storageWriter] and completes later, so between the append and its
// completion the only copy of an entry is the one here. Reads consult those
// entries first, which is what lets the rest of the implementation treat an
// entry as being in the log the moment it is appended -- replicating it,
// counting it towards the up-to-date check that gates a vote -- while
// everything that would be a promise about durability waits for the write.
//
// The one index that answers "what is really on disk" is stableIndex. Nothing
// may be acknowledged to a leader, counted towards a commit quorum, or handed
// to the apply loop above it.
//
// All methods are called from the Node's single event-loop goroutine;
// no additional locking is needed.
type raftLog struct {
	storage  Storage
	w        *storageWriter
	snapMeta SnapshotMeta // metadata of the last installed snapshot

	// snapClientTable holds the client dedup table loaded from the snapshot
	// during initialisation, in eviction order. It is consumed by Node.New() to
	// seed n.clientTable and then cleared.
	snapClientTable []clientRecord

	// snapMembership is the cluster membership recorded in that snapshot, and
	// hasSnapMembership reports whether the snapshot carried one at all —
	// snapshots written before the membership section existed do not, and an
	// empty membership must not be mistaken for an empty cluster. Both are
	// consumed by Node.New() and then cleared.
	snapMembership    membershipState
	hasSnapMembership bool

	// cached positions; 0 means "no entries in storage"
	first    Index
	last     Index
	lastTerm Term // term of the entry at last (or snapMeta.LastIncludedTerm)

	// segs holds entries that are in the log but not yet known to be on stable
	// storage, oldest first. One segment per queued append, tagged with the
	// sequence number of the write that will persist it.
	//
	// Segments are contiguous: each begins exactly where the previous one
	// ends, and the first begins one past the highest index that is both
	// durable and current. A segment is dropped when its write completes, and
	// trimmed or dropped when a truncation removes the entries it holds.
	segs []unstableSeg
	// unstableBytes is the total size of the commands held in segs. It is what
	// a leader throttles its own proposals against: entries accepted faster
	// than the disk retires them have nowhere to live but memory.
	unstableBytes int

	// durable is the highest log index known to be on stable storage as of the
	// most recent completed write. It is not by itself a statement about the
	// current log: a queued truncation can leave storage holding entries this
	// log has already replaced. stableIndex reconciles the two.
	durable Index
	// queuedDurable is what durable will be once every operation queued so far
	// has completed. Each operation records it as durableAfter, which is the
	// only moment at which the log and the queue agree on what the log is.
	queuedDurable Index
	// nextSeq is the sequence number handed to the next queued operation.
	nextSeq uint64
}

// unstableSeg is one queued append: the entries it carried, and the sequence
// number of the write that will put them on stable storage.
type unstableSeg struct {
	seq     uint64
	entries []LogEntry
}

// newRaftLog initialises a raftLog by reading the current state of storage.
// Every mutating call it makes is handed to w.
func newRaftLog(s Storage, w *storageWriter) (*raftLog, error) {
	rl := &raftLog{storage: s, w: w}
	ctx := context.Background()

	// Restore snapshot metadata (may not exist yet).
	meta, r, loadErr := s.LoadSnapshot(ctx)
	if loadErr == nil {
		defer func() { _ = r.Close() }()
		rl.snapMeta = meta
		// Read the framing header to extract the client dedup table.
		table, ms, hasMS, _, parseErr := readWrappedSnapshot(r)
		if parseErr != nil {
			return nil, fmt.Errorf("raftLog: read snapshot framing: %w", parseErr)
		}
		rl.snapClientTable = table
		rl.snapMembership = ms
		rl.hasSnapMembership = hasMS
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
	// Everything that survived a restart is by definition durable.
	rl.durable = last
	rl.queuedDurable = last

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

// stableIndex is the highest index for which storage holds the entry this log
// currently has at that index.
//
// It is deliberately the conjunction of three separate limits, because each
// one on its own can be wrong:
//
//   - durable alone overstates. A queued truncation has already taken effect
//     in memory while storage still holds the entries it will remove, so the
//     completion of an earlier append can report a durable index that covers
//     entries this log has since replaced.
//   - the snapshot boundary raises it. Entries below it are gone from the log
//     but their effect is in a snapshot that was made durable before it was
//     installed, so the apply loop can be told about them.
//   - the start of the unstable region caps it. Those entries are in memory
//     only; whatever storage holds at those indices belongs to a history this
//     node has already abandoned.
//
// Every promise that depends on the disk is bounded by this: the
// acknowledgement a follower sends its leader, the leader counting its own log
// towards a commit quorum, and the commit index handed to the apply loop,
// which reads entries back from storage.
func (rl *raftLog) stableIndex() Index {
	d := rl.durable
	if snap := rl.snapMeta.LastIncludedIndex; snap > d {
		d = snap
	}
	if us := rl.unstableFirst(); us != 0 && d >= us {
		d = us - 1
	}
	if last := rl.lastLogIndex(); d > last {
		d = last
	}
	return d
}

// unstableFirst returns the index of the first entry that is not yet known to
// be on stable storage, or 0 when there is none.
func (rl *raftLog) unstableFirst() Index {
	for i := range rl.segs {
		if len(rl.segs[i].entries) > 0 {
			return rl.segs[i].entries[0].Index
		}
	}
	return 0
}

// unstableSize returns the total size of the commands held in memory pending a
// write.
func (rl *raftLog) unstableSize() int { return rl.unstableBytes }

// unstableAt returns the entry at index if it is still only in memory.
func (rl *raftLog) unstableAt(index Index) (LogEntry, bool) {
	for i := range rl.segs {
		es := rl.segs[i].entries
		if len(es) == 0 {
			continue
		}
		if index < es[0].Index {
			return LogEntry{}, false // segments are ordered; it is not here
		}
		if index <= es[len(es)-1].Index {
			return es[index-es[0].Index], true
		}
	}
	return LogEntry{}, false
}

// unstableSlice returns the entries in [lo, hi) that are still only in memory.
// The caller must have established that the whole range is unstable.
func (rl *raftLog) unstableSlice(lo, hi Index) []LogEntry {
	out := make([]LogEntry, 0, hi-lo)
	for i := range rl.segs {
		es := rl.segs[i].entries
		if len(es) == 0 || es[len(es)-1].Index < lo {
			continue
		}
		if es[0].Index >= hi {
			break
		}
		from := lo
		if es[0].Index > from {
			from = es[0].Index
		}
		to := hi
		if end := es[len(es)-1].Index + 1; end < to {
			to = end
		}
		out = append(out, es[from-es[0].Index:to-es[0].Index]...)
	}
	return out
}

// dropUnstableFrom removes in-memory entries at index >= from.
func (rl *raftLog) dropUnstableFrom(from Index) {
	for i := len(rl.segs) - 1; i >= 0; i-- {
		es := rl.segs[i].entries
		if len(es) == 0 || es[0].Index >= from {
			rl.unstableBytes -= commandBytes(es)
			rl.segs = rl.segs[:i]
			continue
		}
		if last := es[len(es)-1].Index; last >= from {
			keep := from - es[0].Index
			rl.unstableBytes -= commandBytes(es[keep:])
			rl.segs[i].entries = es[:keep]
		}
		break
	}
}

// dropUnstableBelow removes in-memory entries at index < upto. It exists for
// compaction, which can in principle run past entries that have not been
// written yet.
func (rl *raftLog) dropUnstableBelow(upto Index) {
	for len(rl.segs) > 0 {
		es := rl.segs[0].entries
		if len(es) == 0 {
			rl.segs = rl.segs[1:]
			continue
		}
		if es[len(es)-1].Index < upto {
			rl.unstableBytes -= commandBytes(es)
			rl.segs = rl.segs[1:]
			continue
		}
		if es[0].Index < upto {
			drop := upto - es[0].Index
			rl.unstableBytes -= commandBytes(es[:drop])
			rl.segs[0].entries = es[drop:]
		}
		return
	}
}

func commandBytes(entries []LogEntry) int {
	total := 0
	for i := range entries {
		total += len(entries[i].Command)
	}
	return total
}

// stabilize records a completed write. Segments whose write has landed are
// released, and the durable point moves to where that operation left it.
func (rl *raftLog) stabilize(d writeDone) {
	if d.err != nil {
		// The log's contents are now unknown, so nothing may be released and
		// nothing new may be claimed as durable. The node is being stopped.
		return
	}
	rl.durable = d.durableAfter
	for len(rl.segs) > 0 && rl.segs[0].seq <= d.seq {
		rl.unstableBytes -= commandBytes(rl.segs[0].entries)
		rl.segs = rl.segs[1:]
	}
	if len(rl.segs) == 0 {
		rl.segs = nil // release the backing array
	}
}

// nextWriteSeq allocates the sequence number for the next queued operation.
func (rl *raftLog) nextWriteSeq() uint64 {
	rl.nextSeq++
	return rl.nextSeq
}

// appendOne appends a single entry.
func (rl *raftLog) appendOne(entry LogEntry) uint64 {
	return rl.append([]LogEntry{entry})
}

// append adds entries to the log and queues the write that will make them
// durable. It returns the sequence number of that write, which the caller can
// use to defer anything that must not happen until the entries are on disk.
//
// The entries are copied: the caller's slice is usually a window into an
// inbound RPC, and the copy has to outlive it.
func (rl *raftLog) append(entries []LogEntry) uint64 {
	if len(entries) == 0 {
		return 0
	}
	buf := make([]LogEntry, len(entries))
	copy(buf, entries)

	seq := rl.nextWriteSeq()
	rl.segs = append(rl.segs, unstableSeg{seq: seq, entries: buf})
	rl.unstableBytes += commandBytes(buf)

	last := buf[len(buf)-1]
	if rl.first == 0 {
		rl.first = buf[0].Index
	}
	rl.last = last.Index
	rl.lastTerm = last.Term
	rl.queuedDurable = last.Index

	_ = rl.w.enqueue(&writeOp{
		seq:          seq,
		kind:         writeAppend,
		entries:      buf,
		durableAfter: rl.queuedDurable,
	})
	return seq
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
	if e, ok := rl.unstableAt(index); ok {
		return e.Term, nil
	}
	// Outside the range this log describes, storage may still hold an entry:
	// a queued truncation has already taken effect here and not yet there.
	// Reading it would resurrect an entry the log has discarded.
	if rl.first == 0 || index < rl.first || index > rl.last {
		return 0, fmt.Errorf("raft: log entry %d is not in the log", index)
	}
	e, err := rl.storage.GetLogEntry(ctx, index)
	if err != nil {
		return 0, err
	}
	return e.Term, nil
}

// entries returns the log entries in [lo, hi), reading from storage and from
// the in-memory region as needed. hi is clamped to one past the last entry.
func (rl *raftLog) entries(ctx context.Context, lo, hi Index) ([]LogEntry, error) {
	if hi > rl.last+1 {
		hi = rl.last + 1
	}
	if lo >= hi {
		return nil, nil
	}
	if rl.first == 0 || lo < rl.first {
		return nil, fmt.Errorf("raft: log entries [%d,%d) are not in the log", lo, hi)
	}

	us := rl.unstableFirst()
	if us != 0 && lo >= us {
		return rl.unstableSlice(lo, hi), nil
	}

	storageHi := hi
	if us != 0 && us < storageHi {
		storageHi = us
	}
	out, err := rl.storage.GetLogEntries(ctx, lo, storageHi)
	if err != nil {
		return nil, err
	}
	if storageHi < hi {
		out = append(out, rl.unstableSlice(storageHi, hi)...)
	}
	return out, nil
}

// truncateSuffix removes entries at index >= fromIndex and queues the write.
func (rl *raftLog) truncateSuffix(ctx context.Context, fromIndex Index) error {
	// The term of the new last entry has to be read before anything is
	// discarded, while the log can still describe it.
	newLast := fromIndex - 1
	newLastTerm := rl.snapMeta.LastIncludedTerm
	if fromIndex > rl.first && newLast > 0 {
		t, err := rl.termAt(ctx, newLast)
		if err != nil {
			return err
		}
		newLastTerm = t
	}

	rl.dropUnstableFrom(fromIndex)
	if fromIndex > 0 && fromIndex-1 < rl.queuedDurable {
		rl.queuedDurable = fromIndex - 1
	}
	_ = rl.w.enqueue(&writeOp{
		seq:          rl.nextWriteSeq(),
		kind:         writeTruncateSuffix,
		index:        fromIndex,
		durableAfter: rl.queuedDurable,
	})

	if fromIndex <= rl.first {
		rl.first = 0
		rl.last = 0
		rl.lastTerm = rl.snapMeta.LastIncludedTerm
		return nil
	}
	rl.last = newLast
	rl.lastTerm = newLastTerm
	return nil
}

// canDescribe reports whether this log can describe the entry at index: either
// it is still present, or it is the snapshot boundary, or it is the empty
// position before the first entry. Replication needs this for the entry before
// the one it is about to send; when the answer is no, the peer has fallen
// behind what the log still holds and needs a snapshot instead.
func (rl *raftLog) canDescribe(index Index) bool {
	if index == 0 || index == rl.snapMeta.LastIncludedIndex {
		return true
	}
	return rl.first != 0 && index >= rl.first && index <= rl.last
}

// truncatePrefix removes entries at index < toIndex and queues the write.
func (rl *raftLog) truncatePrefix(toIndex Index) {
	if rl.first != 0 && toIndex <= rl.first {
		return // nothing to reclaim
	}
	rl.dropUnstableBelow(toIndex)
	if toIndex > rl.last && toIndex > 0 && toIndex-1 < rl.queuedDurable {
		rl.queuedDurable = toIndex - 1
	}
	_ = rl.w.enqueue(&writeOp{
		seq:          rl.nextWriteSeq(),
		kind:         writeTruncatePrefix,
		index:        toIndex,
		durableAfter: rl.queuedDurable,
	})

	if toIndex > rl.last {
		rl.first = 0
		rl.last = 0
		rl.lastTerm = rl.snapMeta.LastIncludedTerm
	} else {
		rl.first = toIndex
	}
}

// installSnapshot makes meta this log's new base, keeping only the entries that
// belong to the same history as the snapshot.
//
// Raft section 7: entries after the snapshot point may be retained only when
// the log agrees with the snapshot at that point. If the entry there has a
// different term, everything from the snapshot point onwards belongs to a
// history the cluster abandoned, and keeping it would leave a log that starts
// with the leader's state and continues with somebody else's.
func (rl *raftLog) installSnapshot(ctx context.Context, meta SnapshotMeta) error {
	agrees := false
	if t, err := rl.termAt(ctx, meta.LastIncludedIndex); err == nil && t == meta.LastIncludedTerm {
		agrees = true
	}

	switch {
	case agrees:
		rl.truncatePrefix(meta.LastIncludedIndex + 1)
	case rl.first != 0:
		if err := rl.truncateSuffix(ctx, rl.first); err != nil {
			return err
		}
	}

	rl.snapMeta = meta
	if rl.last == 0 {
		// Nothing survived: the snapshot boundary is now the end of the log.
		rl.first = 0
		rl.lastTerm = meta.LastIncludedTerm
	}
	return nil
}

// isUpToDate reports whether a candidate with (candidateLastIndex,
// candidateLastTerm) has a log at least as up-to-date as ours (§5.4.1).
//
// The comparison is against the log as it stands in memory, entries still
// waiting on a write included. That is what makes deferring the write safe:
// an entry counts against a candidate from the moment this node accepts it,
// and a node that crashes before the write lands comes back without the entry
// and without ever having acknowledged it.
func (rl *raftLog) isUpToDate(candidateLastIndex Index, candidateLastTerm Term) bool {
	ourTerm := rl.lastLogTerm()
	ourIndex := rl.lastLogIndex()
	if candidateLastTerm != ourTerm {
		return candidateLastTerm > ourTerm
	}
	return candidateLastIndex >= ourIndex
}
