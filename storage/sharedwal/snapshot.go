package sharedwal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/brunoga/raft"
)

// ---- Snapshot files -----------------------------------------------------------
//
// Snapshot data is large and replaced as a whole, so it lives outside the log
// in one file per group under snap/, named <group>-<index>. The file:
//
//	[4 magic][8 index][8 term][data...][8 data length][4 crc32c of data]
//
// The trailer lets a reader verify the body after streaming it, and a file
// whose trailer is missing or wrong is a snapshot that was never finished.

var snapMagic = [4]byte{'S', 'W', 'S', '1'}

const (
	snapHeaderSize  = 4 + 8 + 8
	snapTrailerSize = 8 + 4
)

func snapshotPath(dir string, group uint64, meta raft.SnapshotMeta) string {
	return filepath.Join(dir, "snap", fmt.Sprintf("%d-%d", group, meta.LastIncludedIndex))
}

// writeSnapshotFile writes the snapshot to a temporary file, syncs it, and
// renames it into place.
func writeSnapshotFile(dir string, group uint64, meta raft.SnapshotMeta, r io.Reader) error {
	final := snapshotPath(dir, group, meta)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("sharedwal: create snapshot: %w", err)
	}
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	var hdr [snapHeaderSize]byte
	copy(hdr[:4], snapMagic[:])
	binary.LittleEndian.PutUint64(hdr[4:12], uint64(meta.LastIncludedIndex))
	binary.LittleEndian.PutUint64(hdr[12:20], uint64(meta.LastIncludedTerm))
	if _, err := f.Write(hdr[:]); err != nil {
		return fail(fmt.Errorf("sharedwal: write snapshot header: %w", err))
	}
	h := crc32.New(crcTable)
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return fail(fmt.Errorf("sharedwal: write snapshot data: %w", err))
	}
	var trailer [snapTrailerSize]byte
	binary.LittleEndian.PutUint64(trailer[:8], uint64(n))
	binary.LittleEndian.PutUint32(trailer[8:], h.Sum32())
	if _, err := f.Write(trailer[:]); err != nil {
		return fail(fmt.Errorf("sharedwal: write snapshot trailer: %w", err))
	}
	if err := f.Sync(); err != nil {
		return fail(fmt.Errorf("sharedwal: sync snapshot: %w", err))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("sharedwal: close snapshot: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("sharedwal: rename snapshot: %w", err)
	}
	return syncDir(filepath.Join(dir, "snap"))
}

func removeSnapshotFile(dir string, group uint64, meta raft.SnapshotMeta) error {
	if err := os.Remove(snapshotPath(dir, group, meta)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sharedwal: remove old snapshot: %w", err)
	}
	return syncDir(filepath.Join(dir, "snap"))
}

// openSnapshotFile opens a snapshot for reading. The returned reader verifies
// the trailer as the last of the data is consumed.
func openSnapshotFile(dir string, group uint64, meta raft.SnapshotMeta) (io.ReadCloser, error) {
	f, err := os.Open(snapshotPath(dir, group, meta))
	if err != nil {
		return nil, fmt.Errorf("sharedwal: open snapshot: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sharedwal: stat snapshot: %w", err)
	}
	var hdr [snapHeaderSize]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sharedwal: read snapshot header: %w", err)
	}
	if [4]byte(hdr[:4]) != snapMagic {
		_ = f.Close()
		return nil, errors.New("sharedwal: snapshot file has the wrong magic")
	}
	got := raft.SnapshotMeta{
		LastIncludedIndex: raft.Index(binary.LittleEndian.Uint64(hdr[4:12])),
		LastIncludedTerm:  raft.Term(binary.LittleEndian.Uint64(hdr[12:20])),
	}
	if got != meta {
		_ = f.Close()
		return nil, fmt.Errorf("sharedwal: snapshot file describes %+v, log says %+v", got, meta)
	}
	dataLen := st.Size() - snapHeaderSize - snapTrailerSize
	if dataLen < 0 {
		_ = f.Close()
		return nil, errors.New("sharedwal: snapshot file is truncated")
	}
	return &snapshotReader{f: f, remaining: dataLen, h: crc32.New(crcTable)}, nil
}

// snapshotReader streams the data section and checks the trailer at the end.
type snapshotReader struct {
	f         *os.File
	remaining int64
	h         crc32Hash
	checked   bool
}

type crc32Hash interface {
	io.Writer
	Sum32() uint32
}

func (r *snapshotReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		if !r.checked {
			r.checked = true
			return 0, r.verify()
		}
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.f.Read(p)
	r.remaining -= int64(n)
	_, _ = r.h.Write(p[:n])
	if err == io.EOF && r.remaining > 0 {
		return n, io.ErrUnexpectedEOF
	}
	if err == io.EOF {
		err = nil
	}
	if err == nil && r.remaining == 0 {
		r.checked = true
		if verr := r.verify(); verr != nil {
			return n, verr
		}
		return n, io.EOF
	}
	return n, err
}

func (r *snapshotReader) verify() error {
	var trailer [snapTrailerSize]byte
	if _, err := io.ReadFull(r.f, trailer[:]); err != nil {
		return fmt.Errorf("sharedwal: read snapshot trailer: %w", err)
	}
	if binary.LittleEndian.Uint32(trailer[8:]) != r.h.Sum32() {
		return errors.New("sharedwal: snapshot data does not match its checksum")
	}
	return io.EOF
}

func (r *snapshotReader) Close() error { return r.f.Close() }

// pruneSnapshotFiles removes snapshot files that no group's recorded
// snapshot refers to: leftovers of a crash between writing a new snapshot
// and recording it, or between recording it and removing the old one.
// Caller holds no lock; runs during Open before the syncer starts.
func (w *WAL) pruneSnapshotFiles() error {
	entries, err := os.ReadDir(filepath.Join(w.dir, "snap"))
	if err != nil {
		return fmt.Errorf("sharedwal: read snap dir: %w", err)
	}
	removed := false
	for _, e := range entries {
		name := e.Name()
		keep := false
		if !strings.HasSuffix(name, ".tmp") {
			if group, index, ok := parseSnapshotName(name); ok {
				if g, exists := w.groups[group]; exists && g.hasSnap && g.snap.LastIncludedIndex == index {
					keep = true
				}
			}
		}
		if keep {
			continue
		}
		if err := os.Remove(filepath.Join(w.dir, "snap", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("sharedwal: remove stray snapshot %s: %w", name, err)
		}
		removed = true
	}
	if removed {
		return syncDir(filepath.Join(w.dir, "snap"))
	}
	return nil
}

func parseSnapshotName(name string) (group uint64, index raft.Index, ok bool) {
	parts := strings.SplitN(name, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	g, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	i, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return g, raft.Index(i), true
}
