// Package keyfile stores local identities with owner-only permissions.
package keyfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/sdk/atomicfile"
)

// Errors of a key file.
var (
	ErrExists   = errors.New("keys: a key file is already there")
	ErrNotFound = errors.New("keys: no key file; make one with arc keys gen")
	ErrOpenMode = errors.New("keys: the key file can be read by other users")
)

// Save writes the secret key to a new file that only its owner can read. It
// refuses to write over a key that is already there, because that would lose
// an identity.
func Save(path string, k keys.Key) error {
	if _, err := os.Stat(path); err == nil {
		return ErrExists
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}
		return err
	}
	defer file.Close()

	_, err = fmt.Fprintln(file, k.Secret.Hex())
	return err
}

// Read returns what a key file holds. It refuses a file that other users can
// read, because the secret key is then no longer secret.
func Read(path string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%w: %s", ErrOpenMode, path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// Load reads a key file that holds the secret key itself.
func Load(path string) (keys.Key, error) {
	text, err := Read(path)
	if err != nil {
		return keys.Key{}, err
	}
	k, err := keys.Parse(text)
	if err != nil {
		return keys.Key{}, fmt.Errorf("%w: %s", err, path)
	}
	return k, nil
}

// Write replaces what a key file holds, at once: it writes a new file that
// only its owner can read, then renames it over the old one.
func Write(path, text string) error { return atomicfile.Write(path, []byte(text+"\n"), 0600) }
