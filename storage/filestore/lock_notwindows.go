//go:build !windows

package filestore

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive flock, failing rather than waiting if another
// process holds it.
//
// flock is held per open file description, so two FileStores in one process
// contend with each other exactly as two processes do. That is what makes a
// recovery tool run against a live node's directory fail even when the node is
// in the same binary.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
