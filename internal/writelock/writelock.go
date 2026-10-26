// Package writelock says whether a write of self data runs on this machine.
// A write saves the parts of a page before its draft, and the store saves
// each event on its own, so a repair must not take the parts of a write
// that still runs for parts that a stopped write left (#397).
package writelock

import (
	"errors"
	"fmt"
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
	file, err := open(dir)
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
	file, err := open(dir)
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

// open opens the lock file of dir, and makes it if it is missing. It never
// follows a symbolic link, so arc makes no file outside the store, and it
// takes only a plain file. A lock that arc cannot open fails closed: a
// write without it could lose its parts to a repair (#435).
func open(dir string) (*os.File, error) {
	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY|syscall.O_NOFOLLOW, 0o600)
	switch {
	case errors.Is(err, syscall.ELOOP):
		return nil, fmt.Errorf("the lock of writes %s is a symbolic link, and arc does not follow it: remove it, and arc makes it again", path)
	case errors.Is(err, os.ErrPermission):
		return nil, fmt.Errorf("arc cannot open the lock of writes %s: %w. Each write of self data takes it: give the folder %s and the file back to this user, with mode 700 and 600, or remove the file, and arc makes it again", path, err, dir)
	case err != nil:
		return nil, fmt.Errorf("arc cannot open the lock of writes %s: %w", path, err)
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("the lock of writes %s is not a plain file: remove it, and arc makes it again", path)
	}
	return file, nil
}
