// Package writelock says whether a write of self data runs on this machine.
// A write saves the parts of a page before its draft, and the store saves
// each event on its own, so a repair must not take the parts of a write
// that still runs for parts that a stopped write left (#397).
package writelock

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// name is the lock file in the folder of the store.
const name = "writes.lock"

// Hold takes a shared lock for one write, until release. Writes in other
// processes share it. The system frees it when the process ends, also on
// SIGKILL, so a stopped write holds nothing.
func Hold(dir string) (release func(), err error) {
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}

// Busy says whether a write holds the lock now, in any process.
func Busy(dir string) (bool, error) {
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
