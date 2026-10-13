// Package bodies keeps the encrypted bodies of drafts as files in one
// directory, each named by the SHA-256 of its bytes. The local store keeps
// events only, so a large file of the files app lives here, beside it. The
// same layout serves a directory that a citizen carries (arc sync --dir).
package bodies

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gezibash/arc/sdk/atomicfile"
)

// Dir is one directory of bodies.
type Dir struct {
	Path string
}

func (d Dir) path(sum string) (string, error) {
	raw, err := hex.DecodeString(sum)
	if err != nil || len(raw) != sha256.Size || hex.EncodeToString(raw) != sum {
		return "", fmt.Errorf("%q is not the SHA-256 of a body", sum)
	}
	return filepath.Join(d.Path, sum), nil
}

// Put keeps data under sum. It refuses bytes that do not hash to sum. A
// body that the directory holds already stays.
func (d Dir) Put(sum string, data []byte) error {
	path, err := d.path(sum)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != sum {
		return errors.New("the body does not hash to its name")
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0o600)
}

// Get reads the body with the SHA-256 sum. It returns an error that wraps
// os.ErrNotExist when the directory does not hold it, and an error when
// the bytes on the disk do not hash to sum.
func (d Dir) Get(sum string) ([]byte, error) {
	path, err := d.path(sum)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != sum {
		return nil, fmt.Errorf("the body %s on the disk is damaged", sum)
	}
	return data, nil
}

// Has says whether the directory holds the body with the SHA-256 sum.
func (d Dir) Has(sum string) bool {
	path, err := d.path(sum)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Prune removes each body that keep does not name and that is older than
// before, and returns the count that it removed. The age protects a body
// that another program wrote and has not named in a wrap yet.
func (d Dir) Prune(keep map[string]bool, before time.Time) (int, error) {
	entries, err := os.ReadDir(d.Path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if _, err := d.path(name); err != nil || keep[name] {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(before) {
			continue
		}
		if err := os.Remove(filepath.Join(d.Path, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
