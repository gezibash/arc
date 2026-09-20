package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// A release of ARC in Go is one program. The archive holds it, and the
// update replaces the running file with it.
//
// The new program runs once before it takes the place of the old one, and
// must report the version that the channel named. The replaced program stays
// beside it as <name>.previous, so an operator can put it back by hand.
const (
	// MaxArchiveEntries caps the files in one archive.
	MaxArchiveEntries = 1000
	// MaxBinaryBytes caps one program in an archive.
	MaxBinaryBytes = 256 * 1024 * 1024
	// ProbeLimit bounds the run that checks the new program.
	ProbeLimit = 30 * time.Second
)

// ErrNotInArchive reports an archive that does not hold the program.
var ErrNotInArchive = errors.New("release: the archive does not hold that program")

// Unpack reads one program out of a gzipped tar archive. The name may stand
// at the front of the archive, or under one directory.
func Unpack(archive []byte, name string) ([]byte, error) {
	unzipped, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("release: the archive is not gzip: %w", err)
	}
	defer unzipped.Close()

	reader := tar.NewReader(unzipped)

	for entries := 0; entries < MaxArchiveEntries; entries++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("release: the archive does not read: %w", err)
		}

		// A member never climbs out of the archive, and never links.
		clean := path.Clean(header.Name)
		if strings.HasPrefix(clean, "..") || path.IsAbs(clean) {
			return nil, errors.New("release: the archive holds a path that climbs out")
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if path.Base(clean) != name {
			continue
		}
		if header.Size > MaxBinaryBytes {
			return nil, errors.New("release: the program in the archive is too large")
		}

		out := make([]byte, header.Size)
		if _, err := io.ReadFull(reader, out); err != nil {
			return nil, err
		}
		return out, nil
	}
	return nil, ErrNotInArchive
}

// Replace puts a new program in the place of one that runs now.
//
// The new program is written beside the old one, run once to prove that it
// starts and reports the version that the channel named, and then renamed
// over it. The old program stays as <name>.previous.
func Replace(path string, program []byte, version string) error {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		target = path
	}

	candidate := target + ".new"
	if err := os.WriteFile(candidate, program, 0o755); err != nil {
		return fmt.Errorf("release: the new program did not save: %w", err)
	}

	if err := probe(candidate, version); err != nil {
		os.Remove(candidate)
		return err
	}

	previous := target + ".previous"
	os.Remove(previous)

	// The old program is kept, and the new one takes its place. A rename on
	// one filesystem never leaves a half written program behind.
	if err := os.Rename(target, previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		os.Remove(candidate)
		return fmt.Errorf("release: the running program did not move aside: %w", err)
	}
	if err := os.Rename(candidate, target); err != nil {
		os.Rename(previous, target)
		return fmt.Errorf("release: the new program did not take its place: %w", err)
	}
	return nil
}

// probe runs the new program once, and reads the version that it reports.
func probe(path, version string) error {
	command := exec.Command(path, "--version")

	out, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("release: the new program did not start: %w", err)
	}
	if version != "" && !strings.Contains(string(out), version) {
		return fmt.Errorf("release: the new program reports %q, and the channel names %s",
			strings.TrimSpace(string(out)), version)
	}
	return nil
}
