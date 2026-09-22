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

	"github.com/brunoga/raft/v2"
)

// ---- Recorded commit index --------------------------------------------------
//
// raft.CommitRecorder, implemented against a file of its own.
//
// Nothing in normal operation reads this value back: a restarting node learns
// its commit index from its leader, as the paper says it should. It exists for
// the node that has no leader left to learn from -- the survivor of a
// permanent quorum loss, whose log splits into a part that is provably
// committed and a part that is a coin toss. Without this, the only proof on
// disk is the snapshot's last included index, so a node that snapshots every
// few thousand entries leaves a band that wide for an operator to guess about.
//
// It is therefore written the cheapest way that is still sound:
//
//   - One fixed-size record, rewritten in place. There is no history to keep;
//     only the newest value matters, and it only ever moves forward.
//   - No fsync. Losing the most recent value to a crash widens the band and
//     costs nothing else, whereas syncing here would put a disk write on a
//     path that runs every time the commit index moves, for a value almost no
//     node will ever read.
//   - A checksum. Without a sync the record can be torn across a crash, and a
//     half-written index is the one outcome that would be actively harmful:
//     it could read back larger than anything that ever committed. A failed
//     checksum reads as "nothing recorded", which is always safe.

const (
	commitFileName   = "commit"
	commitRecordSize = 12 // 4-byte CRC32 + 8-byte index
)

// SaveCommitIndex implements raft.CommitRecorder.
//
// The recorded value only ever moves forward. A recorded index is a claim that
// everything at or below it committed, and a claim like that does not stop
// being true: a caller asking to record less than is already there has nothing
// to add, and writing it would throw away proof a later recovery needs. The
// engine does not ask, but a store is the last place this can be enforced.
func (fs *FileStore) SaveCommitIndex(_ context.Context, index raft.Index) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if fs.commitF == nil {
		return errors.New("filestore: store is closed")
	}
	if index <= fs.commitIdx {
		return nil
	}

	var rec [commitRecordSize]byte
	binary.LittleEndian.PutUint64(rec[4:], uint64(index))
	binary.LittleEndian.PutUint32(rec[0:4], crc32.Checksum(rec[4:], crcTable))

	if _, err := fs.commitF.WriteAt(rec[:], 0); err != nil {
		return fmt.Errorf("filestore: write commit index: %w", err)
	}
	fs.commitIdx = index
	return nil
}

// LoadCommitIndex implements raft.CommitRecorder. It returns 0 when nothing
// was ever recorded and when the record did not survive, which are the same
// thing to a caller: no proof either way.
func (fs *FileStore) LoadCommitIndex(_ context.Context) (raft.Index, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if fs.commitF == nil {
		return 0, errors.New("filestore: store is closed")
	}

	var rec [commitRecordSize]byte
	n, err := fs.commitF.ReadAt(rec[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("filestore: read commit index: %w", err)
	}
	if n < commitRecordSize {
		return 0, nil // never written
	}
	if binary.LittleEndian.Uint32(rec[0:4]) != crc32.Checksum(rec[4:], crcTable) {
		// Torn by a crash between the write and the next sync of the file
		// system. Reading it as nothing is what keeps a half-written index
		// from claiming more than was ever committed.
		return 0, nil
	}
	idx := raft.Index(binary.LittleEndian.Uint64(rec[4:]))
	if idx > fs.commitIdx {
		fs.commitIdx = idx
	}
	return fs.commitIdx, nil
}

// openCommitFile opens (creating if needed) the file the commit index lives in.
func openCommitFile(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, commitFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filestore: open commit file: %w", err)
	}
	return f, nil
}
