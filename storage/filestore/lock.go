package filestore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// lockFileName is the file whose lock represents the right to write to a store
// directory. It holds no data; only the lock on it matters.
const lockFileName = "LOCK"

// ErrLocked is returned by Open when another FileStore already holds the
// directory.
//
// Two stores on one directory do not share anything: each keeps its own
// segment list, its own hard-state sequence number, and its own idea of where
// the log ends. They overwrite each other's records, and the loser of a race
// is a log with entries from two writers interleaved, or a hard state that has
// gone backwards -- which is how a node votes twice in one term. Nothing
// reports it at the time; the damage is found on the next restart, or never.
//
// The usual cause is a second process started against a running node's data
// directory: a recovery tool, an inspection script, or a duplicate of the
// node itself from a supervisor that did not notice the first was alive.
var ErrLocked = errors.New("filestore: directory is already open by another process")

// acquireDirLock takes the directory's exclusive lock and returns the open
// file holding it. Releasing it means closing that file, which the kernel does
// on process exit as well, so a crashed node leaves nothing to clean up.
//
// The lock is advisory: it stops another FileStore, not another program
// writing into the directory, and on NFS it may not stop anything at all.
func acquireDirLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filestore: open lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, dir)
		}
		return nil, fmt.Errorf("filestore: lock %s: %w", path, err)
	}
	return f, nil
}

// releaseDirLock drops the lock. Closing the file is what releases it; the
// explicit unlock is for the case where something else in the process still
// holds a descriptor for the same file.
func releaseDirLock(f *os.File) error {
	if f == nil {
		return nil
	}
	unlockErr := unlockFile(f)
	closeErr := f.Close()
	return errors.Join(unlockErr, closeErr)
}
