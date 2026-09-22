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

	// snapClientTableCap is the client table bound recorded in that
	// snapshot, and hasSnapClientTableCap whether one was recorded.
	snapClientTableCap    int
	hasSnapClientTableCap bool

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

	// runs records where each term begins, oldest first. Terms never decrease
	// with index, so the log is a short sequence of runs -- one per leadership
	// epoch -- and that is the whole of what the consensus logic ever needs to
	// know about an entry it is not about to send.
	//
	// It exists because the alternative was a storage read. Every heartbeat
	// looks up the term of the entry before the one it would send, every
	// inbound append checks the term at the index it starts from, and both
	// sides of a log conflict used to walk backwards an index at a time
	// reading each entry. Those reads sit on the event loop, and in a
	// file-backed store they contend with the same lock the writer holds
	// across its fsync, so the loop could still end up waiting for a disk it
	// no longer writes to.
	runs []termRun

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

// termRun records that the entry at start, and every entry after it up to the
// start of the next run, carries this term.
type termRun struct {
	start Index
	term  Term
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
		frame, _, parseErr := readSnapshotFrame(r)
		if parseErr != nil {
			return nil, fmt.Errorf("raftLog: read snapshot framing: %w", parseErr)
		}
		rl.snapClientTable = frame.table
		rl.snapMembership = frame.membership
		rl.hasSnapMembership = frame.hasMembership
		rl.snapClientTableCap = frame.clientTableCap
		rl.hasSnapClientTableCap = frame.hasClientTableCap
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
		if err := rl.loadRuns(ctx); err != nil {
			return nil, err
		}
		rl.lastTerm = rl.runs[len(rl.runs)-1].term
	} else {
		// No entries; last term comes from the snapshot (or 0).
		rl.lastTerm = rl.snapMeta.LastIncludedTerm
	}

	return rl, nil
}

// loadRuns rebuilds the run index by reading the log once.
//
// This is the only place that reads every entry, and it happens while the node
// is being constructed rather than while it is serving, which is the whole
// point: paying for the scan once at startup is what makes every later term
// lookup free. Node.New already reads the same entries a second time to
// recover the cluster membership, so the log length a node can start with is
// unchanged by this.
func (rl *raftLog) loadRuns(ctx context.Context) error {
	const batch = 1024
	for lo := rl.first; lo <= rl.last; lo += batch {
		hi := min(lo+batch, rl.last+1)
		entries, err := rl.storage.GetLogEntries(ctx, lo, hi)
		if err != nil {
			return fmt.Errorf("raftLog: read terms [%d,%d): %w", lo, hi, err)
		}
		if len(entries) == 0 {
			return fmt.Errorf("raftLog: read terms [%d,%d): no entries returned", lo, hi)
		}
		rl.extendRuns(entries)
	}
	if len(rl.runs) == 0 {
		return fmt.Errorf("raftLog: log holds entries up to %d but no terms were read", rl.last)
	}
	return nil
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

// writeSeqCovering returns the sequence number of the queued write that will
// put the entry at index on stable storage, or 0 when it is already there.
//
// It exists for the request that needs no append at all. A leader that has not
// yet been acknowledged re-sends the same entries, and a follower still
// holding them unwritten finds every one of them already in its log and
// queues nothing -- so the write it must wait for is one queued by an earlier
// request, not by this one. Answering such a request immediately would tell
// the leader those entries were durable when nothing had reached the disk.
func (rl *raftLog) writeSeqCovering(index Index) uint64 {
	if index == 0 || index <= rl.stableIndex() {
		return 0
	}
	for i := range rl.segs {
		es := rl.segs[i].entries
		if len(es) == 0 {
			continue
		}
		if index >= es[0].Index && index <= es[len(es)-1].Index {
			return rl.segs[i].seq
		}
	}
	// Above the durable point but in no segment: a queued truncation has moved
	// the durable point without an append to carry it. Wait for everything
	// queued so far, which is never wrong and only ever too patient.
	return rl.nextSeq
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
	rl.extendRuns(buf)
	rl.last = last.Index
	rl.lastTerm = last.Term
	rl.queuedDurable = last.Index

	rl.w.enqueue(&writeOp{
		seq:          seq,
		kind:         writeAppend,
		entries:      buf,
		durableAfter: rl.queuedDurable,
	})
	return seq
}

// termAt returns the term of the entry at index, consulting the snapshot when
// the entry has been compacted.
//
// It never reads storage. The run index holds every term in the log in a
// handful of entries, one per leadership epoch, however long the log is; that
// is what lets the hot paths calling it stay on the event loop.
func (rl *raftLog) termAt(index Index) (Term, error) {
	if index == 0 {
		return 0, nil
	}
	if index == rl.snapMeta.LastIncludedIndex {
		return rl.snapMeta.LastIncludedTerm, nil
	}
	// Outside the range this log describes there is no answer to give. Storage
	// may still hold an entry there -- a queued truncation has already taken
	// effect here and not yet on disk -- and reporting its term would
	// resurrect an entry the log has discarded.
	if rl.first == 0 || index < rl.first || index > rl.last {
		return 0, fmt.Errorf("raft: log entry %d is not in the log", index)
	}
	i := rl.runIndexFor(index)
	if i < 0 {
		return 0, fmt.Errorf("raft: log entry %d is not in the log", index)
	}
	return rl.runs[i].term, nil
}

// runIndexFor returns the position in runs of the run covering index, or -1
// when no run does.
func (rl *raftLog) runIndexFor(index Index) int {
	// The last run whose start is at or below index. Runs are ordered and
	// short, so a descending scan is as good as a search and easier to read;
	// the loop runs once for the common case of an index in the newest term.
	for i := len(rl.runs) - 1; i >= 0; i-- {
		if rl.runs[i].start <= index {
			return i
		}
	}
	return -1
}

// termRunStart returns the first index of the run of entries sharing the term
// at index, or 0 when index is not in the log.
//
// It answers the question a follower asks when it rejects an append: the
// leader wants to know how far back the disagreement goes, and the useful
// answer is the start of the term it disagreed in. That used to be a walk
// backwards with a storage read per index, bounded only by the length of a
// term.
func (rl *raftLog) termRunStart(index Index) Index {
	i := rl.runIndexFor(index)
	if i < 0 || index > rl.last {
		return 0
	}
	return rl.runs[i].start
}

// lastIndexOfTerm returns the highest index whose entry carries term, and
// whether the log holds any entry with it.
//
// It answers the question a leader asks when a follower rejects an append and
// names the term it disagreed in. That used to be a walk down from the end of
// the log with a storage read per index, bounded only by how far behind the
// follower had fallen.
func (rl *raftLog) lastIndexOfTerm(term Term) (Index, bool) {
	for i := len(rl.runs) - 1; i >= 0; i-- {
		if rl.runs[i].term != term {
			continue
		}
		if i+1 < len(rl.runs) {
			return rl.runs[i+1].start - 1, true
		}
		return rl.last, true
	}
	// A log with nothing left in it still describes one index: the snapshot
	// boundary. A leader that has compacted everything away can still tell a
	// follower where the term it asked about ended.
	if rl.first == 0 && rl.snapMeta.LastIncludedIndex != 0 &&
		rl.snapMeta.LastIncludedTerm == term {
		return rl.snapMeta.LastIncludedIndex, true
	}
	return 0, false
}

// extendRuns records the terms of newly appended entries. The entries are
// contiguous, start one past the end of the log, and carry terms no lower
// than the one already at the end of it.
func (rl *raftLog) extendRuns(entries []LogEntry) {
	for i := range entries {
		if n := len(rl.runs); n > 0 && rl.runs[n-1].term == entries[i].Term {
			continue
		}
		rl.runs = append(rl.runs, termRun{start: entries[i].Index, term: entries[i].Term})
	}
}

// truncateRunsFrom drops the record of every term beginning at or after
// fromIndex, and is how the run index follows a suffix truncation.
func (rl *raftLog) truncateRunsFrom(fromIndex Index) {
	for len(rl.runs) > 0 && rl.runs[len(rl.runs)-1].start >= fromIndex {
		rl.runs = rl.runs[:len(rl.runs)-1]
	}
	if len(rl.runs) == 0 {
		rl.runs = nil
	}
}

// truncateRunsBelow follows a prefix truncation: runs that ended before
// toIndex are gone, and the one that spans it now starts there.
func (rl *raftLog) truncateRunsBelow(toIndex Index) {
	i := rl.runIndexFor(toIndex)
	if i < 0 {
		rl.runs = nil
		return
	}
	rl.runs = append(rl.runs[:0], rl.runs[i:]...)
	rl.runs[0].start = toIndex
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
	// The term of the new last entry has to be taken before anything is
	// discarded, while the log can still describe it.
	newLast := fromIndex - 1
	newLastTerm := rl.snapMeta.LastIncludedTerm
	if fromIndex > rl.first && newLast > 0 {
		t, err := rl.termAt(newLast)
		if err != nil {
			return err
		}
		newLastTerm = t
	}

	rl.truncateRunsFrom(fromIndex)
	rl.dropUnstableFrom(fromIndex)
	if fromIndex > 0 && fromIndex-1 < rl.queuedDurable {
		rl.queuedDurable = fromIndex - 1
	}
	rl.w.enqueue(&writeOp{
		seq:          rl.nextWriteSeq(),
		kind:         writeTruncateSuffix,
		index:        fromIndex,
		durableAfter: rl.queuedDurable,
	})

	if fromIndex <= rl.first {
		rl.first = 0
		rl.last = 0
		rl.lastTerm = rl.snapMeta.LastIncludedTerm
		rl.runs = nil
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
	rl.w.enqueue(&writeOp{
		seq:          rl.nextWriteSeq(),
		kind:         writeTruncatePrefix,
		index:        toIndex,
		durableAfter: rl.queuedDurable,
	})

	if toIndex > rl.last {
		rl.first = 0
		rl.last = 0
		rl.lastTerm = rl.snapMeta.LastIncludedTerm
		rl.runs = nil
	} else {
		rl.truncateRunsBelow(toIndex)
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
	if t, err := rl.termAt(meta.LastIncludedIndex); err == nil && t == meta.LastIncludedTerm {
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
		rl.runs = nil
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
