// Package atomicfile provides private, atomic file replacement and a lock for
// short read-modify-write operations shared by separate ARC processes.
package atomicfile

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Lock holds a stable sidecar inode. The lock file is never removed: removing
// it while another process waits would create two independent locks.
func Lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); file.Close() }, nil
}

// Write replaces path after flushing a unique temporary file in its directory.
// Readers see the complete previous or new value. Callers performing a
// read-modify-write must hold Lock across the entire operation.
func Write(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
