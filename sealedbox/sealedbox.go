// Package sealedbox seals a message to a public key. No session is needed,
// and the recipient is offline while the sender works.
//
// Format, version 1:
//
//	<<1, ephemeral_public[32], ciphertext_and_tag>>
//
// Construction:
//
//  1. Generate an ephemeral X25519 keypair.
//  2. shared = ECDH(ephemeral_secret, recipient_public)
//  3. key = HKDF-SHA256(shared, salt: ephemeral_public || recipient_public,
//     info: "arc-sealed-v1", length: 32)
//  4. ChaCha20-Poly1305 with a zero nonce, and the ephemeral public key as
//     associated data. The key serves one message, so a fixed nonce is safe.
package sealedbox

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"

	"github.com/gezibash/arc/identity"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	version  = 1
	info     = "arc-sealed-v1"
	keyBytes = 32
)

// ErrOpenFailed reports a sealed message that did not open. A wrong key and a
// changed body give the same error, so the answer tells an attacker nothing.
var ErrOpenFailed = errors.New("sealedbox: open failed")

var nonce = make([]byte, chacha20poly1305.NonceSize)

// Seal seals plaintext to the X25519 public key of a recipient.
func Seal(recipientPublic, plaintext []byte) ([]byte, error) {
	recipient, err := ecdh.X25519().NewPublicKey(recipientPublic)
	if err != nil {
		return nil, ErrOpenFailed
	}

	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		return nil, ErrOpenFailed
	}

	aead, err := newAEAD(shared, ephemeral.PublicKey().Bytes(), recipientPublic)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, 1+identity.SeedBytes+len(plaintext)+aead.Overhead())
	out = append(out, version)
	out = append(out, ephemeral.PublicKey().Bytes()...)
	return aead.Seal(out, nonce, plaintext, ephemeral.PublicKey().Bytes()), nil
}

// SealTo seals plaintext to the Ed25519 public key of a recipient. The X25519
// key follows from that key, so a sender needs nothing else.
func SealTo(recipientEd25519, plaintext []byte) ([]byte, error) {
	recipient, err := identity.PublicKeyToX25519(recipientEd25519)
	if err != nil {
		return nil, err
	}
	return Seal(recipient, plaintext)
}

// Open opens a sealed message with the identity of the recipient.
func Open(id *identity.Identity, sealed []byte) ([]byte, error) {
	if len(sealed) < 1+identity.SeedBytes+chacha20poly1305.Overhead || sealed[0] != version {
		return nil, ErrOpenFailed
	}

	ephemeralPublic := sealed[1 : 1+identity.SeedBytes]
	body := sealed[1+identity.SeedBytes:]

	myPublic, mySecret, err := id.ToX25519()
	if err != nil {
		return nil, ErrOpenFailed
	}

	secret, err := ecdh.X25519().NewPrivateKey(mySecret)
	if err != nil {
		return nil, ErrOpenFailed
	}

	ephemeral, err := ecdh.X25519().NewPublicKey(ephemeralPublic)
	if err != nil {
		return nil, ErrOpenFailed
	}

	shared, err := secret.ECDH(ephemeral)
	if err != nil {
		return nil, ErrOpenFailed
	}

	aead, err := newAEAD(shared, ephemeralPublic, myPublic)
	if err != nil {
		return nil, ErrOpenFailed
	}

	plaintext, err := aead.Open(nil, nonce, body, ephemeralPublic)
	if err != nil {
		return nil, ErrOpenFailed
	}
	return plaintext, nil
}

func newAEAD(shared, ephemeralPublic, recipientPublic []byte) (interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	Overhead() int
}, error) {
	salt := make([]byte, 0, len(ephemeralPublic)+len(recipientPublic))
	salt = append(salt, ephemeralPublic...)
	salt = append(salt, recipientPublic...)

	key, err := hkdf.Key(sha256.New, shared, salt, info, keyBytes)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.New(key)
}
