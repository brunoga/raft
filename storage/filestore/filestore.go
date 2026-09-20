// Package filestore provides a durable, file-backed implementation of
// raft.Storage with automatic log segment rotation.
//
// Layout under the data directory:
//
//	meta              — hard state (term + votedFor) in two checksummed slots
//	seg-NNNNNNNNNN.log — append-only binary log segment
//	seg-NNNNNNNNNN.idx — dense array of uint64 byte offsets, one per log entry
//	snap              — latest snapshot (written atomically via snap.tmp → snap)
//
// Segments are numbered sequentially from 0. When the active segment's log
// file reaches SegmentSize bytes a new segment is created. Old segments are
// sealed (immutable) and can be dropped wholesale during prefix truncation,
// avoiding the expensive in-place compaction for all but the newest segment.
// Segment file names written by older releases used a narrower zero-padded
// sequence number; any width is still recognised on open.
//
// Every mutating operation fsyncs before returning — and fsyncs the containing
// directory whenever a file is created, renamed or unlinked — so that Raft
// safety invariants hold across crashes.
//
// On open, the tail of the active segment is walked and validated: the index
// file is a dense array in which slot i must hold the entry with index
// firstID+i at a strictly increasing byte offset, and every entry's checksum
// must verify. The first violation marks the end of the durable data and
// everything after it is truncated away. Segments that do not chain
// contiguously onto their predecessor are discarded, because a gap can only
// mean that the log beyond it was already discarded but its files outlived a
// crash.
package filestore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
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
	// votedForMaxLen bounds the node ID stored in the hard-state record.
	votedForMaxLen = 256

	// Hard-state record (one slot):
	//   crc32:4 | seq:8 | term:8 | votedForLen:2 | votedFor:256 = 278 bytes
	//
	// Two slots are written alternately so that a torn write always leaves the
	// previous record intact. The slot with the highest sequence number whose
	// checksum verifies is the current one.
	metaRecordSize = 4 + 8 + 8 + 2 + votedForMaxLen // 278
	metaSlots      = 2
	metaFileSize   = metaRecordSize * metaSlots // 556

	// legacyMetaSize is the size of the pre-checksum hard-state record written
	// by older releases: term:8 | votedForLen:2 | votedFor:256.
	legacyMetaSize = 8 + 2 + votedForMaxLen // 266

	// Log entry wire format:
	//   crc32:4 | index:8 | term:8 | dataLen:4 | data[dataLen]
	entryHeaderSize = 4 + 8 + 8 + 4 // 24 bytes

	// Index entry: one uint64 byte-offset per log entry (segment-local).
	idxEntrySize = 8

	// defaultSegmentSize is the log-file size threshold for rotation.
	defaultSegmentSize = 64 * 1024 * 1024 // 64 MiB

	// copyChunkSize bounds the amount of memory used when a segment is
	// rewritten during prefix truncation. Without it the whole kept tail of a
	// segment (up to the segment size) would be buffered at once.
	copyChunkSize = 1 << 20 // 1 MiB

	// segNamePrefix and segNameFormat control segment file naming. The width
	// is wide enough that the sequence number cannot realistically wrap the
	// field; names written with a narrower width by older releases are still
	// recognised because the number is parsed rather than matched positionally.
	segNamePrefix = "seg-"
	segNameFormat = segNamePrefix + "%010d"
)

// Snapshot file format:
//
//	magic:4 "RSNP" | version:4 | lastIncludedIndex:8 | lastIncludedTerm:8
//	body[dataLen]
//	dataLen:8 | crc32:4
//
// Older releases wrote a bare 16-byte header (index + term) followed by the
// raw body with no length and no checksum. Such files are still readable but
// cannot be verified.
var snapMagic = [4]byte{'R', 'S', 'N', 'P'}

const (
	snapVersion          = 1
	snapHeaderSize       = 4 + 4 + 8 + 8 // 24
	snapTrailerSize      = 8 + 4         // 12
	legacySnapHeaderSize = 8 + 8         // 16
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// ---- segment ---------------------------------------------------------------

// segment manages one log+index file pair. Byte offsets stored in the .idx
// file are relative to byte 0 of that segment's .log file.
type segment struct {
	seqNum  int    // creation sequence (ordering key)
	name    string // file base name without extension, e.g. "seg-0000000007"
	logF    *os.File
	idxF    *os.File
	firstID raft.Index // raft index of the first entry; 0 = empty
	lastID  raft.Index // raft index of the last entry; 0 = empty
	logSize int64      // current byte size of logF

	// needsScan is set when the cheap consistency check performed while
	// opening the segment failed. Such a segment must be walked entry by
	// entry before its contents can be trusted.
	needsScan bool
}

// contains reports whether this segment holds the entry at index.
func (s *segment) contains(index raft.Index) bool {
	return s.firstID != 0 && s.firstID <= index && s.lastID >= index
}

// writeEntry appends one entry to the segment. Caller holds FileStore.mu.
func (s *segment) writeEntry(e raft.LogEntry) error {
	// The index file is addressed by e.Index-s.firstID. Index is unsigned, so
	// an entry that precedes the segment would silently wrap to a huge slot
	// (and a negative file offset once converted). Reject it instead.
	if s.firstID == 0 || e.Index < s.firstID {
		return fmt.Errorf("filestore: seg%05d: entry index %d precedes segment first index %d",
			s.seqNum, e.Index, s.firstID)
	}

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
	if offset < 0 {
		return raft.LogEntry{}, fmt.Errorf("filestore: seg%05d: negative entry offset %d",
			s.seqNum, offset)
	}

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
		s.logF = nil
	}
	if s.idxF != nil {
		if err := s.idxF.Close(); err != nil {
			errs = append(errs, err)
		}
		s.idxF = nil
	}
	return errors.Join(errs...)
}

// ---- FileStore -------------------------------------------------------------

// FileStore is the file-backed raft.Storage implementation.
type FileStore struct {
	mu      sync.Mutex
	dir     string
	metaF   *os.File
	hsSeq   uint64 // sequence number of the newest hard-state record on disk
	segs    []*segment
	segSize int64

	// snapMu serialises snapshot writers. It is deliberately separate from mu:
	// snapshot data is streamed from a reader the caller controls (on a
	// follower, one InstallSnapshot chunk at a time, delivered by the Raft
	// loop), so the store-wide lock must not be held while it is copied.
	snapMu sync.Mutex
}

// Open opens (or creates) a FileStore rooted at dir using the default 64 MiB
// segment size. It performs crash recovery on the log and validates the
// hard-state record.
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
	if err := mkdirAllSync(dir, 0o700); err != nil {
		return nil, err
	}

	// Complete or roll back any interrupted TruncatePrefix Phase 2 operations
	// before loading segments. This must happen before loadSegments so that
	// the segment files are in a consistent state when we read firstID/lastID.
	if err := recoverPendingTruncations(dir); err != nil {
		return nil, fmt.Errorf("filestore: recover truncations: %w", err)
	}

	// A snapshot that was still being written when the process died is
	// worthless; the committed snapshot (if any) is under its final name.
	if err := os.Remove(filepath.Join(dir, "snap.tmp")); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("filestore: remove stale snap.tmp: %w", err)
	}

	metaPath := filepath.Join(dir, "meta")
	_, statErr := os.Stat(metaPath)
	metaIsNew := errors.Is(statErr, os.ErrNotExist)

	metaF, err := openFile(metaPath)
	if err != nil {
		return nil, err
	}
	if metaIsNew {
		// A newly created file's directory entry is not durable until the
		// directory itself is fsynced.
		if err = syncDir(dir); err != nil {
			_ = metaF.Close()
			return nil, err
		}
	}

	fs := &FileStore{
		dir:     dir,
		metaF:   metaF,
		segSize: segSize,
	}

	if err = fs.loadSegments(); err != nil {
		_ = metaF.Close()
		return nil, fmt.Errorf("filestore: load segments: %w", err)
	}

	if err = fs.recoverSegments(); err != nil {
		_ = fs.closeAll()
		return nil, fmt.Errorf("filestore: recovery: %w", err)
	}

	// Validate the hard-state record up front. A record that exists but cannot
	// be authenticated must never be reported as "nothing saved yet": doing so
	// would resurrect a lower term or forget a vote already granted.
	hs, seq, legacy, err := fs.readMeta()
	if err != nil {
		_ = fs.closeAll()
		return nil, err
	}
	fs.hsSeq = seq
	if legacy {
		// Migrate to the checksummed two-slot layout. Sequence 1 is skipped so
		// that the first record lands in slot 1, leaving the legacy bytes in
		// slot 0 untouched until the new record is durable.
		fs.hsSeq = 1
		if err = fs.writeMeta(hs); err != nil {
			_ = fs.closeAll()
			return nil, fmt.Errorf("filestore: migrate meta: %w", err)
		}
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
	idxTmps, err := filepath.Glob(filepath.Join(dir, segNamePrefix+"*.idx.tmp"))
	if err != nil {
		return err
	}
	renamed := false
	for _, idxTmp := range idxTmps {
		base := filepath.Base(idxTmp) // "seg-NNNN.idx.tmp"
		name := strings.TrimSuffix(base, ".idx.tmp")
		if _, ok := parseSegSeq(name); !ok {
			continue // not one of ours
		}
		logTmp := filepath.Join(dir, name+".log.tmp")

		if _, statErr := os.Stat(logTmp); statErr == nil {
			// Both tmps exist — crash before the first rename; discard both.
			_ = os.Remove(idxTmp)
			_ = os.Remove(logTmp)
		} else if errors.Is(statErr, os.ErrNotExist) {
			// Only .idx.tmp exists — log was already renamed; finish the idx rename.
			finalIdx := filepath.Join(dir, name+".idx")
			if renErr := os.Rename(idxTmp, finalIdx); renErr != nil {
				return fmt.Errorf("filestore: recover: rename %s → %s: %w", idxTmp, finalIdx, renErr)
			}
			renamed = true
		} else {
			return fmt.Errorf("filestore: recover: stat %s: %w", logTmp, statErr)
		}
	}

	// Discard any orphaned .log.tmp files (no matching .idx.tmp).
	logTmps, err := filepath.Glob(filepath.Join(dir, segNamePrefix+"*.log.tmp"))
	if err != nil {
		return err
	}
	for _, logTmp := range logTmps {
		if _, ok := parseSegSeq(strings.TrimSuffix(filepath.Base(logTmp), ".log.tmp")); !ok {
			continue
		}
		_ = os.Remove(logTmp)
	}

	if renamed {
		return syncDir(dir)
	}
	return nil
}

// loadSegments discovers all segment files in dir and opens them, ordered by
// sequence number. Sealed (non-last) segments have firstID/lastID read from
// their index file without a full checksum scan.
func (fs *FileStore) loadSegments() error {
	logFiles, err := filepath.Glob(filepath.Join(fs.dir, segNamePrefix+"*.log"))
	if err != nil {
		return err
	}

	type found struct {
		name string
		seq  int
	}
	discovered := make([]found, 0, len(logFiles))
	for _, logPath := range logFiles {
		name := strings.TrimSuffix(filepath.Base(logPath), ".log")
		seq, ok := parseSegSeq(name)
		if !ok {
			// Some other file that happens to share the prefix. Leave it alone
			// rather than failing the whole open.
			continue
		}
		discovered = append(discovered, found{name: name, seq: seq})
	}
	// Sort numerically: names written with different zero-padding widths do not
	// order correctly lexicographically.
	sort.Slice(discovered, func(i, j int) bool { return discovered[i].seq < discovered[j].seq })

	for _, f := range discovered {
		s, err := fs.openSegment(f.seq, f.name)
		if err != nil {
			return err
		}
		fs.segs = append(fs.segs, s)
	}
	return nil
}

// openSegment opens the log+idx files for a discovered segment and derives
// firstID/lastID from the index file. The index file is a dense array, so the
// last slot must hold the entry with index firstID+numEntries-1; if it does
// not, or if either end fails to decode, the segment is flagged for a full
// validating scan instead of being trusted.
func (fs *FileStore) openSegment(seqNum int, name string) (*segment, error) {
	logF, err := openFile(fs.logPath(name))
	if err != nil {
		return nil, err
	}
	idxF, err := openFile(fs.idxPath(name))
	if err != nil {
		_ = logF.Close()
		return nil, err
	}

	s := &segment{seqNum: seqNum, name: name, logF: logF, idxF: idxF}

	logStat, err := logF.Stat()
	if err != nil {
		_ = s.close()
		return nil, fmt.Errorf("filestore: stat seg%05d log: %w", seqNum, err)
	}
	s.logSize = logStat.Size()

	idxStat, err := idxF.Stat()
	if err != nil {
		_ = s.close()
		return nil, fmt.Errorf("filestore: stat seg%05d idx: %w", seqNum, err)
	}

	numEntries := idxStat.Size() / idxEntrySize
	if numEntries == 0 {
		// Empty segment: firstID/lastID stay 0.
		return s, nil
	}

	// A damaged or partially written index is recoverable: the segment opens
	// and is marked for a full scan, which rebuilds what the index should have
	// said. Returning the error instead would refuse to open a store that is
	// perfectly repairable.
	firstOffset, err := s.readIdxOffsetAt(0)
	if err != nil {
		s.needsScan = true
		return s, nil //nolint:nilerr // damaged index: repair by scanning
	}
	first, err := s.decodeEntryAt(firstOffset)
	if err != nil {
		s.needsScan = true
		return s, nil //nolint:nilerr // damaged index: repair by scanning
	}
	lastOffset, err := s.readIdxOffsetAt(numEntries - 1)
	if err != nil {
		s.needsScan = true
		return s, nil //nolint:nilerr // damaged index: repair by scanning
	}
	last, err := s.decodeEntryAt(lastOffset)
	if err != nil {
		s.needsScan = true
		return s, nil //nolint:nilerr // damaged index: repair by scanning
	}
	if last.Index != first.Index+raft.Index(numEntries)-1 {
		// An index slot that was never durably written reads back as offset 0,
		// which decodes as the segment's first entry with a perfectly valid
		// checksum. The dense-array invariant is what catches that.
		s.needsScan = true
		return s, nil
	}

	s.firstID = first.Index
	s.lastID = last.Index
	return s, nil
}

// createSegment creates new log+idx files for seqNum and returns an empty
// segment. The directory is fsynced so the new names survive a crash.
func (fs *FileStore) createSegment(seqNum int) (*segment, error) {
	name := fmt.Sprintf(segNameFormat, seqNum)
	logF, err := openFile(fs.logPath(name))
	if err != nil {
		return nil, err
	}
	idxF, err := openFile(fs.idxPath(name))
	if err != nil {
		_ = logF.Close()
		return nil, err
	}
	// fsync(fd) does not make a new directory entry durable; the directory
	// itself has to be fsynced before anything is written into the segment.
	if err = syncDir(fs.dir); err != nil {
		_ = logF.Close()
		_ = idxF.Close()
		return nil, err
	}
	return &segment{seqNum: seqNum, name: name, logF: logF, idxF: idxF}, nil
}

// ---- Recovery --------------------------------------------------------------

// recoverSegments brings the on-disk log back to a state the Raft engine can
// trust: the active segment's tail is validated and any partial write beyond
// the last durable entry is truncated away, then segments that hold nothing or
// that do not chain contiguously onto their predecessor are removed.
func (fs *FileStore) recoverSegments() error {
	for i, s := range fs.segs {
		if i == len(fs.segs)-1 || s.needsScan {
			if err := fs.repairSegment(s); err != nil {
				return err
			}
		}
	}

	dropped, err := fs.pruneSegments()
	if err != nil {
		return err
	}
	if dropped && len(fs.segs) > 0 {
		// Pruning can promote a previously sealed segment to active. Validate
		// the tail we are about to append to.
		if err := fs.repairSegment(fs.segs[len(fs.segs)-1]); err != nil {
			return err
		}
		if _, err := fs.pruneSegments(); err != nil {
			return err
		}
	}
	return nil
}

// repairSegment walks a segment's index array and truncates it back to the
// last durable entry.
//
// Three invariants are checked for slot i, in addition to the entry checksum:
// the slot's byte offset must be strictly greater than the previous slot's,
// the decoded entry index must equal firstID+i, and the log file must not
// extend past the last indexed entry. The first violation is the true durable
// tail: an index slot that was allocated but never written reads back as
// offset 0, which otherwise decodes as a perfectly valid copy of the first
// entry and would silently hide every entry after it.
func (fs *FileStore) repairSegment(s *segment) error {
	idxStat, err := s.idxF.Stat()
	if err != nil {
		return fmt.Errorf("filestore: stat seg%05d idx: %w", s.seqNum, err)
	}
	logStat, err := s.logF.Stat()
	if err != nil {
		return fmt.Errorf("filestore: stat seg%05d log: %w", s.seqNum, err)
	}
	numEntries := idxStat.Size() / idxEntrySize

	var (
		validCount int64
		firstID    raft.Index
		lastID     raft.Index
		logEnd     int64
	)
	prevOffset := int64(-1)
	for i := int64(0); i < numEntries; i++ {
		offset, offErr := s.readIdxOffsetAt(i)
		if offErr != nil {
			break
		}
		if offset <= prevOffset {
			break // offsets in a dense, append-only index strictly increase
		}
		e, decErr := s.decodeEntryAt(offset)
		if decErr != nil {
			break
		}
		if i == 0 {
			firstID = e.Index
		} else if e.Index != firstID+raft.Index(i) {
			break // slot i does not hold the entry it is supposed to
		}
		prevOffset = offset
		lastID = e.Index
		validCount = i + 1
		logEnd = offset + int64(entryHeaderSize) + int64(len(e.Command))
	}

	if validCount == 0 {
		if logStat.Size() != 0 || idxStat.Size() != 0 {
			if err = s.logF.Truncate(0); err != nil {
				return fmt.Errorf("filestore: truncate seg%05d log: %w", s.seqNum, err)
			}
			if err = s.idxF.Truncate(0); err != nil {
				return fmt.Errorf("filestore: truncate seg%05d idx: %w", s.seqNum, err)
			}
			if syncErr := s.sync(); syncErr != nil {
				return syncErr
			}
		}
		s.firstID, s.lastID, s.logSize, s.needsScan = 0, 0, 0, false
		return nil
	}

	if validCount != numEntries || logStat.Size() != logEnd {
		if err = s.logF.Truncate(logEnd); err != nil {
			return fmt.Errorf("filestore: truncate seg%05d log: %w", s.seqNum, err)
		}
		if err = s.idxF.Truncate(validCount * idxEntrySize); err != nil {
			return fmt.Errorf("filestore: truncate seg%05d idx: %w", s.seqNum, err)
		}
		if err := s.sync(); err != nil {
			return err
		}
	}

	s.firstID = firstID
	s.lastID = lastID
	s.logSize = logEnd
	s.needsScan = false
	return nil
}

// unlinkSegment closes one segment and removes its files, and does not return
// until the removal is durable.
//
// Segments are always unlinked one at a time and in a deliberate order: a
// suffix truncation works from the tail inwards, a prefix truncation from the
// head outwards. Unlinks in a directory are not ordered with respect to one
// another unless the directory is fsynced between them, so removing a run of
// segments in one batch lets a crash leave a hole in the *middle* of the log —
// say segment 0 and segment 2 present with segment 1 gone. Recovery cannot
// safely resolve that: after an interrupted suffix truncation the live entries
// are the run before the hole, after an interrupted prefix truncation they are
// the run after it, and nothing on disk distinguishes the two. Making each
// unlink durable in order means the surviving segments are always a contiguous
// run, which is what lets pruneSegments simply keep the leading one.
//
// The index file is removed before the log file: a segment whose index is gone
// reads back as empty, which is the state recovery handles most simply.
func (fs *FileStore) unlinkSegment(s *segment) error {
	if err := s.close(); err != nil {
		return err
	}
	for _, p := range []string{fs.idxPath(s.name), fs.logPath(s.name)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("filestore: remove %s: %w", p, err)
		}
	}
	return syncDir(fs.dir)
}

// pruneSegments removes segments that hold no entries and every segment from
// the first one that does not start exactly one index past the end of its
// predecessor. Such a gap can only mean that the entries beyond it were
// discarded (by a suffix truncation) but their files outlived the crash;
// reporting them as present would resurrect entries the engine believes gone.
// It reports whether anything was removed.
func (fs *FileStore) pruneSegments() (bool, error) {
	var keep, drop []*segment
	for i := 0; i < len(fs.segs); i++ {
		s := fs.segs[i]
		if s.firstID == 0 {
			drop = append(drop, s)
			continue
		}
		if len(keep) > 0 && s.firstID != keep[len(keep)-1].lastID+1 {
			drop = append(drop, fs.segs[i:]...)
			break
		}
		keep = append(keep, s)
	}
	if len(drop) == 0 {
		return false, nil
	}

	var errs []error
	for _, s := range drop {
		if err := s.close(); err != nil {
			errs = append(errs, err)
		}
		for _, p := range []string{fs.logPath(s.name), fs.idxPath(s.name)} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("filestore: remove %s: %w", p, err))
			}
		}
	}
	fs.segs = keep
	if err := errors.Join(errs...); err != nil {
		return true, err
	}
	return true, syncDir(fs.dir)
}

// ---- Hard state ------------------------------------------------------------

// SaveHardState durably persists currentTerm and votedFor. The record is
// checksummed and written to one of two slots chosen by an increasing
// sequence number, so a torn or interrupted write can never destroy the
// previously persisted state.
func (fs *FileStore) SaveHardState(_ context.Context, hs raft.HardState) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.writeMeta(hs)
}

// SaveState implements raft.BatchWriter. It records the hard state and appends
// the entries under a single acquisition of the store lock, in that order, and
// returns only once both are durable.
//
// A store that kept its hard state inside the log itself could make this one
// record and one fsync. This one keeps it in a separate file, so it is still
// two syncs; what the batch buys here is that the log is not unlocked and
// relocked between them, and that the engine issues one call where it used to
// issue two. The ordering is the part that matters for correctness: a log
// recovered after a crash must never hold entries from a term the node does
// not believe it reached.
func (fs *FileStore) SaveState(_ context.Context, hs *raft.HardState, entries []raft.LogEntry) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if hs != nil {
		if err := fs.writeMeta(*hs); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		return nil
	}
	return fs.appendLocked(entries)
}

// LoadHardState returns the last durably saved hard state. It returns a
// zero-value HardState only when nothing has ever been saved; a record that
// exists but fails verification is reported as an error.
func (fs *FileStore) LoadHardState(_ context.Context) (raft.HardState, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	hs, _, _, err := fs.readMeta()
	if err != nil {
		return raft.HardState{}, err
	}
	return hs, nil
}

// writeMeta writes hs into the next hard-state slot and fsyncs. Caller holds mu.
func (fs *FileStore) writeMeta(hs raft.HardState) error {
	voted := []byte(hs.VotedFor)
	if len(voted) > votedForMaxLen {
		return fmt.Errorf("filestore: VotedFor too long (%d > %d)", len(voted), votedForMaxLen)
	}

	seq := fs.hsSeq + 1

	var rec [metaRecordSize]byte
	binary.LittleEndian.PutUint64(rec[4:12], seq)
	binary.LittleEndian.PutUint64(rec[12:20], uint64(hs.CurrentTerm))
	binary.LittleEndian.PutUint16(rec[20:22], uint16(len(voted)))
	copy(rec[22:22+votedForMaxLen], voted)
	binary.LittleEndian.PutUint32(rec[0:4], crc32.Checksum(rec[4:], crcTable))

	slot := int64((seq - 1) % metaSlots)
	if _, err := fs.metaF.WriteAt(rec[:], slot*metaRecordSize); err != nil {
		return fmt.Errorf("filestore: write meta slot %d: %w", slot, err)
	}
	if err := fs.metaF.Sync(); err != nil {
		return fmt.Errorf("filestore: sync meta: %w", err)
	}

	fs.hsSeq = seq
	return nil
}

// readMeta reads the hard-state record. It returns the state, the sequence
// number of the slot it came from and whether the record was stored in the
// legacy checksum-free layout (in which case the caller should migrate it).
// Caller holds mu.
func (fs *FileStore) readMeta() (hs raft.HardState, seq uint64, legacy bool, err error) {
	buf := make([]byte, metaFileSize)
	n, err := fs.metaF.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return raft.HardState{}, 0, false, fmt.Errorf("filestore: read meta: %w", err)
	}
	buf = buf[:n]
	if n == 0 {
		return raft.HardState{}, 0, false, nil // nothing saved yet
	}

	var (
		bestHS  raft.HardState
		bestSeq uint64
		found   bool
	)
	for slot := 0; slot < metaSlots; slot++ {
		off := slot * metaRecordSize
		if off+metaRecordSize > n {
			break
		}
		hs, seq, ok := decodeMetaRecord(buf[off : off+metaRecordSize])
		if !ok {
			continue
		}
		if !found || seq > bestSeq {
			bestHS, bestSeq, found = hs, seq, true
		}
	}
	if found {
		return bestHS, bestSeq, false, nil
	}

	// No slot verified. A file written by an older release holds a single
	// checksum-free record of exactly legacyMetaSize bytes.
	if n == legacyMetaSize {
		if hs, ok := decodeLegacyMetaRecord(buf); ok {
			return hs, 0, true, nil
		}
	}

	return raft.HardState{}, 0, false, fmt.Errorf(
		"filestore: hard state is corrupt or truncated (%d bytes on disk, no slot verifies)", n)
}

// decodeMetaRecord verifies and decodes one hard-state slot.
func decodeMetaRecord(rec []byte) (raft.HardState, uint64, bool) {
	stored := binary.LittleEndian.Uint32(rec[0:4])
	if crc32.Checksum(rec[4:], crcTable) != stored {
		return raft.HardState{}, 0, false
	}
	seq := binary.LittleEndian.Uint64(rec[4:12])
	if seq == 0 {
		return raft.HardState{}, 0, false // slot never written
	}
	vlen := int(binary.LittleEndian.Uint16(rec[20:22]))
	if vlen > votedForMaxLen {
		return raft.HardState{}, 0, false
	}
	return raft.HardState{
		CurrentTerm: raft.Term(binary.LittleEndian.Uint64(rec[12:20])),
		VotedFor:    raft.NodeID(rec[22 : 22+vlen]),
	}, seq, true
}

// decodeLegacyMetaRecord decodes the pre-checksum hard-state layout. It rejects
// anything that does not look exactly like one: the length field must be in
// range and, because the writer always copied into a zeroed buffer, every byte
// past the node ID must be zero.
func decodeLegacyMetaRecord(rec []byte) (raft.HardState, bool) {
	if len(rec) != legacyMetaSize {
		return raft.HardState{}, false
	}
	vlen := int(binary.LittleEndian.Uint16(rec[8:10]))
	if vlen > votedForMaxLen {
		return raft.HardState{}, false
	}
	for _, b := range rec[10+vlen:] {
		if b != 0 {
			return raft.HardState{}, false
		}
	}
	return raft.HardState{
		CurrentTerm: raft.Term(binary.LittleEndian.Uint64(rec[0:8])),
		VotedFor:    raft.NodeID(rec[10 : 10+vlen]),
	}, true
}

// ---- Log -------------------------------------------------------------------

func (fs *FileStore) AppendLogEntries(_ context.Context, entries []raft.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.appendLocked(entries)
}

// appendLocked is AppendLogEntries with the store lock already held.
func (fs *FileStore) appendLocked(entries []raft.LogEntry) error {
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

func (fs *FileStore) FirstIndex() (raft.Index, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.segs) == 0 {
		return 0, nil
	}
	return fs.segs[0].firstID, nil
}

func (fs *FileStore) LastIndex() (raft.Index, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	active := fs.activeSeg()
	if active == nil {
		return 0, nil
	}
	return active.lastID, nil
}

// TruncateSuffix deletes all entries with index >= fromIndex and fsyncs.
//
// Crash safety depends on the order of the two on-disk steps. The segments
// that are discarded whole are unlinked and the directory is fsynced *before*
// the surviving boundary segment is shortened. A crash at any point therefore
// leaves either the original log (the truncation was never acknowledged, so
// keeping it is legal) or a log that has already lost the discarded suffix —
// never a shortened boundary segment still followed by segments that were
// supposed to be gone, which recovery would happily report as live entries.
func (fs *FileStore) TruncateSuffix(_ context.Context, fromIndex raft.Index) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if len(fs.segs) == 0 {
		return nil
	}
	last := fs.activeSeg()
	if last.lastID != 0 && fromIndex > last.lastID {
		return nil
	}
	first := fs.segs[0]
	if first.firstID != 0 && fromIndex < first.firstID {
		return fmt.Errorf("%w: TruncateSuffix(%d) < firstIndex(%d)",
			raft.ErrCompacted, fromIndex, first.firstID)
	}

	boundIdx := fs.findSegIdx(fromIndex)
	if boundIdx < 0 {
		return nil
	}

	// If nothing of the boundary segment survives it is discarded whole, which
	// makes it part of the same unlink step as the segments after it.
	dropFrom := boundIdx + 1
	if fromIndex <= fs.segs[boundIdx].firstID {
		dropFrom = boundIdx
	}

	drop := fs.segs[dropFrom:]
	fs.segs = fs.segs[:dropFrom]

	// Unlink from the tail inwards, one segment at a time, so that a crash can
	// only ever leave a contiguous run of segments behind. See unlinkSegment.
	for i := len(drop) - 1; i >= 0; i-- {
		if err := fs.unlinkSegment(drop[i]); err != nil {
			return err
		}
	}

	if dropFrom == boundIdx {
		return nil // boundary segment was discarded whole
	}

	// Partial truncation within the boundary segment.
	s := fs.segs[boundIdx]
	logOffset, err := s.readIdxOffset(fromIndex)
	if err != nil {
		return err
	}
	if err = s.logF.Truncate(logOffset); err != nil {
		return fmt.Errorf("filestore: truncate seg%05d log: %w", s.seqNum, err)
	}
	numKeep := int64(fromIndex - s.firstID)
	if err = s.idxF.Truncate(numKeep * idxEntrySize); err != nil {
		return fmt.Errorf("filestore: truncate seg%05d idx: %w", s.seqNum, err)
	}
	if err := s.sync(); err != nil {
		return err
	}
	s.lastID = fromIndex - 1
	s.logSize = logOffset
	return nil
}

func (fs *FileStore) TruncatePrefix(_ context.Context, toIndex raft.Index) error {
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
	// Detach them under the lock; unlink them after it is released.
	var toDelete []*segment
	for len(fs.segs) > 1 { // always keep at least one segment
		s := fs.segs[0]
		if s.lastID == 0 || s.lastID >= toIndex {
			break
		}
		toDelete = append(toDelete, s)
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

	// Unlink from the head outwards, one segment at a time, so that a crash can
	// only ever leave a contiguous run of segments behind. See unlinkSegment.
	for _, s := range toDelete {
		if err := fs.unlinkSegment(s); err != nil {
			return err
		}
	}

	if phase2seg == nil {
		return nil
	}
	return fs.rewriteBoundarySegment(phase2seg, toIndex)
}

// rewriteBoundarySegment performs TruncatePrefix Phase 2: the crash-safe
// rewrite of the segment that straddles toIndex, using tmp files and rename.
//
// The kept content is copied in bounded chunks through private read-only
// handles with the store lock released, so neither the peak memory nor the
// time the Raft loop can be blocked scales with the segment size. The lock is
// retaken to commit, and the segment is re-checked first: if anything moved
// while it was released the rewrite is abandoned rather than committed on top
// of stale content.
func (fs *FileStore) rewriteBoundarySegment(seg *segment, toIndex raft.Index) error {
	fs.mu.Lock()

	if len(fs.segs) == 0 || fs.segs[0] != seg || seg.firstID == 0 || seg.firstID >= toIndex {
		fs.mu.Unlock()
		return nil
	}

	numDrop := int64(toIndex - seg.firstID)
	numKeep := int64(seg.lastID-toIndex) + 1
	if numKeep <= 0 {
		// Nothing survives. Zeroing both files in place is safe because an
		// empty segment is indistinguishable from a freshly created one.
		defer fs.mu.Unlock()
		if err := seg.logF.Truncate(0); err != nil {
			return fmt.Errorf("filestore: truncate seg%05d log: %w", seg.seqNum, err)
		}
		if err := seg.idxF.Truncate(0); err != nil {
			return fmt.Errorf("filestore: truncate seg%05d idx: %w", seg.seqNum, err)
		}
		if err := seg.sync(); err != nil {
			return err
		}
		seg.firstID, seg.lastID, seg.logSize = 0, 0, 0
		return nil
	}

	firstKeptOffset, err := seg.readIdxOffsetAt(numDrop)
	if err != nil {
		fs.mu.Unlock()
		return fmt.Errorf("filestore: read first-kept offset in seg%05d: %w", seg.seqNum, err)
	}
	keepLogSize := seg.logSize - firstKeptOffset
	if keepLogSize < 0 {
		fs.mu.Unlock()
		return fmt.Errorf("filestore: seg%05d: kept region starts past end of log", seg.seqNum)
	}
	name := seg.name
	seqNum := seg.seqNum
	origFirst, origLast := seg.firstID, seg.lastID

	fs.mu.Unlock()

	logTmpPath := fs.logPath(name) + ".tmp"
	idxTmpPath := fs.idxPath(name) + ".tmp"
	cleanup := func() {
		_ = os.Remove(logTmpPath)
		_ = os.Remove(idxTmpPath)
	}

	// Private read handles: the originals are untouched until the rename, so
	// reading them without the store lock is safe.
	srcLog, err := os.Open(fs.logPath(name))
	if err != nil {
		return fmt.Errorf("filestore: open seg%05d log for rewrite: %w", seqNum, err)
	}
	defer func() { _ = srcLog.Close() }()
	srcIdx, err := os.Open(fs.idxPath(name))
	if err != nil {
		return fmt.Errorf("filestore: open seg%05d idx for rewrite: %w", seqNum, err)
	}
	defer func() { _ = srcIdx.Close() }()

	if err = writeTmpFile(logTmpPath, func(dst *os.File) error {
		return copyFileRange(dst, srcLog, firstKeptOffset, keepLogSize)
	}); err != nil {
		cleanup()
		return fmt.Errorf("filestore: write log.tmp in seg%05d: %w", seqNum, err)
	}
	if err = writeTmpFile(idxTmpPath, func(dst *os.File) error {
		return copyRebasedIdx(dst, srcIdx, numDrop, numKeep, firstKeptOffset)
	}); err != nil {
		cleanup()
		return fmt.Errorf("filestore: write idx.tmp in seg%05d: %w", seqNum, err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	if len(fs.segs) == 0 || fs.segs[0] != seg ||
		seg.firstID != origFirst || seg.lastID != origLast {
		// Another operation changed this segment while the lock was released.
		// Committing would publish stale content, so discard the rewrite; the
		// entries it would have dropped simply stay until the next compaction.
		cleanup()
		return nil
	}

	// Close the existing open handles before renaming over the files.
	if err = seg.close(); err != nil {
		cleanup()
		return fmt.Errorf("filestore: close seg%05d before rewrite: %w", seqNum, err)
	}

	// Atomic rename: log first (the commit point), then idx.
	// recoverPendingTruncations detects a crash between the two renames and
	// completes the idx rename on the next Open.
	if err = os.Rename(logTmpPath, fs.logPath(name)); err != nil {
		return fmt.Errorf("filestore: rename log.tmp in seg%05d: %w", seqNum, err)
	}
	if err = os.Rename(idxTmpPath, fs.idxPath(name)); err != nil {
		// idx.tmp still exists; recoverPendingTruncations will finish this.
		return fmt.Errorf("filestore: rename idx.tmp in seg%05d: %w", seqNum, err)
	}

	// Reopen the now-replaced files and update in-memory state.
	seg.logF, err = openFile(fs.logPath(name))
	if err != nil {
		return fmt.Errorf("filestore: reopen seg%05d log after truncate: %w", seqNum, err)
	}
	seg.idxF, err = openFile(fs.idxPath(name))
	if err != nil {
		_ = seg.logF.Close()
		seg.logF = nil
		return fmt.Errorf("filestore: reopen seg%05d idx after truncate: %w", seqNum, err)
	}

	seg.firstID = toIndex
	seg.logSize = keepLogSize

	return syncDir(fs.dir)
}

// writeTmpFile creates path, hands it to fill, then fsyncs and closes it.
// The file is left behind only on success; every error path removes it.
func writeTmpFile(path string, fill func(*os.File) error) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err = fill(f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// copyFileRange copies n bytes starting at srcOff from src to offset 0 of dst
// using a fixed-size buffer, so peak memory does not scale with n.
func copyFileRange(dst, src *os.File, srcOff, n int64) error {
	if n == 0 {
		return nil
	}
	buf := make([]byte, min(int64(copyChunkSize), n))
	for done := int64(0); done < n; {
		chunk := min(int64(len(buf)), n-done)
		if _, err := src.ReadAt(buf[:chunk], srcOff+done); err != nil {
			return err
		}
		if _, err := dst.WriteAt(buf[:chunk], done); err != nil {
			return err
		}
		done += chunk
	}
	return nil
}

// copyRebasedIdx copies numKeep index slots starting at slot numDrop, shifting
// every byte offset down by base so it is relative to the new byte 0.
func copyRebasedIdx(dst, src *os.File, numDrop, numKeep, base int64) error {
	const slotsPerChunk = copyChunkSize / idxEntrySize
	buf := make([]byte, min(int64(slotsPerChunk), numKeep)*idxEntrySize)
	for done := int64(0); done < numKeep; {
		slots := min(int64(len(buf)/idxEntrySize), numKeep-done)
		b := buf[:slots*idxEntrySize]
		if _, err := src.ReadAt(b, (numDrop+done)*idxEntrySize); err != nil {
			return err
		}
		for i := int64(0); i < slots; i++ {
			off := int64(binary.LittleEndian.Uint64(b[i*idxEntrySize:])) - base
			if off < 0 {
				return fmt.Errorf("filestore: index slot %d has offset before the kept region",
					numDrop+done+i)
			}
			binary.LittleEndian.PutUint64(b[i*idxEntrySize:], uint64(off))
		}
		if _, err := dst.WriteAt(b, done*idxEntrySize); err != nil {
			return err
		}
		done += slots
	}
	return nil
}

// ---- Snapshot --------------------------------------------------------------

// SaveSnapshot durably stores a snapshot and its metadata. The body is framed
// with its length and a CRC32 so that a later truncation or bit flip is
// detected on load.
//
// The store-wide lock is deliberately not held while the data is copied: the
// reader is supplied by the caller and, on a follower installing a snapshot,
// is fed one InstallSnapshot chunk at a time by the Raft loop. Blocking that
// loop on the storage lock would stall heartbeats on a leader and deadlock a
// follower. Concurrent snapshot writers are serialised by a dedicated lock,
// and the store-wide lock is taken only for the rename.
func (fs *FileStore) SaveSnapshot(_ context.Context, meta raft.SnapshotMeta, r io.Reader) error {
	fs.snapMu.Lock()
	defer fs.snapMu.Unlock()

	tmpPath := filepath.Join(fs.dir, "snap.tmp")
	snapPath := filepath.Join(fs.dir, "snap")

	f, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("filestore: create snap.tmp: %w", err)
	}
	// Never leave a partial snap.tmp behind: it would be mistaken for work in
	// progress and waste space until the next open.
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	var hdr [snapHeaderSize]byte
	copy(hdr[0:4], snapMagic[:])
	binary.LittleEndian.PutUint32(hdr[4:8], snapVersion)
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(meta.LastIncludedIndex))
	binary.LittleEndian.PutUint64(hdr[16:24], uint64(meta.LastIncludedTerm))
	if _, err = f.Write(hdr[:]); err != nil {
		return fail(fmt.Errorf("filestore: write snap header: %w", err))
	}

	h := crc32.New(crcTable)
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return fail(fmt.Errorf("filestore: write snap data: %w", err))
	}

	var trailer [snapTrailerSize]byte
	binary.LittleEndian.PutUint64(trailer[0:8], uint64(n))
	binary.LittleEndian.PutUint32(trailer[8:12], h.Sum32())
	if _, err = f.Write(trailer[:]); err != nil {
		return fail(fmt.Errorf("filestore: write snap trailer: %w", err))
	}

	if err = f.Sync(); err != nil {
		return fail(fmt.Errorf("filestore: sync snap.tmp: %w", err))
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("filestore: close snap.tmp: %w", err)
	}

	// Only the rename and the directory fsync touch state shared with the log.
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if err = os.Rename(tmpPath, snapPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("filestore: rename snap: %w", err)
	}
	return syncDir(fs.dir)
}

// LoadSnapshot returns the most recently saved snapshot. The returned reader
// verifies the body length and checksum as it is consumed and fails the final
// Read if either disagrees with what was stored.
func (fs *FileStore) LoadSnapshot(_ context.Context) (raft.SnapshotMeta, io.ReadCloser, error) {
	fs.mu.Lock()
	f, err := os.Open(filepath.Join(fs.dir, "snap"))
	fs.mu.Unlock()

	if errors.Is(err, os.ErrNotExist) {
		return raft.SnapshotMeta{}, nil, raft.ErrNoSnapshot
	}
	if err != nil {
		return raft.SnapshotMeta{}, nil, fmt.Errorf("filestore: open snap: %w", err)
	}

	meta, rc, err := openSnapshotReader(f)
	if err != nil {
		_ = f.Close()
		return raft.SnapshotMeta{}, nil, err
	}
	return meta, rc, nil
}

// openSnapshotReader parses the snapshot header and returns a reader over the
// body. Ownership of f passes to the returned reader on success.
func openSnapshotReader(f *os.File) (raft.SnapshotMeta, io.ReadCloser, error) {
	st, err := f.Stat()
	if err != nil {
		return raft.SnapshotMeta{}, nil, fmt.Errorf("filestore: stat snap: %w", err)
	}
	size := st.Size()

	var hdr [snapHeaderSize]byte
	if _, err = io.ReadFull(f, hdr[:legacySnapHeaderSize]); err != nil {
		return raft.SnapshotMeta{}, nil, fmt.Errorf("filestore: read snap header: %w", err)
	}

	if !bytes.Equal(hdr[0:4], snapMagic[:]) ||
		binary.LittleEndian.Uint32(hdr[4:8]) != snapVersion {
		// Pre-framing layout: a bare 16-byte header followed by the raw body.
		// There is nothing to verify, so hand the body over as-is.
		return raft.SnapshotMeta{
			LastIncludedIndex: raft.Index(binary.LittleEndian.Uint64(hdr[0:8])),
			LastIncludedTerm:  raft.Term(binary.LittleEndian.Uint64(hdr[8:16])),
		}, f, nil
	}

	if _, err = io.ReadFull(f, hdr[legacySnapHeaderSize:]); err != nil {
		return raft.SnapshotMeta{}, nil, fmt.Errorf("filestore: read snap header: %w", err)
	}
	meta := raft.SnapshotMeta{
		LastIncludedIndex: raft.Index(binary.LittleEndian.Uint64(hdr[8:16])),
		LastIncludedTerm:  raft.Term(binary.LittleEndian.Uint64(hdr[16:24])),
	}

	bodyLen := size - snapHeaderSize - snapTrailerSize
	if bodyLen < 0 {
		return raft.SnapshotMeta{}, nil, fmt.Errorf(
			"filestore: snapshot is truncated (%d bytes, smaller than its framing)", size)
	}

	var trailer [snapTrailerSize]byte
	if _, err = f.ReadAt(trailer[:], size-snapTrailerSize); err != nil {
		return raft.SnapshotMeta{}, nil, fmt.Errorf("filestore: read snap trailer: %w", err)
	}
	storedLen := int64(binary.LittleEndian.Uint64(trailer[0:8]))
	if storedLen != bodyLen {
		return raft.SnapshotMeta{}, nil, fmt.Errorf(
			"filestore: snapshot length mismatch (trailer says %d body bytes, file holds %d)",
			storedLen, bodyLen)
	}

	return meta, &snapshotReader{
		f:         f,
		body:      io.LimitReader(f, bodyLen),
		h:         crc32.New(crcTable),
		want:      binary.LittleEndian.Uint32(trailer[8:12]),
		remaining: bodyLen,
	}, nil
}

// snapshotReader streams a snapshot body while checksumming it, so verification
// costs no extra memory and no second pass over the data.
type snapshotReader struct {
	f         *os.File
	body      io.Reader
	h         hash.Hash32
	want      uint32
	remaining int64
	checked   bool
}

func (s *snapshotReader) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	if n > 0 {
		s.h.Write(p[:n])
		s.remaining -= int64(n)
	}
	if errors.Is(err, io.EOF) && !s.checked {
		s.checked = true
		if s.remaining != 0 {
			return n, fmt.Errorf("filestore: snapshot body is short by %d bytes", s.remaining)
		}
		if got := s.h.Sum32(); got != s.want {
			return n, fmt.Errorf(
				"filestore: snapshot checksum mismatch (stored %08x, computed %08x)", s.want, got)
		}
	}
	return n, err
}

func (s *snapshotReader) Close() error { return s.f.Close() }

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
		fs.metaF = nil
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
//
// Segments are ordered by index range, so a binary search applies — but an
// empty segment carries no range at all, and probing one yields no ordering
// information. Treating it as "search to the left" would drop every segment to
// its right, so the probe steps to the nearest non-empty neighbour instead.
func (fs *FileStore) findSegIdx(index raft.Index) int {
	lo, hi := 0, len(fs.segs)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if fs.segs[mid].firstID == 0 {
			j := mid + 1
			for j <= hi && fs.segs[j].firstID == 0 {
				j++
			}
			if j > hi {
				j = mid - 1
				for j >= lo && fs.segs[j].firstID == 0 {
					j--
				}
				if j < lo {
					return -1 // every segment in the window is empty
				}
			}
			mid = j
		}

		s := fs.segs[mid]
		switch {
		case s.lastID < index:
			lo = mid + 1
		case s.firstID > index:
			hi = mid - 1
		default:
			return mid
		}
	}
	return -1
}

// logPath returns the path of the log file for a segment base name.
func (fs *FileStore) logPath(name string) string {
	return filepath.Join(fs.dir, name+".log")
}

// idxPath returns the path of the index file for a segment base name.
func (fs *FileStore) idxPath(name string) string {
	return filepath.Join(fs.dir, name+".idx")
}

// parseSegSeq extracts the sequence number from a segment base name such as
// "seg-0000000042". Any number of digits is accepted so that files written
// with a narrower padding width by an older release remain readable.
func parseSegSeq(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, segNamePrefix)
	if !ok || digits == "" {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	seq, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return int(seq), true
}

// openFile opens the file at path for read/write, creating it if needed.
func openFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filestore: open %s: %w", path, err)
	}
	return f, nil
}

// mkdirAllSync creates dir and any missing parents, and makes each creation
// durable before returning.
//
// os.MkdirAll alone is not enough. A directory's name lives in its parent, so
// the parent is what has to be fsynced for the creation to survive a crash --
// exactly the rule syncDir exists to enforce for files, applied one level up.
// Without it the data directory itself is the unsynced name: a node can create
// it, write a vote and a run of log entries, fsync every one of those files
// and acknowledge them, then crash and come back to find the whole directory
// absent. It would rejoin with the same ID, an empty log and no record of its
// vote, which is the one thing Raft's safety argument assumes storage never
// does.
//
// The window is narrow -- first start on a fresh data directory -- but it is
// the start of a node's life, when it is most likely to be one of several
// coming up at once and casting the votes that elect the first leader.
func mkdirAllSync(dir string, perm os.FileMode) error {
	// Collect the missing suffix of the path, deepest first, stopping at the
	// first ancestor that already exists.
	var created []string
	for p := filepath.Clean(dir); ; {
		_, err := os.Stat(p)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("filestore: stat %s: %w", p, err)
		}
		created = append(created, p)
		parent := filepath.Dir(p)
		if parent == p {
			break // reached the root, which cannot itself be created
		}
		p = parent
	}

	if err := os.MkdirAll(dir, perm); err != nil {
		return fmt.Errorf("filestore: mkdir %s: %w", dir, err)
	}

	// Shallowest first, so that a directory's own name is durable before
	// anything inside it is. Each created directory is made durable by syncing
	// the directory that holds its name; for the shallowest that is the
	// pre-existing ancestor the loop above stopped at.
	for i := len(created) - 1; i >= 0; i-- {
		if err := syncDir(filepath.Dir(created[i])); err != nil {
			return err
		}
	}

	return nil
}

// syncDir fsyncs the directory itself to make create/rename/unlink operations
// durable. An fsync on a file descriptor does not cover its directory entry:
// without this, a crash can leave a segment that was written, fsynced and
// acknowledged to the Raft engine but whose name never reached disk, so every
// entry in it is silently gone on restart.
//
// It is a variable rather than a plain function purely so that tests can wrap
// it and record which directory was synced; nothing outside tests assigns to
// it, and the production path always runs fsyncDir.
var syncDir = fsyncDir

// fsyncDir is the real implementation behind syncDir.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("filestore: open dir for sync: %w", err)
	}
	err = d.Sync()
	_ = d.Close()
	if err != nil {
		return fmt.Errorf("filestore: sync dir: %w", err)
	}
	return nil
}

// encodeEntry serialises a LogEntry into (header, payload).
// Wire format: crc32:4 | index:8 | term:8 | dataLen:4 | data
func encodeEntry(e raft.LogEntry) (hdr [entryHeaderSize]byte, payload []byte) {
	binary.LittleEndian.PutUint64(hdr[4:12], uint64(e.Index))
	binary.LittleEndian.PutUint64(hdr[12:20], uint64(e.Term))
	binary.LittleEndian.PutUint32(hdr[20:24], uint32(len(e.Command)))

	h := crc32.New(crcTable)
	h.Write(hdr[4:])
	h.Write(e.Command)
	binary.LittleEndian.PutUint32(hdr[0:4], h.Sum32())

	return hdr, e.Command
}

// Compile-time check that the batch seam is implemented.
var _ raft.BatchWriter = (*FileStore)(nil)
