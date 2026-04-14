// Package filestore provides a durable, file-backed implementation of
// raft.Storage with automatic log segment rotation.
//
// Layout under the data directory:
//
//	meta              — fixed-size hard state record (term + votedFor)
//	seg-NNNNN.log     — append-only binary log segment
//	seg-NNNNN.idx     — dense array of uint64 byte offsets, one per log entry
//	snap              — latest snapshot (written atomically via snap.tmp → snap)
//
// Segments are numbered sequentially from 00000. When the active segment's
// log file reaches SegmentSize bytes a new segment is created. Old segments
// are sealed (immutable) and can be dropped wholesale during prefix truncation,
// avoiding the expensive in-place compaction for all but the newest segment.
//
// Every mutating operation fsyncs before returning so that Raft safety
// invariants hold across crashes.
//
// On open, the tail of the last (active) segment is scanned for partial
// writes; any corrupt entries are truncated and the index file is rebuilt
// to match. Sealed segments are trusted to be complete.
package filestore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/brunoga/raft"
)

const (
	// metaSize is the fixed on-disk size of the hard-state record:
	//   term:8 + votedForLen:2 + votedFor:256 = 266 bytes
	votedForMaxLen = 256
	metaSize       = 8 + 2 + votedForMaxLen // 266

	// Log entry wire format:
	//   crc32:4 | index:8 | term:8 | dataLen:4 | data[dataLen]
	entryHeaderSize = 4 + 8 + 8 + 4 // 24 bytes

	// Index entry: one uint64 byte-offset per log entry (segment-local).
	idxEntrySize = 8

	// defaultSegmentSize is the log-file size threshold for rotation.
	defaultSegmentSize = 64 * 1024 * 1024 // 64 MiB
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// ---- segment ---------------------------------------------------------------

// segment manages one log+index file pair. Byte offsets stored in the .idx
// file are relative to byte 0 of that segment's .log file.
type segment struct {
	seqNum  int        // creation sequence (used to derive file names)
	logF    *os.File
	idxF    *os.File
	firstID raft.Index // raft index of the first entry; 0 = empty
	lastID  raft.Index // raft index of the last entry; 0 = empty
	logSize int64      // current byte size of logF
}

// contains reports whether this segment holds the entry at index.
func (s *segment) contains(index raft.Index) bool {
	return s.firstID != 0 && s.firstID <= index && s.lastID >= index
}

// writeEntry appends one entry to the segment. Caller holds FileStore.mu.
func (s *segment) writeEntry(e raft.LogEntry) error {
	header, payload := encodeEntry(e)
	offset := s.logSize

	// Use WriteAt so we always write at the exact end of the segment
	// regardless of where the file descriptor's position currently is.
	// (ReadAt operations during recovery/reads do not advance the position,
	// but we cannot rely on it being at EOF.)
	if _, err := s.logF.WriteAt(header[:], offset); err != nil {
		return fmt.Errorf("filestore: write seg%05d log header: %w", s.seqNum, err)
	}
	if len(payload) > 0 {
		if _, err := s.logF.WriteAt(payload, offset+entryHeaderSize); err != nil {
			return fmt.Errorf("filestore: write seg%05d log payload: %w", s.seqNum, err)
		}
	}
	s.logSize += int64(entryHeaderSize) + int64(len(payload))

	// Append the segment-local byte offset to the index file.
	var idxBuf [idxEntrySize]byte
	binary.LittleEndian.PutUint64(idxBuf[:], uint64(offset))
	idxPos := int64(e.Index-s.firstID) * idxEntrySize
	if _, err := s.idxF.WriteAt(idxBuf[:], idxPos); err != nil {
		return fmt.Errorf("filestore: write seg%05d idx: %w", s.seqNum, err)
	}

	if s.firstID == 0 {
		s.firstID = e.Index
	}
	s.lastID = e.Index
	return nil
}

// readIdxOffsetAt returns the byte offset stored at position i in the idx file.
func (s *segment) readIdxOffsetAt(i int64) (int64, error) {
	var buf [idxEntrySize]byte
	if _, err := s.idxF.ReadAt(buf[:], i*idxEntrySize); err != nil {
		return 0, fmt.Errorf("filestore: read seg%05d idx[%d]: %w", s.seqNum, i, err)
	}
	return int64(binary.LittleEndian.Uint64(buf[:])), nil
}

// readIdxOffset returns the segment-local byte offset for the given raft index.
func (s *segment) readIdxOffset(index raft.Index) (int64, error) {
	if !s.contains(index) {
		return 0, fmt.Errorf("%w: index %d not in seg%05d [%d,%d]",
			raft.ErrNotFound, index, s.seqNum, s.firstID, s.lastID)
	}
	return s.readIdxOffsetAt(int64(index - s.firstID))
}

// decodeEntryAt reads and verifies the log entry at the given segment-local
// byte offset.
func (s *segment) decodeEntryAt(offset int64) (raft.LogEntry, error) {
	var hdr [entryHeaderSize]byte
	if _, err := s.logF.ReadAt(hdr[:], offset); err != nil {
		return raft.LogEntry{}, fmt.Errorf("filestore: read seg%05d entry header at %d: %w",
			s.seqNum, offset, err)
	}

	storedCRC := binary.LittleEndian.Uint32(hdr[0:4])
	index := raft.Index(binary.LittleEndian.Uint64(hdr[4:12]))
	term := raft.Term(binary.LittleEndian.Uint64(hdr[12:20]))
	dataLen := binary.LittleEndian.Uint32(hdr[20:24])

	var payload []byte
	if dataLen > 0 {
		payload = make([]byte, dataLen)
		if _, err := s.logF.ReadAt(payload, offset+entryHeaderSize); err != nil {
			return raft.LogEntry{}, fmt.Errorf("filestore: read seg%05d payload at %d: %w",
				s.seqNum, offset, err)
		}
	}

	h := crc32.New(crcTable)
	h.Write(hdr[4:]) // index + term + dataLen
	h.Write(payload)
	if computed := h.Sum32(); computed != storedCRC {
		return raft.LogEntry{}, fmt.Errorf(
			"filestore: CRC mismatch in seg%05d at offset %d (stored %08x, computed %08x)",
			s.seqNum, offset, storedCRC, computed)
	}

	return raft.LogEntry{Index: index, Term: term, Command: payload}, nil
}

// readEntry reads the entry at the given raft index.
func (s *segment) readEntry(index raft.Index) (raft.LogEntry, error) {
	offset, err := s.readIdxOffset(index)
	if err != nil {
		return raft.LogEntry{}, err
	}
	return s.decodeEntryAt(offset)
}

// getEntries returns entries in [lo, hi) from this segment. The caller must
// ensure the entire range is within [s.firstID, s.lastID].
func (s *segment) getEntries(lo, hi raft.Index) ([]raft.LogEntry, error) {
	n := int(hi - lo)
	// Bulk-read all n index entries in one call to avoid N individual seeks.
	idxBuf := make([]byte, int64(n)*idxEntrySize)
	idxPos := int64(lo-s.firstID) * idxEntrySize
	if _, err := s.idxF.ReadAt(idxBuf, idxPos); err != nil {
		return nil, fmt.Errorf("filestore: bulk read seg%05d idx [%d,%d): %w",
			s.seqNum, lo, hi, err)
	}
	entries := make([]raft.LogEntry, 0, n)
	for i := range n {
		offset := int64(binary.LittleEndian.Uint64(idxBuf[i*idxEntrySize:]))
		e, err := s.decodeEntryAt(offset)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// sync fsyncs both the log and index files.
func (s *segment) sync() error {
	if err := s.logF.Sync(); err != nil {
		return fmt.Errorf("filestore: sync seg%05d log: %w", s.seqNum, err)
	}
	if err := s.idxF.Sync(); err != nil {
		return fmt.Errorf("filestore: sync seg%05d idx: %w", s.seqNum, err)
	}
	return nil
}

// close closes both file handles.
func (s *segment) close() error {
	var errs []error
	if s.logF != nil {
		if err := s.logF.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.idxF != nil {
		if err := s.idxF.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---- FileStore -------------------------------------------------------------

// FileStore is the file-backed raft.Storage implementation.
type FileStore struct {
	mu      sync.Mutex
	dir     string
	metaF   *os.File
	segs    []*segment
	segSize int64
}

// Open opens (or creates) a FileStore rooted at dir using the default 64 MiB
// segment size. It performs crash recovery on the active segment's tail.
func Open(dir string) (*FileStore, error) {
	return openWith(dir, defaultSegmentSize)
}

// OpenWithSegmentSize is like Open but uses the given segment size threshold.
// A new segment is created once the current log file reaches size bytes.
// Useful in tests where a small threshold exercises rotation quickly.
func OpenWithSegmentSize(dir string, size int64) (*FileStore, error) {
	if size <= 0 {
		return nil, fmt.Errorf("filestore: segment size must be positive")
	}
	return openWith(dir, size)
}

func openWith(dir string, segSize int64) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("filestore: mkdir %s: %w", dir, err)
	}

	// Complete or roll back any interrupted TruncatePrefix Phase 2 operations
	// before loading segments. This must happen before loadSegments so that
	// the segment files are in a consistent state when we read firstID/lastID.
	if err := recoverPendingTruncations(dir); err != nil {
		return nil, fmt.Errorf("filestore: recover truncations: %w", err)
	}

	metaF, err := openFile(filepath.Join(dir, "meta"))
	if err != nil {
		return nil, err
	}

	fs := &FileStore{
		dir:     dir,
		metaF:   metaF,
		segSize: segSize,
	}

	if err := fs.loadSegments(); err != nil {
		metaF.Close()
		return nil, fmt.Errorf("filestore: load segments: %w", err)
	}

	if err := fs.recoverLast(); err != nil {
		fs.closeAll()
		return nil, fmt.Errorf("filestore: recovery: %w", err)
	}

	return fs, nil
}

// recoverPendingTruncations completes or discards interrupted TruncatePrefix
// Phase 2 operations. Phase 2 writes .log.tmp and .idx.tmp, then renames
// .log.tmp → .log (commit point), then .idx.tmp → .idx. Three crash states:
//
//   - Both .log.tmp and .idx.tmp exist: crash before the first rename.
//     Original files are untouched; discard both tmps.
//
//   - Only .idx.tmp exists (.log.tmp is gone): crash after .log was renamed
//     but before .idx was renamed. Complete by renaming .idx.tmp → .idx.
//
//   - Only .log.tmp exists: partial write before .idx.tmp was created;
//     original files are untouched. Discard the .log.tmp.
func recoverPendingTruncations(dir string) error {
	idxTmps, err := filepath.Glob(filepath.Join(dir, "seg-?????.idx.tmp"))
	if err != nil {
		return err
	}
	for _, idxTmp := range idxTmps {
		base := filepath.Base(idxTmp) // "seg-NNNNN.idx.tmp"
		seqStr := strings.TrimPrefix(strings.TrimSuffix(base, ".idx.tmp"), "seg-")
		logTmp := filepath.Join(dir, "seg-"+seqStr+".log.tmp")

		if _, statErr := os.Stat(logTmp); statErr == nil {
			// Both tmps exist — crash before the first rename; discard both.
			_ = os.Remove(idxTmp)
			_ = os.Remove(logTmp)
		} else if errors.Is(statErr, os.ErrNotExist) {
			// Only .idx.tmp exists — log was already renamed; finish the idx rename.
			finalIdx := filepath.Join(dir, "seg-"+seqStr+".idx")
			if renErr := os.Rename(idxTmp, finalIdx); renErr != nil {
				return fmt.Errorf("filestore: recover: rename %s → %s: %w", idxTmp, finalIdx, renErr)
			}
		} else {
			return fmt.Errorf("filestore: recover: stat %s: %w", logTmp, statErr)
		}
	}

	// Discard any orphaned .log.tmp files (no matching .idx.tmp).
	logTmps, err := filepath.Glob(filepath.Join(dir, "seg-?????.log.tmp"))
	if err != nil {
		return err
	}
	for _, logTmp := range logTmps {
		_ = os.Remove(logTmp)
	}

	return nil
}

// loadSegments discovers all segment files in dir and opens them.
// Sealed (non-last) segments have firstID/lastID read directly from their
// file content without a full CRC scan.
func (fs *FileStore) loadSegments() error {
	pattern := filepath.Join(fs.dir, "seg-?????.log")
	logFiles, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	sort.Strings(logFiles) // lexicographic == ascending seqNum (zero-padded)

	for _, logPath := range logFiles {
		base := filepath.Base(logPath)
		// "seg-NNNNN.log" → extract NNNNN
		seqStr := strings.TrimPrefix(strings.TrimSuffix(base, ".log"), "seg-")
		seqNum, err := strconv.Atoi(seqStr)
		if err != nil {
			return fmt.Errorf("filestore: unexpected segment filename %q: %w", base, err)
		}

		s, err := fs.openSegment(seqNum)
		if err != nil {
			return err
		}
		fs.segs = append(fs.segs, s)
	}
	return nil
}

// openSegment opens the log+idx files for the given sequence number and reads
// firstID/lastID from the file content. On success the segment is appended to
// fs.segs.
func (fs *FileStore) openSegment(seqNum int) (*segment, error) {
	logF, err := openFile(fs.segLogPath(seqNum))
	if err != nil {
		return nil, err
	}
	idxF, err := openFile(fs.segIdxPath(seqNum))
	if err != nil {
		logF.Close()
		return nil, err
	}

	logStat, err := logF.Stat()
	if err != nil {
		logF.Close()
		idxF.Close()
		return nil, fmt.Errorf("filestore: stat seg%05d log: %w", seqNum, err)
	}

	idxStat, err := idxF.Stat()
	if err != nil {
		logF.Close()
		idxF.Close()
		return nil, fmt.Errorf("filestore: stat seg%05d idx: %w", seqNum, err)
	}

	s := &segment{
		seqNum:  seqNum,
		logF:    logF,
		idxF:    idxF,
		logSize: logStat.Size(),
	}

	numEntries := idxStat.Size() / idxEntrySize
	if numEntries == 0 {
		// Empty segment: firstID/lastID stay 0.
		return s, nil
	}

	// Read first entry to get firstID.
	firstOffset, err := s.readIdxOffsetAt(0)
	if err != nil {
		s.close()
		return nil, err
	}
	first, err := s.decodeEntryAt(firstOffset)
	if err != nil {
		s.close()
		return nil, err
	}
	s.firstID = first.Index

	// Read last entry to get lastID. For the active (last) segment this may
	// fail if the tail is corrupt — recoverLast() will repair it afterward.
	// For sealed segments it always succeeds because they were synced before
	// the next segment was created.
	lastOffset, err := s.readIdxOffsetAt(numEntries - 1)
	if err == nil {
		if last, decErr := s.decodeEntryAt(lastOffset); decErr == nil {
			s.lastID = last.Index
		}
		// If decodeEntryAt fails, lastID stays 0; recoverLast will fix it.
	}

	return s, nil
}

// createSegment creates new log+idx files for seqNum and returns an empty
// segment.
func (fs *FileStore) createSegment(seqNum int) (*segment, error) {
	logF, err := openFile(fs.segLogPath(seqNum))
	if err != nil {
		return nil, err
	}
	idxF, err := openFile(fs.segIdxPath(seqNum))
	if err != nil {
		logF.Close()
		return nil, err
	}
	return &segment{seqNum: seqNum, logF: logF, idxF: idxF}, nil
}

// recoverLast performs crash recovery on the last (active) segment. It walks
// every entry, verifies CRCs, and truncates any corrupt tail. Sealed segments
// (all but the last) are assumed complete. If the last segment is empty after
// recovery it is removed.
func (fs *FileStore) recoverLast() error {
	if len(fs.segs) == 0 {
		return nil
	}
	s := fs.segs[len(fs.segs)-1]

	idxStat, err := s.idxF.Stat()
	if err != nil {
		return err
	}
	numEntries := idxStat.Size() / idxEntrySize
	if numEntries == 0 {
		// Empty segment (possibly from a rotation-then-crash). Remove it.
		_ = s.close()
		_ = os.Remove(fs.segLogPath(s.seqNum))
		_ = os.Remove(fs.segIdxPath(s.seqNum))
		fs.segs = fs.segs[:len(fs.segs)-1]
		return nil
	}

	// Walk all entries, verify CRC; find the last valid one.
	lastValid := int64(-1)
	for i := int64(0); i < numEntries; i++ {
		offset, err := s.readIdxOffsetAt(i)
		if err != nil {
			break
		}
		if _, err = s.decodeEntryAt(offset); err != nil {
			break
		}
		lastValid = i
	}

	if lastValid == int64(numEntries)-1 {
		// All entries valid. firstID/lastID are already correct from openSegment.
		return nil
	}

	// Truncate to lastValid+1 entries.
	validCount := lastValid + 1
	if validCount <= 0 {
		// Nothing valid — wipe both files and remove the segment.
		_ = s.logF.Truncate(0)
		_ = s.idxF.Truncate(0)
		_ = s.close()
		_ = os.Remove(fs.segLogPath(s.seqNum))
		_ = os.Remove(fs.segIdxPath(s.seqNum))
		fs.segs = fs.segs[:len(fs.segs)-1]
		return nil
	}

	// Find where the good log data ends.
	goodLogEnd, err := s.readIdxOffsetAt(validCount - 1)
	if err != nil {
		return err
	}
	last, err := s.decodeEntryAt(goodLogEnd)
	if err != nil {
		return err
	}
	goodLogEnd += int64(entryHeaderSize) + int64(len(last.Command))

	_ = s.logF.Truncate(goodLogEnd)
	_ = s.idxF.Truncate(validCount * idxEntrySize)

	// Reload firstID/lastID from the file.
	firstOffset, err := s.readIdxOffsetAt(0)
	if err != nil {
		return err
	}
	first, err := s.decodeEntryAt(firstOffset)
	if err != nil {
		return err
	}
	s.firstID = first.Index
	s.lastID = last.Index
	s.logSize = goodLogEnd
	return nil
}

// ---- Hard state ------------------------------------------------------------

func (fs *FileStore) SaveHardState(_ context.Context, hs raft.HardState) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	voted := []byte(hs.VotedFor)
	if len(voted) > votedForMaxLen {
		return fmt.Errorf("filestore: VotedFor too long (%d > %d)", len(voted), votedForMaxLen)
	}

	var buf [metaSize]byte
	binary.LittleEndian.PutUint64(buf[0:8], uint64(hs.CurrentTerm))
	binary.LittleEndian.PutUint16(buf[8:10], uint16(len(voted)))
	copy(buf[10:10+votedForMaxLen], voted)

	if _, err := fs.metaF.WriteAt(buf[:], 0); err != nil {
		return fmt.Errorf("filestore: write meta: %w", err)
	}
	return fs.metaF.Sync()
}

func (fs *FileStore) LoadHardState(_ context.Context) (raft.HardState, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	var buf [metaSize]byte
	n, err := fs.metaF.ReadAt(buf[:], 0)
	if err == io.EOF && n == 0 {
		return raft.HardState{}, nil
	}
	if err != nil && err != io.EOF {
		return raft.HardState{}, fmt.Errorf("filestore: read meta: %w", err)
	}
	if n < metaSize {
		return raft.HardState{}, nil
	}

	term := raft.Term(binary.LittleEndian.Uint64(buf[0:8]))
	vlen := binary.LittleEndian.Uint16(buf[8:10])
	if int(vlen) > votedForMaxLen {
		return raft.HardState{}, fmt.Errorf("filestore: corrupt meta: votedForLen=%d", vlen)
	}
	votedFor := raft.NodeID(buf[10 : 10+vlen])
	return raft.HardState{CurrentTerm: term, VotedFor: votedFor}, nil
}

// ---- Log -------------------------------------------------------------------

func (fs *FileStore) AppendLogEntries(_ context.Context, entries []raft.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	firstDirtySegIdx := len(fs.segs)
	if firstDirtySegIdx > 0 {
		firstDirtySegIdx-- // active segment may already exist
	}

	for _, e := range entries {
		// Rotate if the active segment is non-empty and at or above the size limit.
		active := fs.activeSeg()
		if active == nil || (active.lastID != 0 && active.logSize >= fs.segSize) {
			if active != nil {
				// Sync the outgoing segment before sealing it.
				if err := active.sync(); err != nil {
					return err
				}
			}
			nextSeq := 0
			if active != nil {
				nextSeq = active.seqNum + 1
			}
			newSeg, err := fs.createSegment(nextSeq)
			if err != nil {
				return err
			}
			fs.segs = append(fs.segs, newSeg)
			if firstDirtySegIdx >= len(fs.segs) {
				firstDirtySegIdx = len(fs.segs) - 1
			}
		}

		active = fs.activeSeg()
		// Set firstID before writing so writeEntry can compute the idx position.
		if active.firstID == 0 {
			active.firstID = e.Index
		}
		if err := active.writeEntry(e); err != nil {
			return err
		}
	}

	// Sync the final active segment (and any segments written to during rotation).
	for i := firstDirtySegIdx; i < len(fs.segs); i++ {
		if err := fs.segs[i].sync(); err != nil {
			return err
		}
	}
	return nil
}

func (fs *FileStore) GetLogEntry(_ context.Context, index raft.Index) (raft.LogEntry, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	s := fs.findSeg(index)
	if s == nil {
		return raft.LogEntry{}, fmt.Errorf("%w: index %d", raft.ErrNotFound, index)
	}
	return s.readEntry(index)
}

func (fs *FileStore) GetLogEntries(_ context.Context, lo, hi raft.Index) ([]raft.LogEntry, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if lo >= hi {
		return nil, nil
	}

	var result []raft.LogEntry
	cur := lo
	for cur < hi {
		s := fs.findSeg(cur)
		if s == nil {
			return nil, fmt.Errorf("%w: index %d", raft.ErrNotFound, cur)
		}
		// Read up to the end of this segment or hi, whichever comes first.
		segHi := hi
		if s.lastID+1 < segHi {
			segHi = s.lastID + 1
		}
		entries, err := s.getEntries(cur, segHi)
		if err != nil {
			return nil, err
		}
		result = append(result, entries...)
		cur = segHi
	}
	return result, nil
}

func (fs *FileStore) FirstIndex(_ context.Context) (raft.Index, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.segs) == 0 {
		return 0, nil
	}
	return fs.segs[0].firstID, nil
}

func (fs *FileStore) LastIndex(_ context.Context) (raft.Index, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	active := fs.activeSeg()
	if active == nil {
		return 0, nil
	}
	return active.lastID, nil
}

func (fs *FileStore) TruncateSuffix(_ context.Context, fromIndex raft.Index) error {
	// pathPair holds the log and index file paths of a segment to be deleted.
	type pathPair struct{ log, idx string }

	fs.mu.Lock()

	if len(fs.segs) == 0 {
		fs.mu.Unlock()
		return nil
	}
	last := fs.activeSeg()
	if last.lastID != 0 && fromIndex > last.lastID {
		fs.mu.Unlock()
		return nil
	}
	first := fs.segs[0]
	if first.firstID != 0 && fromIndex < first.firstID {
		fs.mu.Unlock()
		return fmt.Errorf("%w: TruncateSuffix(%d) < firstIndex(%d)",
			raft.ErrCompacted, fromIndex, first.firstID)
	}

	boundIdx := fs.findSegIdx(fromIndex)
	if boundIdx < 0 {
		fs.mu.Unlock()
		return nil
	}

	// Safety: mark all later segments for deletion BEFORE touching the boundary.
	// Close their handles and remove them from fs.segs now (under lock) so no
	// concurrent reader can access them; the actual os.Remove calls happen after
	// we release the lock so other goroutines (e.g. SaveSnapshot) are not blocked
	// during potentially slow filesystem operations.
	var toDelete []pathPair
	for i := len(fs.segs) - 1; i > boundIdx; i-- {
		s := fs.segs[i]
		if err := s.close(); err != nil {
			fs.mu.Unlock()
			return err
		}
		toDelete = append(toDelete, pathPair{fs.segLogPath(s.seqNum), fs.segIdxPath(s.seqNum)})
	}
	fs.segs = fs.segs[:boundIdx+1]

	// Truncate the boundary segment (file-level ops; must stay under lock since
	// the open *os.File is shared with concurrent readers via the mutex).
	s := fs.segs[boundIdx]
	var boundaryPath *pathPair // set only when the entire boundary seg is removed
	if s.firstID == 0 || fromIndex <= s.firstID {
		// Zero-out and sync the entire boundary segment before removing it, so
		// that a crash after the delete but before syncDir leaves the file
		// content invalid, preventing accidental re-use on recovery.
		if err := s.logF.Truncate(0); err != nil {
			fs.mu.Unlock()
			return fmt.Errorf("filestore: truncate seg%05d log: %w", s.seqNum, err)
		}
		if err := s.idxF.Truncate(0); err != nil {
			fs.mu.Unlock()
			return fmt.Errorf("filestore: truncate seg%05d idx: %w", s.seqNum, err)
		}
		if err := s.sync(); err != nil {
			fs.mu.Unlock()
			return err
		}
		if err := s.close(); err != nil {
			fs.mu.Unlock()
			return err
		}
		p := pathPair{fs.segLogPath(s.seqNum), fs.segIdxPath(s.seqNum)}
		boundaryPath = &p
		fs.segs = fs.segs[:boundIdx]
	} else {
		// Partial truncation within the boundary segment.
		logOffset, err := s.readIdxOffset(fromIndex)
		if err != nil {
			fs.mu.Unlock()
			return err
		}
		if err := s.logF.Truncate(logOffset); err != nil {
			fs.mu.Unlock()
			return fmt.Errorf("filestore: truncate seg%05d log: %w", s.seqNum, err)
		}
		numKeep := int64(fromIndex - s.firstID)
		if err := s.idxF.Truncate(numKeep * idxEntrySize); err != nil {
			fs.mu.Unlock()
			return fmt.Errorf("filestore: truncate seg%05d idx: %w", s.seqNum, err)
		}
		if err := s.sync(); err != nil {
			fs.mu.Unlock()
			return err
		}
		s.lastID = fromIndex - 1
		s.logSize = logOffset
	}

	// All in-memory state updated. Release the lock before slow filesystem ops.
	fs.mu.Unlock()

	// Remove files outside the lock. Crash safety: the files were zeroed/synced
	// (or never written to) before we dropped the lock, so recovery will not
	// mistake them for valid data even if os.Remove hasn't run yet.
	for _, p := range toDelete {
		if err := os.Remove(p.log); err != nil {
			return fmt.Errorf("filestore: remove seg log: %w", err)
		}
		if err := os.Remove(p.idx); err != nil {
			return fmt.Errorf("filestore: remove seg idx: %w", err)
		}
	}
	if boundaryPath != nil {
		if err := os.Remove(boundaryPath.log); err != nil {
			return fmt.Errorf("filestore: remove seg log: %w", err)
		}
		if err := os.Remove(boundaryPath.idx); err != nil {
			return fmt.Errorf("filestore: remove seg idx: %w", err)
		}
	}
	if len(toDelete) > 0 || boundaryPath != nil {
		return syncDir(fs.dir)
	}
	return nil
}

func (fs *FileStore) TruncatePrefix(_ context.Context, toIndex raft.Index) error {
	type pathPair struct{ log, idx string }

	fs.mu.Lock()

	if len(fs.segs) == 0 || fs.segs[0].firstID == 0 {
		fs.mu.Unlock()
		return nil
	}
	if toIndex <= fs.segs[0].firstID {
		fs.mu.Unlock()
		return nil
	}

	last := fs.activeSeg()
	if toIndex > last.lastID+1 {
		toIndex = last.lastID + 1
	}

	// Phase 1: drop complete leading segments where lastID < toIndex.
	// Close handles and collect paths under the lock; remove files after unlock.
	var toDelete []pathPair
	for len(fs.segs) > 1 { // always keep at least one segment
		s := fs.segs[0]
		if s.lastID == 0 || s.lastID >= toIndex {
			break
		}
		if err := s.close(); err != nil {
			fs.mu.Unlock()
			return err
		}
		toDelete = append(toDelete, pathPair{fs.segLogPath(s.seqNum), fs.segIdxPath(s.seqNum)})
		fs.segs = fs.segs[1:]
	}

	// Phase 2 needs fs.segs to be non-empty; capture the boundary segment
	// pointer before releasing the lock.
	var phase2seg *segment
	if len(fs.segs) > 0 {
		seg := fs.segs[0]
		if seg.firstID != 0 && seg.firstID < toIndex {
			phase2seg = seg
		}
	}

	// All in-memory state updated. Release lock before slow filesystem ops.
	fs.mu.Unlock()

	// Remove whole-segment files outside the lock.
	for _, p := range toDelete {
		if err := os.Remove(p.log); err != nil {
			return fmt.Errorf("filestore: remove seg log: %w", err)
		}
		if err := os.Remove(p.idx); err != nil {
			return fmt.Errorf("filestore: remove seg idx: %w", err)
		}
	}
	if len(toDelete) > 0 {
		if err := syncDir(fs.dir); err != nil {
			return err
		}
	}

	if phase2seg == nil {
		return nil
	}

	// Phase 2: crash-safe rewrite of the boundary segment using tmp+rename.
	//
	// We write the kept content to seg-NNNNN.log.tmp and seg-NNNNN.idx.tmp,
	// then rename log.tmp → log (the commit point), then idx.tmp → idx.
	// recoverPendingTruncations (called by Open) detects which rename(s)
	// completed and finishes or rolls back the operation accordingly.
	//
	// Re-acquire lock because we are reading and replacing shared file handles.
	fs.mu.Lock()
	defer fs.mu.Unlock()

	seg := phase2seg
	numDrop := int(toIndex - seg.firstID)
	numKeep := int(seg.lastID-toIndex) + 1
	if numKeep <= 0 {
		// Wipe the segment entirely (shouldn't happen if phase 1 ran correctly,
		// but guard against edge cases). Zeroing both files in-place is safe
		// here because an empty segment is indistinguishable from a freshly
		// created one; recovery never needs to distinguish the two.
		if err := seg.logF.Truncate(0); err != nil {
			return fmt.Errorf("filestore: truncate seg%05d log: %w", seg.seqNum, err)
		}
		if err := seg.logF.Sync(); err != nil {
			return err
		}
		if err := seg.idxF.Truncate(0); err != nil {
			return fmt.Errorf("filestore: truncate seg%05d idx: %w", seg.seqNum, err)
		}
		if err := seg.idxF.Sync(); err != nil {
			return err
		}
		seg.firstID = 0
		seg.lastID = 0
		seg.logSize = 0
		return nil
	}

	// Find the byte offset of the first kept log entry.
	firstKeptOffset, err := seg.readIdxOffsetAt(int64(numDrop))
	if err != nil {
		return fmt.Errorf("filestore: read first-kept offset in seg%05d: %w", seg.seqNum, err)
	}

	// Read the kept portion of the log.
	logStat, err := seg.logF.Stat()
	if err != nil {
		return fmt.Errorf("filestore: stat seg%05d log: %w", seg.seqNum, err)
	}
	keepLogSize := logStat.Size() - firstKeptOffset
	keepLogBuf := make([]byte, keepLogSize)
	if _, err := seg.logF.ReadAt(keepLogBuf, firstKeptOffset); err != nil {
		return fmt.Errorf("filestore: read kept log in seg%05d: %w", seg.seqNum, err)
	}

	// Read and adjust the kept index entries (offsets become relative to new byte 0).
	keepIdxBuf := make([]byte, int64(numKeep)*idxEntrySize)
	if _, err := seg.idxF.ReadAt(keepIdxBuf, int64(numDrop)*idxEntrySize); err != nil {
		return fmt.Errorf("filestore: read idx for prefix truncate in seg%05d: %w", seg.seqNum, err)
	}
	for i := range numKeep {
		off := int64(binary.LittleEndian.Uint64(keepIdxBuf[i*idxEntrySize:]))
		off -= firstKeptOffset
		binary.LittleEndian.PutUint64(keepIdxBuf[i*idxEntrySize:], uint64(off))
	}

	// Write kept log content to .log.tmp.
	logTmpPath := fs.segLogPath(seg.seqNum) + ".tmp"
	idxTmpPath := fs.segIdxPath(seg.seqNum) + ".tmp"

	cleanup := func() {
		_ = os.Remove(logTmpPath)
		_ = os.Remove(idxTmpPath)
	}

	logTmpF, err := os.OpenFile(logTmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("filestore: create log.tmp in seg%05d: %w", seg.seqNum, err)
	}
	if _, err := logTmpF.Write(keepLogBuf); err != nil {
		logTmpF.Close(); cleanup()
		return fmt.Errorf("filestore: write log.tmp in seg%05d: %w", seg.seqNum, err)
	}
	if err := logTmpF.Sync(); err != nil {
		logTmpF.Close(); cleanup()
		return fmt.Errorf("filestore: sync log.tmp in seg%05d: %w", seg.seqNum, err)
	}
	logTmpF.Close()

	// Write adjusted index to .idx.tmp.
	idxTmpF, err := os.OpenFile(idxTmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		cleanup()
		return fmt.Errorf("filestore: create idx.tmp in seg%05d: %w", seg.seqNum, err)
	}
	if _, err := idxTmpF.Write(keepIdxBuf); err != nil {
		idxTmpF.Close(); cleanup()
		return fmt.Errorf("filestore: write idx.tmp in seg%05d: %w", seg.seqNum, err)
	}
	if err := idxTmpF.Sync(); err != nil {
		idxTmpF.Close(); cleanup()
		return fmt.Errorf("filestore: sync idx.tmp in seg%05d: %w", seg.seqNum, err)
	}
	idxTmpF.Close()

	// Close the existing open handles before renaming over the files.
	if err := seg.logF.Close(); err != nil {
		cleanup()
		return fmt.Errorf("filestore: close seg%05d log: %w", seg.seqNum, err)
	}
	seg.logF = nil
	if err := seg.idxF.Close(); err != nil {
		cleanup()
		return fmt.Errorf("filestore: close seg%05d idx: %w", seg.seqNum, err)
	}
	seg.idxF = nil

	// Atomic rename: log first (the commit point), then idx.
	// recoverPendingTruncations detects a crash between the two renames and
	// completes the idx rename on the next Open.
	if err := os.Rename(logTmpPath, fs.segLogPath(seg.seqNum)); err != nil {
		return fmt.Errorf("filestore: rename log.tmp in seg%05d: %w", seg.seqNum, err)
	}
	if err := os.Rename(idxTmpPath, fs.segIdxPath(seg.seqNum)); err != nil {
		// idx.tmp still exists; recoverPendingTruncations will finish this.
		return fmt.Errorf("filestore: rename idx.tmp in seg%05d: %w", seg.seqNum, err)
	}

	// Reopen the now-replaced files and update in-memory state.
	seg.logF, err = openFile(fs.segLogPath(seg.seqNum))
	if err != nil {
		return fmt.Errorf("filestore: reopen seg%05d log after truncate: %w", seg.seqNum, err)
	}
	seg.idxF, err = openFile(fs.segIdxPath(seg.seqNum))
	if err != nil {
		seg.logF.Close()
		seg.logF = nil
		return fmt.Errorf("filestore: reopen seg%05d idx after truncate: %w", seg.seqNum, err)
	}

	seg.firstID = toIndex
	seg.logSize = keepLogSize

	return syncDir(fs.dir)
}

// ---- Snapshot --------------------------------------------------------------

func (fs *FileStore) SaveSnapshot(_ context.Context, meta raft.SnapshotMeta, data []byte) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	tmpPath := filepath.Join(fs.dir, "snap.tmp")
	snapPath := filepath.Join(fs.dir, "snap")

	f, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("filestore: create snap.tmp: %w", err)
	}

	var hdr [24]byte
	binary.LittleEndian.PutUint64(hdr[0:8], uint64(meta.LastIncludedIndex))
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(meta.LastIncludedTerm))
	binary.LittleEndian.PutUint64(hdr[16:24], uint64(len(data)))

	if _, err := f.Write(hdr[:]); err != nil {
		f.Close()
		return fmt.Errorf("filestore: write snap header: %w", err)
	}
	if len(data) > 0 {
		if _, err := f.Write(data); err != nil {
			f.Close()
			return fmt.Errorf("filestore: write snap data: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("filestore: sync snap.tmp: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, snapPath); err != nil {
		return fmt.Errorf("filestore: rename snap: %w", err)
	}
	return syncDir(fs.dir)
}

func (fs *FileStore) LoadSnapshot(_ context.Context) (raft.SnapshotMeta, []byte, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	snapPath := filepath.Join(fs.dir, "snap")
	f, err := os.Open(snapPath)
	if os.IsNotExist(err) {
		return raft.SnapshotMeta{}, nil, raft.ErrNoSnapshot
	}
	if err != nil {
		return raft.SnapshotMeta{}, nil, fmt.Errorf("filestore: open snap: %w", err)
	}
	defer f.Close()

	var hdr [24]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return raft.SnapshotMeta{}, nil, fmt.Errorf("filestore: read snap header: %w", err)
	}
	meta := raft.SnapshotMeta{
		LastIncludedIndex: raft.Index(binary.LittleEndian.Uint64(hdr[0:8])),
		LastIncludedTerm:  raft.Term(binary.LittleEndian.Uint64(hdr[8:16])),
	}
	dataLen := binary.LittleEndian.Uint64(hdr[16:24])
	data := make([]byte, dataLen)
	if _, err := io.ReadFull(f, data); err != nil {
		return raft.SnapshotMeta{}, nil, fmt.Errorf("filestore: read snap data: %w", err)
	}
	return meta, data, nil
}

// ---- Lifecycle -------------------------------------------------------------

func (fs *FileStore) Close() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.closeAll()
}

// closeAll closes all open file handles. Must be called with mu held.
func (fs *FileStore) closeAll() error {
	var errs []error
	if fs.metaF != nil {
		if err := fs.metaF.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, s := range fs.segs {
		if err := s.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---- Internal helpers ------------------------------------------------------

// activeSeg returns the last (writable) segment, or nil if there are none.
func (fs *FileStore) activeSeg() *segment {
	if len(fs.segs) == 0 {
		return nil
	}
	return fs.segs[len(fs.segs)-1]
}

// findSeg returns the segment that contains index, or nil.
func (fs *FileStore) findSeg(index raft.Index) *segment {
	i := fs.findSegIdx(index)
	if i < 0 {
		return nil
	}
	return fs.segs[i]
}

// findSegIdx returns the position in fs.segs of the segment containing index,
// or -1 if not found.
func (fs *FileStore) findSegIdx(index raft.Index) int {
	// Binary search: find largest i such that segs[i].firstID <= index.
	lo, hi := 0, len(fs.segs)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		s := fs.segs[mid]
		if s.lastID != 0 && s.lastID < index {
			lo = mid + 1
		} else if s.firstID > index {
			hi = mid - 1
		} else if s.firstID == 0 {
			// Empty segment — shouldn't contain anything.
			hi = mid - 1
		} else {
			return mid
		}
	}
	return -1
}

// segLogPath returns the path of the log file for a given sequence number.
func (fs *FileStore) segLogPath(seqNum int) string {
	return filepath.Join(fs.dir, fmt.Sprintf("seg-%05d.log", seqNum))
}

// segIdxPath returns the path of the index file for a given sequence number.
func (fs *FileStore) segIdxPath(seqNum int) string {
	return filepath.Join(fs.dir, fmt.Sprintf("seg-%05d.idx", seqNum))
}

// openFile opens the file at path for read/write, creating it if needed.
func openFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filestore: open %s: %w", path, err)
	}
	return f, nil
}

// syncDir fsyncs the directory itself to make rename/unlink operations durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("filestore: open dir for sync: %w", err)
	}
	defer d.Close()
	return d.Sync()
}

// encodeEntry serialises a LogEntry into (header, payload).
// Wire format: crc32:4 | index:8 | term:8 | dataLen:4 | data
func encodeEntry(e raft.LogEntry) ([entryHeaderSize]byte, []byte) {
	var hdr [entryHeaderSize]byte
	binary.LittleEndian.PutUint64(hdr[4:12], uint64(e.Index))
	binary.LittleEndian.PutUint64(hdr[12:20], uint64(e.Term))
	binary.LittleEndian.PutUint32(hdr[20:24], uint32(len(e.Command)))

	h := crc32.New(crcTable)
	h.Write(hdr[4:])
	h.Write(e.Command)
	binary.LittleEndian.PutUint32(hdr[0:4], h.Sum32())

	return hdr, e.Command
}
