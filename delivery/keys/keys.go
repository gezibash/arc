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
	"fiatjaf.com/nostr/nip19"
	"fiatjaf.com/nostr/nip44"
	"fiatjaf.com/nostr/nip49"
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

// Errors of what a key file holds.
var (
	// ErrEncrypted says that the file holds an ncryptsec of NIP-49, which
	// needs its passphrase.
	ErrEncrypted = errors.New("keys: the key is encrypted with a passphrase")
	// ErrRemote says that the file names a remote signer of NIP-46.
	ErrRemote = errors.New("keys: the key is on a remote signer")
)

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

// Parse reads a secret key as 64 hex characters, or as an nsec of NIP-19.
func Parse(text string) (Key, error) {
	switch {
	case strings.HasPrefix(text, "ncryptsec1"):
		return Key{}, ErrEncrypted
	case strings.HasPrefix(text, "bunker://"):
		return Key{}, ErrRemote
	case strings.HasPrefix(text, "nsec1"):
		prefix, value, err := nip19.Decode(text)
		if err != nil || prefix != "nsec" {
			return Key{}, errors.New("keys: the nsec does not decode")
		}
		return FromSecret(value.(nostr.SecretKey)), nil
	}
	secret, err := nostr.SecretKeyFromHex(text)
	if err != nil {
		return Key{}, errors.New("keys: the file holds no valid secret key")
	}
	return FromSecret(secret), nil
}

// Load reads a key file that holds the secret key itself.
func Load(path string) (Key, error) {
	text, err := Read(path)
	if err != nil {
		return Key{}, err
	}
	k, err := Parse(text)
	if err != nil {
		return Key{}, fmt.Errorf("%w: %s", err, path)
	}
	return k, nil
}

// Encrypt seals a secret key with a passphrase, as an ncryptsec of NIP-49.
func Encrypt(k Key, passphrase string) (string, error) {
	if passphrase == "" {
		return "", errors.New("keys: the passphrase is empty")
	}
	return nip49.Encrypt(k.Secret, passphrase, 16, nip49.ClientDoesNotTrackThisData)
}

// Decrypt opens an ncryptsec with its passphrase.
func Decrypt(text, passphrase string) (Key, error) {
	secret, err := nip49.Decrypt(text, passphrase)
	if err != nil {
		return Key{}, errors.New("keys: the passphrase does not open the key")
	}
	return FromSecret(secret), nil
}

// Write replaces what a key file holds, at once: it writes a new file that
// only its owner can read, then renames it over the old one.
func Write(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp := path + ".new"
	if err := os.WriteFile(temp, []byte(text+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// Signer is a citizen who signs and seals: with the secret key on this
// machine, as Key does, or through a remote signer of NIP-46. The mail layer
// and calls need nothing more.
type Signer interface {
	PublicKey() nostr.PubKey
	// Sign sets the author, the ID and the signature of an event.
	Sign(event *nostr.Event) error
	// Encrypt and Decrypt use NIP-44 with another citizen.
	Encrypt(plaintext string, to nostr.PubKey) (string, error)
	Decrypt(ciphertext string, from nostr.PubKey) (string, error)
}

// PublicKey is the address of the citizen.
func (k Key) PublicKey() nostr.PubKey { return k.Public }

// Sign signs an event with the secret key.
func (k Key) Sign(event *nostr.Event) error { return event.Sign(k.Secret) }

// Encrypt seals text to another citizen with NIP-44.
func (k Key) Encrypt(plaintext string, to nostr.PubKey) (string, error) {
	conversation, err := nip44.GenerateConversationKey(to, k.Secret)
	if err != nil {
		return "", err
	}
	return nip44.Encrypt(plaintext, conversation)
}

// Decrypt opens text that another citizen sealed with NIP-44.
func (k Key) Decrypt(ciphertext string, from nostr.PubKey) (string, error) {
	conversation, err := nip44.GenerateConversationKey(from, k.Secret)
	if err != nil {
		return "", err
	}
	return nip44.Decrypt(ciphertext, conversation)
}
