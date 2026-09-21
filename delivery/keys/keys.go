// Package keys holds the identity of a citizen: one secp256k1 key pair.
//
// The public key is the address of the citizen. The secret key never leaves
// the file that holds it, and nothing in this package prints it.
package keys

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/identity"
)

// Errors of a key file.
var (
	ErrExists   = errors.New("keys: a key file is already there")
	ErrNotFound = errors.New("keys: no key file; make one with arcn key new")
	ErrOpenMode = errors.New("keys: the key file can be read by other users")
)

// Key is one citizen.
type Key struct {
	Secret nostr.SecretKey
	Public nostr.PubKey
}

// Generate makes a new key pair.
func Generate() Key {
	secret := nostr.Generate()
	return Key{Secret: secret, Public: secret.Public()}
}

// FromSecret returns the key pair of a secret key.
func FromSecret(secret nostr.SecretKey) Key {
	return Key{Secret: secret, Public: secret.Public()}
}

// Name is the petname of the citizen. It follows from the public key, as it
// does for every ARC identity.
func (k Key) Name() string { return identity.Name(k.Public[:]) }

// Save writes the secret key to a new file that only its owner can read. It
// refuses to write over a key that is already there, because that would lose
// an identity.
func Save(path string, k Key) error {
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

// Load reads a key file. It refuses a file that other users can read, because
// the secret key is then no longer secret.
func Load(path string) (Key, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Key{}, fmt.Errorf("%w: %s", ErrOpenMode, path)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return Key{}, err
	}

	secret, err := nostr.SecretKeyFromHex(strings.TrimSpace(string(body)))
	if err != nil {
		return Key{}, fmt.Errorf("keys: %s holds no valid secret key", path)
	}
	return FromSecret(secret), nil
}
