package sharedwal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/brunoga/raft/v2"
)

// DefaultSegmentSize is the size at which the active segment is closed and a
// new one begun.
const DefaultSegmentSize = 64 << 20

// DefaultRewriteThreshold is the number of live entry bytes below which a
// segment that nothing else needs is reclaimed by copying those entries to
// the active segment.
const DefaultRewriteThreshold = 1 << 20

// Option configures Open.
type Option func(*options)

type options struct {
	segmentSize      int64
	rewriteThreshold int64
}

// WithSegmentSize sets the size at which a new segment is begun. Smaller
// segments are reclaimed sooner and cost more files.
func WithSegmentSize(bytes int64) Option {
	return func(o *options) { o.segmentSize = bytes }
}

// WithRewriteThreshold sets how many live entry bytes a segment may hold and
// still be reclaimed by rewriting them into the active segment. Zero disables
// rewriting entries; segments then live until every entry in them is gone.
func WithRewriteThreshold(bytes int64) Option {
	return func(o *options) { o.rewriteThreshold = bytes }
}

// ErrClosed is returned by every operation on a closed WAL.
var ErrClosed = errors.New("sharedwal: log is closed")

// WAL is one write-ahead log shared by every Raft group on a host. See the
// package documentation.
type WAL struct {
	dir  string
	opts options

	mu       sync.Mutex // guards everything below
	closed   bool
	lockF    *os.File
	segments []*segment // in sequence order; the last is active
	groups   map[uint64]*groupState

	// writes carries requests to the syncer goroutine; done is closed when it
	// has exited.
	writes chan *writeRequest
	done   chan struct{}
	// syncErr is the first write or sync failure the syncer hit. From then on
	// every request fails with it: a log that cannot be made durable cannot
	// be trusted to say anything is.
	syncErr error
}

// segment is one append-only file of records.
type segment struct {
	seq  int
	path string
	f    *os.File
	size int64
	// liveEntries and liveBytes count the entries in this segment that some
	// group's log still holds, and their command bytes. A segment with none,
	// that no group's latest hard state, snapshot or commit index lives in,
	// can be deleted.
	liveEntries int
	liveBytes   int64
	// pins counts the groups whose latest hard state, snapshot position or
	// commit index is recorded here.
	pins int
}

// entryLoc says where one entry lives.
type entryLoc struct {
	seg  *segment
	off  int64 // of the entry's own header within the segment
	size int   // header plus command
	term raft.Term
}

// groupState is what the log knows about one group.
type groupState struct {
	id      uint64
	hs      raft.HardState
	hsSeg   *segment // where the latest hard state lives, nil if none
	first   raft.Index
	locs    []entryLoc // locs[i] describes index first+i; empty when the log is empty
	snap    raft.SnapshotMeta
	hasSnap bool
	snapSeg *segment
	commit  raft.Index
	cmtSeg  *segment
}

func (g *groupState) last() raft.Index {
	if len(g.locs) == 0 {
		return 0
	}
	return g.first + raft.Index(len(g.locs)) - 1
}

// Open opens, or creates, the log at dir and recovers every group in it.
func Open(dir string, opts ...Option) (*WAL, error) {
	o := options{segmentSize: DefaultSegmentSize, rewriteThreshold: DefaultRewriteThreshold}
	for _, fn := range opts {
		fn(&o)
	}
	if o.segmentSize <= 0 {
		return nil, errors.New("sharedwal: segment size must be positive")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sharedwal: mkdir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "snap"), 0o700); err != nil {
		return nil, fmt.Errorf("sharedwal: mkdir snap: %w", err)
	}
	lockF, err := acquireDirLock(dir)
	if err != nil {
		return nil, err
	}
	w := &WAL{
		dir:    dir,
		opts:   o,
		lockF:  lockF,
		groups: make(map[uint64]*groupState),
		writes: make(chan *writeRequest, 1024),
		done:   make(chan struct{}),
	}
	if err := w.recover(); err != nil {
		_ = w.closeFiles()
		_ = releaseDirLock(lockF)
		return nil, err
	}
	go w.syncLoop()
	return w, nil
}

// Dir returns the directory the log lives in.
func (w *WAL) Dir() string { return w.dir }

// Groups returns the IDs of every group the log holds state for, in
// ascending order. It is how a host finds its groups again after a restart.
func (w *WAL) Groups() []uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]uint64, 0, len(w.groups))
	for id := range w.groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Storage returns the raft.Storage for one group. The same value is returned
// for the same group every time; a group the log has never seen starts
// empty.
func (w *WAL) Storage(groupID uint64) *GroupStore {
	return &GroupStore{w: w, id: groupID}
}

// Close makes everything already accepted durable, then closes the log. Every
// operation afterwards returns ErrClosed.
func (w *WAL) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	close(w.writes)
	<-w.done

	w.mu.Lock()
	defer w.mu.Unlock()
	return errors.Join(w.closeFiles(), releaseDirLock(w.lockF))
}

func (w *WAL) closeFiles() error {
	var errs []error
	for _, s := range w.segments {
		if s.f != nil {
			errs = append(errs, s.f.Close())
			s.f = nil
		}
	}
	return errors.Join(errs...)
}

// group returns the state for id, creating it if needed. Caller holds mu.
func (w *WAL) group(id uint64) *groupState {
	g, ok := w.groups[id]
	if !ok {
		g = &groupState{id: id}
		w.groups[id] = g
	}
	return g
}

// ---- Segments -----------------------------------------------------------------

const segmentPrefix = "wal-"

func segmentName(seq int) string {
	return fmt.Sprintf("%s%08d.log", segmentPrefix, seq)
}

func parseSegmentName(name string) (int, bool) {
	if !strings.HasPrefix(name, segmentPrefix) || !strings.HasSuffix(name, ".log") {
		return 0, false
	}
	seq, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, segmentPrefix), ".log"))
	if err != nil || seq < 0 {
		return 0, false
	}
	return seq, true
}

// createSegment begins a new active segment. Caller holds mu, or is the
// syncer between batches.
func (w *WAL) createSegment(seq int) (*segment, error) {
	path := filepath.Join(w.dir, segmentName(seq))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sharedwal: create segment: %w", err)
	}
	if err := syncDir(w.dir); err != nil {
		_ = f.Close()
		return nil, err
	}
	s := &segment{seq: seq, path: path, f: f}
	w.segments = append(w.segments, s)
	return s, nil
}

// active returns the segment appends go to, creating the first one on demand.
func (w *WAL) active() (*segment, error) {
	if len(w.segments) == 0 {
		return w.createSegment(0)
	}
	return w.segments[len(w.segments)-1], nil
}

// syncDir fsyncs a directory so that a created or removed name is durable.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sharedwal: open dir for sync: %w", err)
	}
	err = d.Sync()
	_ = d.Close()
	if err != nil {
		return fmt.Errorf("sharedwal: sync dir: %w", err)
	}
	return nil
}

// syncFile fsyncs a segment. It is a variable so that a test can count calls.
var syncFile = func(f *os.File) error { return f.Sync() }

// ---- Group commit -------------------------------------------------------------

// writeRequest is one caller's records, written and synced together with
// everything else waiting, then applied to the in-memory state.
type writeRequest struct {
	// records are the framed bytes to append, in order.
	records []byte
	// apply installs what the records describe once they are durable. locs
	// receives the segment and offset each record landed at, in order of
	// the recordOffsets the request declared.
	apply func(seg *segment, base int64)
	// done receives the outcome, once. nil means the caller is not waiting.
	done chan error
}

// submit hands a request to the syncer and waits for it unless wait is
// false.
func (w *WAL) submit(req *writeRequest, wait bool) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrClosed
	}
	if w.syncErr != nil {
		err := w.syncErr
		w.mu.Unlock()
		return err
	}
	w.mu.Unlock()
	if wait {
		req.done = make(chan error, 1)
	}
	// The channel is closed by Close after w.closed is set; a send racing
	// that close is caught by the recover below rather than by a lock, so
	// that submitters never hold the lock across a channel operation.
	if err := w.send(req); err != nil {
		return err
	}
	if !wait {
		return nil
	}
	return <-req.done
}

func (w *WAL) send(req *writeRequest) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrClosed
		}
	}()
	w.writes <- req
	return nil
}

// syncLoop is the one goroutine that writes segments. It drains every request
// waiting, writes them all, syncs once, then applies and answers each.
func (w *WAL) syncLoop() {
	defer close(w.done)
	for req := range w.writes {
		batch := []*writeRequest{req}
	drain:
		for len(batch) < 4096 {
			select {
			case r, ok := <-w.writes:
				if !ok {
					break drain
				}
				batch = append(batch, r)
			default:
				break drain
			}
		}
		w.writeBatch(batch)
	}
}

// writeBatch writes and syncs a batch, then applies it. Requests in a failed
// batch are all failed: the sync did not happen, so nothing in it is known
// to be durable.
func (w *WAL) writeBatch(batch []*writeRequest) {
	w.mu.Lock()
	if w.syncErr != nil {
		err := w.syncErr
		w.mu.Unlock()
		for _, r := range batch {
			answer(r, err)
		}
		return
	}
	// Placement: each record goes whole into the active segment, rotating
	// when it would overflow. The segment set is read here and extended
	// under the lock; the bytes are written outside it.
	type placement struct {
		req  *writeRequest
		seg  *segment
		base int64
	}
	placements := make([]placement, 0, len(batch))
	seg, err := w.active()
	if err == nil {
		for _, r := range batch {
			n := int64(len(r.records))
			if seg.size > 0 && seg.size+n > w.opts.segmentSize {
				// Sync what is in the old segment before moving on, so a
				// crash after the rotation cannot lose a record that a
				// later record in the new segment depends on.
				if err = syncFile(seg.f); err != nil {
					break
				}
				seg, err = w.createSegment(seg.seq + 1)
				if err != nil {
					break
				}
			}
			placements = append(placements, placement{req: r, seg: seg, base: seg.size})
			seg.size += n
		}
	}
	w.mu.Unlock()

	if err == nil {
		var last *segment
		for _, p := range placements {
			if _, werr := p.seg.f.WriteAt(p.req.records, p.base); werr != nil {
				err = fmt.Errorf("sharedwal: write segment: %w", werr)
				break
			}
			if last != nil && last != p.seg {
				if err = syncFile(last.f); err != nil {
					break
				}
			}
			last = p.seg
		}
		if err == nil && last != nil {
			if serr := syncFile(last.f); serr != nil {
				err = fmt.Errorf("sharedwal: sync segment: %w", serr)
			}
		}
	}

	w.mu.Lock()
	if err != nil {
		w.syncErr = err
		w.mu.Unlock()
		for _, r := range batch {
			answer(r, err)
		}
		return
	}
	for _, p := range placements {
		p.req.apply(p.seg, p.base)
	}
	w.mu.Unlock()
	for _, r := range batch {
		answer(r, nil)
	}
}

func answer(r *writeRequest, err error) {
	if r.done != nil {
		r.done <- err
	}
}

// ---- Recovery -----------------------------------------------------------------

// recover rebuilds every group's state from the segments on disk.
func (w *WAL) recover() error {
	names, err := os.ReadDir(w.dir)
	if err != nil {
		return fmt.Errorf("sharedwal: read dir: %w", err)
	}
	seqs := []int{}
	for _, e := range names {
		if seq, ok := parseSegmentName(e.Name()); ok {
			seqs = append(seqs, seq)
		}
	}
	sort.Ints(seqs)
	for i, seq := range seqs {
		if err := w.recoverSegment(seq, i == len(seqs)-1); err != nil {
			return err
		}
	}
	return w.pruneSnapshotFiles()
}

// recoverSegment replays one segment. On the last segment a record that fails
// its check ends the log there and the file is cut back to the last good
// record; anywhere else it is corruption.
func (w *WAL) recoverSegment(seq int, last bool) error {
	path := filepath.Join(w.dir, segmentName(seq))
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("sharedwal: open segment: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("sharedwal: read segment: %w", err)
	}
	s := &segment{seq: seq, path: path, f: f}
	w.segments = append(w.segments, s)

	off := int64(0)
	for int(off) < len(data) {
		rec, n, perr := parseRecord(data[off:])
		if perr != nil {
			if !last {
				return fmt.Errorf("sharedwal: %s at offset %d: %w", segmentName(seq), off, perr)
			}
			// A torn tail from a crash mid-write. Everything before it was
			// acknowledged only if it was synced, and a sync covers whole
			// batches, so cutting here loses nothing that was promised.
			if err := f.Truncate(off); err != nil {
				return fmt.Errorf("sharedwal: truncate torn tail: %w", err)
			}
			if err := syncFile(f); err != nil {
				return err
			}
			break
		}
		rec.off = off
		if err := w.replay(s, &rec); err != nil {
			return fmt.Errorf("sharedwal: %s at offset %d: %w", segmentName(seq), off, err)
		}
		off += int64(n)
	}
	s.size = off
	return nil
}

// replay applies one recovered record to the in-memory state, exactly as the
// syncer's apply step would have when it was written.
func (w *WAL) replay(s *segment, rec *parsedRecord) error {
	g := w.group(rec.group)
	switch rec.kind {
	case kindEntries:
		entries, offsets, err := decodeEntriesBody(rec.body)
		if err != nil {
			return err
		}
		w.installEntries(g, s, rec.off+recordHeaderSize, entries, offsets, len(rec.body))
	case kindHardState:
		hs, err := decodeHardStateBody(rec.body)
		if err != nil {
			return err
		}
		w.installHardState(g, s, hs)
	case kindTruncateSuffix:
		from, err := decodeIndexBody(rec.body)
		if err != nil {
			return err
		}
		w.dropSuffix(g, from)
	case kindTruncatePrefix:
		to, err := decodeIndexBody(rec.body)
		if err != nil {
			return err
		}
		w.dropPrefix(g, to)
	case kindSnapshot:
		meta, err := decodeSnapshotBody(rec.body)
		if err != nil {
			return err
		}
		w.installSnapshot(g, s, meta)
	case kindCommitIndex:
		idx, err := decodeIndexBody(rec.body)
		if err != nil {
			return err
		}
		w.installCommit(g, s, idx)
	case kindRemoveGroup:
		w.dropGroup(g)
	default:
		return fmt.Errorf("unknown record %s", kindName(rec.kind))
	}
	return nil
}

// ---- In-memory state transitions ----------------------------------------------
//
// Each of these is called both by the syncer after a record is durable and by
// recovery as the record is read back, so that the two agree by construction.
// Caller holds mu.

// installEntries records a run of entries that landed in s with the given
// body offset. An entry at an index the group already holds replaces it and
// everything after it, which is what a leader change looks like on disk.
func (w *WAL) installEntries(g *groupState, s *segment, bodyOff int64, entries []raft.LogEntry, offsets []int64, bodyLen int) {
	if len(entries) == 0 {
		return
	}
	first := entries[0].Index
	if len(g.locs) == 0 || first < g.first {
		w.dropSuffix(g, first)
		g.first = first
		g.locs = g.locs[:0]
	} else if first <= g.last() {
		w.dropSuffix(g, first)
	}
	for i := range entries {
		end := int64(bodyLen)
		if i+1 < len(offsets) {
			end = offsets[i+1]
		}
		loc := entryLoc{
			seg:  s,
			off:  bodyOff + offsets[i],
			size: int(end - offsets[i]),
			term: entries[i].Term,
		}
		g.locs = append(g.locs, loc)
		s.liveEntries++
		s.liveBytes += int64(loc.size - entryHeaderSize)
	}
}

func (w *WAL) unpin(s *segment) {
	if s != nil {
		s.pins--
	}
}

func (w *WAL) installHardState(g *groupState, s *segment, hs raft.HardState) {
	w.unpin(g.hsSeg)
	g.hs, g.hsSeg = hs, s
	s.pins++
}

func (w *WAL) installSnapshot(g *groupState, s *segment, meta raft.SnapshotMeta) {
	w.unpin(g.snapSeg)
	g.snap, g.hasSnap, g.snapSeg = meta, true, s
	s.pins++
}

func (w *WAL) installCommit(g *groupState, s *segment, idx raft.Index) {
	if idx <= g.commit && g.cmtSeg != nil {
		return
	}
	w.unpin(g.cmtSeg)
	g.commit, g.cmtSeg = max(idx, g.commit), s
	s.pins++
}

// dropSuffix forgets entries at index >= from.
func (w *WAL) dropSuffix(g *groupState, from raft.Index) {
	if len(g.locs) == 0 || from > g.last() {
		return
	}
	if from <= g.first {
		w.release(g.locs)
		g.locs = g.locs[:0]
		return
	}
	keep := int(from - g.first)
	w.release(g.locs[keep:])
	g.locs = g.locs[:keep]
}

// dropPrefix forgets entries at index < to.
func (w *WAL) dropPrefix(g *groupState, to raft.Index) {
	if len(g.locs) == 0 || to <= g.first {
		return
	}
	if to > g.last() {
		w.release(g.locs)
		g.locs = g.locs[:0]
		g.first = 0
		return
	}
	n := int(to - g.first)
	w.release(g.locs[:n])
	g.locs = append(g.locs[:0], g.locs[n:]...)
	g.first = to
}

// release gives the segments credit for entries no longer held.
func (w *WAL) release(locs []entryLoc) {
	for i := range locs {
		locs[i].seg.liveEntries--
		locs[i].seg.liveBytes -= int64(locs[i].size - entryHeaderSize)
	}
}

func (w *WAL) dropGroup(g *groupState) {
	w.release(g.locs)
	w.unpin(g.hsSeg)
	w.unpin(g.snapSeg)
	w.unpin(g.cmtSeg)
	delete(w.groups, g.id)
}

// ---- Reclaim ------------------------------------------------------------------

// reclaimable reports whether a non-active segment holds nothing any group
// still needs. Caller holds mu.
func (s *segment) reclaimable() bool {
	return s.liveEntries == 0 && s.pins <= 0
}

// Reclaim deletes every segment that nothing needs any more and rewrites the
// little that keeps an old one alive, so that it can be deleted too. It runs
// on its own after every prefix truncation; call it to reclaim space at a
// time of your choosing, or to see the error an automatic pass would have
// swallowed. A failure here leaks space and nothing else, which is why the
// automatic pass does not report it through the storage operation that
// triggered it.
func (w *WAL) Reclaim() error {
	return w.maybeReclaim()
}

// maybeReclaim deletes every non-active segment that nothing needs, and
// rewrites the little that keeps an old segment alive -- pinned records, or
// a small number of live entries -- into the active segment so that it can
// be deleted too. It is called after a compaction, which is when a segment
// is most likely to have just become empty.
func (w *WAL) maybeReclaim() error {
	w.mu.Lock()
	var rewrite []*segment
	for _, s := range w.segments[:max(len(w.segments)-1, 0)] {
		if s.reclaimable() {
			continue
		}
		if s.liveEntries > 0 && (w.opts.rewriteThreshold == 0 || s.liveBytes > w.opts.rewriteThreshold) {
			break // in order: nothing behind this can go until it does
		}
		rewrite = append(rewrite, s)
	}
	w.mu.Unlock()

	for _, s := range rewrite {
		if err := w.rewriteSegment(s); err != nil {
			return err
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	var errs []error
	kept := w.segments[:0]
	for i, s := range w.segments {
		if i == len(w.segments)-1 || !s.reclaimable() {
			kept = append(kept, s)
			continue
		}
		_ = s.f.Close()
		s.f = nil
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("sharedwal: remove segment: %w", err))
		}
	}
	if len(kept) != len(w.segments) {
		w.segments = kept
		errs = append(errs, syncDir(w.dir))
	}
	return errors.Join(errs...)
}

// rewriteSegment copies everything a segment still holds that some group
// needs -- pinned records and live entries -- into the active segment, so that
// the old one becomes reclaimable. Each group's copies are written through the
// ordinary durable path, one request per group, so that they land after
// anything the group has written since and before anything it writes next.
func (w *WAL) rewriteSegment(s *segment) error {
	w.mu.Lock()
	type work struct {
		g       *groupState
		hs      *raft.HardState
		snap    *raft.SnapshotMeta
		commit  *raft.Index
		entries []raft.LogEntry
		lo, hi  raft.Index
	}
	var todo []work
	for _, g := range w.groups {
		var wk work
		wk.g = g
		if g.hsSeg == s {
			hs := g.hs
			wk.hs = &hs
		}
		if g.snapSeg == s {
			snap := g.snap
			wk.snap = &snap
		}
		if g.cmtSeg == s {
			c := g.commit
			wk.commit = &c
		}
		// Live entries in s form a contiguous run in the group's log only
		// when the group was written to nothing else in between; the run is
		// copied by rewriting the whole tail from the first entry that lives
		// in s, which preserves contiguity and is bounded by the threshold.
		for i := range g.locs {
			if g.locs[i].seg == s {
				wk.lo = g.first + raft.Index(i)
				wk.hi = g.last()
				break
			}
		}
		if wk.hs != nil || wk.snap != nil || wk.commit != nil || wk.lo != 0 {
			todo = append(todo, wk)
		}
	}
	w.mu.Unlock()

	for _, wk := range todo {
		gs := &GroupStore{w: w, id: wk.g.id}
		if wk.lo != 0 {
			entries, err := gs.readRange(wk.lo, wk.hi+1)
			if err != nil {
				return err
			}
			wk.entries = entries
		}
		var buf []byte
		var offsets []int64
		if wk.hs != nil {
			buf = appendRecord(buf, wk.g.id, kindHardState, encodeHardStateBody(*wk.hs))
		}
		if wk.snap != nil {
			buf = appendRecord(buf, wk.g.id, kindSnapshot, encodeSnapshotBody(*wk.snap))
		}
		if wk.commit != nil {
			buf = appendRecord(buf, wk.g.id, kindCommitIndex, encodeIndexBody(*wk.commit))
		}
		entriesAt, bodyLen := int64(-1), 0
		if len(wk.entries) > 0 {
			body, offs := encodeEntriesBody(wk.entries)
			entriesAt = int64(len(buf))
			offsets = offs
			bodyLen = len(body)
			buf = appendRecord(buf, wk.g.id, kindEntries, body)
		}
		g := wk.g
		req := &writeRequest{
			records: buf,
			apply: func(seg *segment, base int64) {
				// The group may have moved on since the copies were read:
				// only install what is still current, so that a newer
				// record is never overridden by a rewritten older one.
				if wk.hs != nil && g.hsSeg == s {
					w.installHardState(g, seg, *wk.hs)
				}
				if wk.snap != nil && g.snapSeg == s {
					w.installSnapshot(g, seg, *wk.snap)
				}
				if wk.commit != nil && g.cmtSeg == s {
					w.unpin(g.cmtSeg)
					g.cmtSeg = seg
					seg.pins++
				}
				if entriesAt >= 0 {
					// Re-point every entry that is still the one copied.
					bodyOff := base + entriesAt + recordHeaderSize
					for i, e := range wk.entries {
						if e.Index < g.first || e.Index > g.last() {
							continue
						}
						loc := &g.locs[e.Index-g.first]
						if loc.seg != s || loc.term != e.Term {
							continue
						}
						loc.seg.liveEntries--
						loc.seg.liveBytes -= int64(loc.size - entryHeaderSize)
						loc.seg, loc.off = seg, bodyOff+offsets[i]
						end := int64(bodyLen)
						if i+1 < len(offsets) {
							end = offsets[i+1]
						}
						loc.size = int(end - offsets[i])
						seg.liveEntries++
						seg.liveBytes += int64(loc.size - entryHeaderSize)
					}
				}
			},
		}
		if err := w.submit(req, true); err != nil {
			return err
		}
	}
	return nil
}
