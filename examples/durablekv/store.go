package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// ---- A state machine that keeps its own state on disk -----------------------
//
// The point of this example is the applied index. A state machine that stores
// its data durably has already done the work a restart would otherwise redo,
// and raft.DurableStateMachine is how it says so: report the index whose effect
// is on stable storage and the engine replays nothing at or below it.
//
// That promise is only keepable if the index is made durable *with* the data it
// describes. Writing the data and then recording the index separately leaves a
// window where a crash loses one and not the other, and the two failures are
// not symmetrical:
//
//   - Index behind data: entries are applied twice on restart. Survivable only
//     if every command is idempotent, which is not something a library can
//     assume.
//   - Index ahead of data: entries are never applied at all. The node is
//     permanently different from every other replica, and nothing in the log
//     explains it -- the engine was told that work was done.
//
// So every record here carries the index of the entry that produced it, in the
// same write. The durable applied index is whatever the last intact record
// says, which cannot disagree with the data by construction.

const (
	opPut byte = 1
	opDel byte = 2

	// recordHeader is the fixed part of a record: index, op, key length, value
	// length.
	recordHeader = 8 + 1 + 4 + 4

	// compactLiveFactor triggers a rewrite once the file holds this many times
	// more records than there are live keys. Without it an append-only file
	// grows with the number of writes rather than the size of the state.
	compactLiveFactor = 2
	// compactMinRecords keeps small stores from rewriting themselves
	// constantly, where the ratio is noisy and the saving is nothing.
	compactMinRecords = 1024
)

// kvStore is a durable key-value state machine.
//
// Reads are served from an in-memory map, which is a cache of the file rather
// than the state itself: the file is authoritative and the map is rebuilt from
// it on open.
type kvStore struct {
	dir  string
	path string

	mu      sync.RWMutex
	f       *os.File
	data    map[string]string
	applied uint64
	records int
}

// openStore opens or creates the store in dir, rebuilding its state from what
// is on disk.
func openStore(dir string) (*kvStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("durablekv: create %s: %w", dir, err)
	}
	s := &kvStore{
		dir:  dir,
		path: filepath.Join(dir, "state"),
		data: make(map[string]string),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load replays the file into memory and leaves it open for appending.
//
// A record that does not parse, or whose checksum does not match, ends the
// replay and the file is truncated there. That is the tail of a write that was
// interrupted by a crash, and it is exactly the write whose entry the engine
// will replay, because the applied index this store reports stops at the last
// intact record.
func (s *kvStore) load() error {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("durablekv: open state: %w", err)
	}

	r := bufio.NewReader(f)
	var offset int64
	for {
		rec, n, rerr := readRecord(r)
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) &&
				!errors.Is(rerr, errBadRecord) {
				_ = f.Close()
				return fmt.Errorf("durablekv: read state: %w", rerr)
			}
			break
		}
		s.apply(rec)
		s.records++
		offset += n
	}

	if err := f.Truncate(offset); err != nil {
		_ = f.Close()
		return fmt.Errorf("durablekv: truncate partial record: %w", err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return fmt.Errorf("durablekv: seek: %w", err)
	}
	s.f = f
	return nil
}

// record is one durable change.
type record struct {
	index uint64
	op    byte
	key   string
	value string
}

// apply puts a record into the in-memory map. Caller holds the write lock, or
// is in load before the store is shared.
func (s *kvStore) apply(rec record) {
	switch rec.op {
	case opPut:
		s.data[rec.key] = rec.value
	case opDel:
		delete(s.data, rec.key)
	}
	if rec.index > s.applied {
		s.applied = rec.index
	}
}

var errBadRecord = errors.New("durablekv: malformed record")

func appendRecord(buf []byte, rec record) []byte {
	payload := make([]byte, recordHeader+len(rec.key)+len(rec.value))
	binary.LittleEndian.PutUint64(payload[0:8], rec.index)
	payload[8] = rec.op
	binary.LittleEndian.PutUint32(payload[9:13], uint32(len(rec.key)))
	binary.LittleEndian.PutUint32(payload[13:17], uint32(len(rec.value)))
	copy(payload[recordHeader:], rec.key)
	copy(payload[recordHeader+len(rec.key):], rec.value)

	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(payload)))
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(payload))
	return append(buf, payload...)
}

// readRecord reads one framed record, returning how many bytes it consumed.
func readRecord(r io.Reader) (record, int64, error) {
	var frame [8]byte
	if _, err := io.ReadFull(r, frame[:]); err != nil {
		return record{}, 0, err
	}
	length := binary.LittleEndian.Uint32(frame[0:4])
	sum := binary.LittleEndian.Uint32(frame[4:8])
	if length < recordHeader || length > 64<<20 {
		return record{}, 0, errBadRecord
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return record{}, 0, err
	}
	if crc32.ChecksumIEEE(payload) != sum {
		return record{}, 0, errBadRecord
	}

	keyLen := int(binary.LittleEndian.Uint32(payload[9:13]))
	valLen := int(binary.LittleEndian.Uint32(payload[13:17]))
	if recordHeader+keyLen+valLen != int(length) {
		return record{}, 0, errBadRecord
	}
	return record{
		index: binary.LittleEndian.Uint64(payload[0:8]),
		op:    payload[8],
		key:   string(payload[recordHeader : recordHeader+keyLen]),
		value: string(payload[recordHeader+keyLen:]),
	}, int64(8 + length), nil
}

// commit writes records and makes them durable, then puts them into effect.
//
// One write and one sync for the whole batch, which is the reason
// raft.BatchApplier exists: the cost of durability is per-sync, not per-entry,
// so a batch of fifty entries costs what one costs.
func (s *kvStore) commit(recs []record) error {
	if len(recs) == 0 {
		return nil
	}
	var buf []byte
	for _, rec := range recs {
		buf = appendRecord(buf, rec)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.f.Write(buf); err != nil {
		return fmt.Errorf("durablekv: write records: %w", err)
	}
	// Durable before the in-memory map moves: the map is a cache of the file,
	// and a cache that is ahead of what it caches is how a reader sees a value
	// a restart would lose.
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("durablekv: sync: %w", err)
	}
	for _, rec := range recs {
		s.apply(rec)
	}
	s.records += len(recs)

	return s.maybeCompactLocked()
}

// maybeCompactLocked rewrites the file when it holds far more records than
// live keys. Caller holds the write lock.
func (s *kvStore) maybeCompactLocked() error {
	if s.records < compactMinRecords || s.records < compactLiveFactor*max(len(s.data), 1) {
		return nil
	}
	return s.rewriteLocked()
}

// rewriteLocked replaces the file with one record per live key, plus the
// applied index, and swaps it in atomically. Caller holds the write lock.
//
// The applied index has to survive the rewrite, and there may be no live key
// carrying it -- a store whose last operation was a delete has an applied index
// higher than any record it keeps. A marker record with no key preserves it.
func (s *kvStore) rewriteLocked() error {
	tmp := s.path + ".tmp"
	f, openErr := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if openErr != nil {
		return fmt.Errorf("durablekv: create temp state: %w", openErr)
	}

	var buf []byte
	for _, k := range slices.Sorted(maps.Keys(s.data)) {
		buf = appendRecord(buf, record{index: s.applied, op: opPut, key: k, value: s.data[k]})
	}
	if len(s.data) == 0 {
		buf = appendRecord(buf, record{index: s.applied, op: opDel})
	}

	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		return fmt.Errorf("durablekv: write temp state: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("durablekv: sync temp state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("durablekv: close temp state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("durablekv: rename temp state: %w", err)
	}
	// The rename is only durable once the directory is.
	if err := syncDir(s.dir); err != nil {
		return err
	}

	if err := s.f.Close(); err != nil {
		return fmt.Errorf("durablekv: close old state: %w", err)
	}
	reopened, reopenErr := os.OpenFile(s.path, os.O_RDWR|os.O_APPEND, 0o600)
	if reopenErr != nil {
		return fmt.Errorf("durablekv: reopen state: %w", reopenErr)
	}
	s.f = reopened
	s.records = max(len(s.data), 1)
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("durablekv: open dir: %w", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("durablekv: sync dir: %w", err)
	}
	return d.Close()
}

// get returns the value for key.
func (s *kvStore) get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

// all returns a copy of the whole state.
func (s *kvStore) all() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.data)
}

// Close releases the file.
func (s *kvStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
