// Package keys holds the identity of a citizen: one secp256k1 key pair.
//
// The public key is the address of the citizen. Persistence belongs to a key adapter.
package keys

import (
	"context"
	"errors"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip19"
	"fiatjaf.com/nostr/nip44"
	"fiatjaf.com/nostr/nip49"
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
func (k Key) Name() string { return Name(k.Public[:]) }

// Errors of what a key file holds.
var (
	// ErrEncrypted says that the file holds an ncryptsec of NIP-49, which
	// needs its passphrase.
	ErrEncrypted = errors.New("keys: the key is encrypted with a passphrase")
	// ErrRemote says that the file names a remote signer of NIP-46.
	ErrRemote = errors.New("keys: the key is on a remote signer")
)

// Parse reads a secret key as 64 hex characters, or as an nsec of NIP-19.
func Parse(text string) (Key, error) {
	switch {
	case strings.HasPrefix(text, "ncryptsec1"):
		return Key{}, ErrEncrypted
	case strings.HasPrefix(text, "bunker://"):
		return Key{}, ErrRemote
	case strings.HasPrefix(text, "nsec1"):
		prefix, value, err := nip19.Decode(text)
		secret, ok := value.(nostr.SecretKey)
		if err != nil || prefix != "nsec" || !ok {
			return Key{}, errors.New("keys: the nsec does not decode")
		}
		return FromSecret(secret), nil
	}
	secret, err := nostr.SecretKeyFromHex(text)
	if err != nil {
		return Key{}, errors.New("keys: the file holds no valid secret key")
	}
	return FromSecret(secret), nil
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

// Signer is a citizen who signs and seals: with the secret key on this
// machine, as Key does, or through a remote signer of NIP-46. The mail layer
// and calls need nothing more.
type Signer interface {
	nostr.Keyer
	PublicKey() nostr.PubKey
}

// Identity caches a remote signer's public key without introducing another
// signing/encryption API. All effects use the context-aware Nostr contract.
type Identity struct {
	Public nostr.PubKey
	nostr.Keyer
}

func (i Identity) PublicKey() nostr.PubKey { return i.Public }

// PublicKey is the address of the citizen.
func (k Key) PublicKey() nostr.PubKey { return k.Public }

// GetPublicKey returns the cached public identity.
func (k Key) GetPublicKey(ctx context.Context) (nostr.PubKey, error) { return k.Public, ctx.Err() }

// SignEvent signs with the local secret key, respecting cancellation.
func (k Key) SignEvent(ctx context.Context, event *nostr.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return event.Sign(k.Secret)
}

// Encrypt seals text to another citizen with NIP-44.
func (k Key) Encrypt(ctx context.Context, plaintext string, to nostr.PubKey) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	conversation, err := nip44.GenerateConversationKey(to, k.Secret)
	if err != nil {
		return "", err
	}
	return nip44.Encrypt(plaintext, conversation)
}

// Decrypt opens text that another citizen sealed with NIP-44.
func (k Key) Decrypt(ctx context.Context, ciphertext string, from nostr.PubKey) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	conversation, err := nip44.GenerateConversationKey(from, k.Secret)
	if err != nil {
		return "", err
	}
	return nip44.Decrypt(ciphertext, conversation)
}

func (k Key) Nip04Encrypt(ctx context.Context, text string, to nostr.PubKey) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return keyer.NewPlainKeySigner(k.Secret).Nip04Encrypt(ctx, text, to)
}
func (k Key) Nip04Decrypt(ctx context.Context, text string, from nostr.PubKey) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return keyer.NewPlainKeySigner(k.Secret).Nip04Decrypt(ctx, text, from)
}
