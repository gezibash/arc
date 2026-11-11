package files

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

// fileSync puts the bytes of a file on the disk, and dirSync the names in a
// directory, such as a rename. Tests replace them to check the order.
var (
	fileSync = func(f *os.File) error { return f.Sync() }
	dirSync  = syncDir
)

// syncDir puts the names in dir on the disk. Windows cannot sync a
// directory, and its rename needs none.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// WriteFile replaces path with data in one step: a reader sees the old or
// the new file, and the new file is on the disk before the rename.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := MkdirSynced(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = fileSync(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	// The file is in place; see Part.Finish.
	_ = dirSync(dir)
	return nil
}

// MkdirSynced makes dir and the directories above it that are missing,
// with mode 0700, and puts each new name on the disk in its parent.
func MkdirSynced(dir string) error {
	var missing []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for _, d := range slices.Backward(missing) {
		if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := dirSync(filepath.Dir(d)); err != nil {
			return err
		}
	}
	return nil
}

// MoveFile moves src to dst, and puts dst on the disk. On another file
// system it copies through dst.part, syncs it, renames it, and then removes
// src.
func MoveFile(src, dst string) error {
	if os.Rename(src, dst) == nil {
		_ = dirSync(filepath.Dir(dst))
		return nil
	}
	if err := copyThroughPart(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// copyThroughPart copies src to dst through dst.part, which is on the disk
// before the rename.
func copyThroughPart(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst+".part", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = fileSync(out)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(dst+".part", dst)
	}
	if err != nil {
		_ = os.Remove(dst + ".part")
		return err
	}
	_ = dirSync(filepath.Dir(dst))
	return nil
}
