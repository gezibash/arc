// Package identity holds the identity axiom of ARC: every participant is a
// keypair first.
//
// A seed of 32 bytes derives an Ed25519 keypair. The seed is the identity:
// the same seed gives the same keypair on any device. The public key is the
// address of a participant.
package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"

	"filippo.io/edwards25519"
)

// SeedBytes is the length of a seed, a public key, and an X25519 key.
const SeedBytes = 32

// ErrInvalidPublicKey reports a public key with no matching curve point.
var ErrInvalidPublicKey = errors.New("identity: invalid public key")

// Identity is a keypair and the seed that derives it.
type Identity struct {
	Seed      []byte
	PublicKey []byte
	secretKey ed25519.PrivateKey
}

// Generate makes a new random identity.
func Generate() (*Identity, error) {
	seed := make([]byte, SeedBytes)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return FromSeed(seed)
}

// FromSeed derives an identity from an existing seed.
func FromSeed(seed []byte) (*Identity, error) {
	if len(seed) != SeedBytes {
		return nil, fmt.Errorf("identity: a seed holds %d bytes", SeedBytes)
	}

	secret := ed25519.NewKeyFromSeed(seed)
	public := make([]byte, SeedBytes)
	copy(public, secret.Public().(ed25519.PublicKey))

	return &Identity{Seed: append([]byte(nil), seed...), PublicKey: public, secretKey: secret}, nil
}

// EncodeSeed returns the seed in hexadecimal. It is the secret of the
// identity, and belongs only in the key store.
func (id *Identity) EncodeSeed() string {
	return hex.EncodeToString(id.Seed)
}

// FromSeedHex builds an identity from a seed in hex.
func FromSeedHex(value string) (*Identity, error) {
	seed, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("identity: a seed holds 64 hexadecimal characters")
	}
	return FromSeed(seed)
}

// Sign signs a message with the secret key of this identity.
func (id *Identity) Sign(message []byte) []byte {
	return ed25519.Sign(id.secretKey, message)
}

// Verify reports whether a signature matches a public key and a message.
func Verify(publicKey, message, signature []byte) bool {
	if len(publicKey) != SeedBytes {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(publicKey), message, signature)
}

// EncodePublicKey returns the public key as lowercase hexadecimal.
func (id *Identity) EncodePublicKey() string { return hex.EncodeToString(id.PublicKey) }

// ToX25519 derives the X25519 keypair of this identity for key exchange.
//
// This is the standard conversion that libsodium calls
// crypto_sign_ed25519_sk_to_curve25519: the secret is the clamped first half
// of SHA-512 of the seed. The public key equals PublicKeyToX25519 of the
// Ed25519 public key, so any sender computes it without this identity.
func (id *Identity) ToX25519() (publicKey, secretKey []byte, err error) {
	digest := sha512.Sum512(id.Seed)
	secret := make([]byte, SeedBytes)
	copy(secret, digest[:SeedBytes])
	secret[0] &= 248
	secret[31] &= 127
	secret[31] |= 64

	key, err := ecdh.X25519().NewPrivateKey(secret)
	if err != nil {
		return nil, nil, err
	}
	return key.PublicKey().Bytes(), secret, nil
}

// PublicKeyToX25519 computes the X25519 public key of an identity from its
// Ed25519 public key.
//
// This is the birational map from edwards25519 to Curve25519 of RFC 7748,
// u = (1 + y) / (1 - y). libsodium calls it
// crypto_sign_ed25519_pk_to_curve25519.
//
// A key of small order has no X25519 key here. Such a key gives a shared
// secret that an attacker predicts.
func PublicKeyToX25519(publicKey []byte) ([]byte, error) {
	if len(publicKey) != SeedBytes {
		return nil, ErrInvalidPublicKey
	}

	// SetBytes refuses a key that is not a canonical point.
	point, err := new(edwards25519.Point).SetBytes(publicKey)
	if err != nil {
		return nil, ErrInvalidPublicKey
	}

	// A point of small order becomes the identity when multiplied by the
	// cofactor.
	cleared := new(edwards25519.Point).MultByCofactor(point)
	if cleared.Equal(edwards25519.NewIdentityPoint()) == 1 {
		return nil, ErrInvalidPublicKey
	}

	return point.BytesMontgomery(), nil
}
