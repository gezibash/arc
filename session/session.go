// Package session holds the encrypted session between two citizens.
//
// Establishment, version 2:
//
//  1. The initiator computes the X25519 public key of the responder from the
//     Ed25519 public key. See identity.PublicKeyToX25519.
//  2. The initiator generates one ephemeral X25519 keypair for each session,
//     and derives shared = ECDH(ephemeral_secret, responder_x25519_public).
//  3. key = HKDF-SHA256(shared, salt: ephemeral_public, info: "arc-session-v2")
//  4. Every packet header carries the ephemeral public key. The responder
//     derives the same key with ECDH(my_x25519_secret, ephemeral_public).
//  5. ChaCha20-Poly1305 encrypts every message under the session key.
//
// Loss of the long-term key of the initiator does not open past sessions.
// Loss of the long-term key of the responder does. An ephemeral key for the
// responder is version 3 work.
//
// Version 1 derived the key from both long-term keys and carried no ephemeral
// key. This package does not speak version 1.
package session

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"sync"

	"github.com/gezibash/arc/identity"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	info = "arc-session-v2"
	// Version is the session version that this package speaks.
	Version = 2
	// IDBytes is the length of a session id.
	IDBytes = 16
	// KeyBytes is the length of a session key.
	KeyBytes = 32
)

// ErrDecryptFailed reports a message that did not decrypt. A wrong key and a
// changed body give the same error.
var ErrDecryptFailed = errors.New("session: decrypt failed")

// ErrInvalidArgument reports a key or an id of the wrong length.
var ErrInvalidArgument = errors.New("session: invalid argument")

// Session holds the key, the id and the sequence counter of one session.
// Encrypt takes a lock, so several goroutines can share one session and each
// message still gets its own sequence number.
type Session struct {
	// PeerPublicKey is the Ed25519 public key of the other side.
	PeerPublicKey []byte
	// ID is the 16 random bytes that every packet header of this session carries.
	ID []byte
	// EphemeralPublic is the X25519 public key of the initiator.
	EphemeralPublic []byte
	// Identity is the local identity.
	Identity *identity.Identity

	key []byte

	mu  sync.Mutex
	seq uint64
}

// Establish starts a session as the initiator, from the Ed25519 public key of
// the peer.
func Establish(me *identity.Identity, peerPublicKey []byte) (*Session, error) {
	peerX, err := identity.PublicKeyToX25519(peerPublicKey)
	if err != nil {
		return nil, err
	}

	peer, err := ecdh.X25519().NewPublicKey(peerX)
	if err != nil {
		return nil, ErrInvalidArgument
	}

	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	shared, err := ephemeral.ECDH(peer)
	if err != nil {
		return nil, ErrInvalidArgument
	}

	id := make([]byte, IDBytes)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}

	return newSession(me, peerPublicKey, ephemeral.PublicKey().Bytes(), id, shared)
}

// Accept joins a session as the responder, from the ephemeral public key and
// the session id in the header of the first packet.
func Accept(me *identity.Identity, peerPublicKey, ephemeralPublic, id []byte) (*Session, error) {
	if len(peerPublicKey) != identity.SeedBytes || len(id) != IDBytes {
		return nil, ErrInvalidArgument
	}

	_, mySecret, err := me.ToX25519()
	if err != nil {
		return nil, err
	}

	secret, err := ecdh.X25519().NewPrivateKey(mySecret)
	if err != nil {
		return nil, ErrInvalidArgument
	}

	ephemeral, err := ecdh.X25519().NewPublicKey(ephemeralPublic)
	if err != nil {
		return nil, ErrInvalidArgument
	}

	shared, err := secret.ECDH(ephemeral)
	if err != nil {
		return nil, ErrInvalidArgument
	}

	return newSession(me, peerPublicKey, ephemeralPublic, id, shared)
}

func newSession(me *identity.Identity, peerPublicKey, ephemeralPublic, id, shared []byte) (*Session, error) {
	key, err := hkdf.Key(sha256.New, shared, ephemeralPublic, info, KeyBytes)
	if err != nil {
		return nil, err
	}

	return &Session{
		PeerPublicKey:   peerPublicKey,
		ID:              id,
		EphemeralPublic: ephemeralPublic,
		Identity:        me,
		key:             key,
	}, nil
}

// Key returns the session key. The relay never sees it. Tests and the packet
// layer read it.
func (s *Session) Key() []byte {
	return s.key
}

// Seq returns the sequence number of the next message.
func (s *Session) Seq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Encrypt encrypts one message. It returns the nonce, the ciphertext with its
// tag, and the sequence number of this message. The packet header carries the
// sequence number.
func (s *Session) Encrypt(plaintext []byte) (nonce, ciphertext []byte, seq uint64, err error) {
	aead, err := chacha20poly1305.New(s.key)
	if err != nil {
		return nil, nil, 0, err
	}

	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, 0, err
	}

	s.mu.Lock()
	seq = s.seq
	s.seq++
	s.mu.Unlock()

	return nonce, aead.Seal(nil, nonce, plaintext, nil), seq, nil
}

// Decrypt decrypts one message.
func (s *Session) Decrypt(nonce, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(s.key)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	if len(nonce) != aead.NonceSize() || len(ciphertext) < aead.Overhead() {
		return nil, ErrDecryptFailed
	}

	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	return plaintext, nil
}
