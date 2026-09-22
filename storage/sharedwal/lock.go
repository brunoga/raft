package sharedwal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked is returned by Open when another process, or another WAL in this
// one, already holds the directory. Two logs on one directory would append
// interleaved records and neither would recover the other's.
var ErrLocked = errors.New("sharedwal: directory is already open by another process")

const lockFileName = "LOCK"

// acquireDirLock takes the directory's exclusive lock and returns the open
// file holding it. Closing the file releases it, which the kernel does on
// process exit as well.
func acquireDirLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sharedwal: open lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, dir)
		}
		return nil, fmt.Errorf("sharedwal: lock %s: %w", path, err)
	}
	return f, nil
}

func releaseDirLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return errors.Join(unlockFile(f), f.Close())
}
