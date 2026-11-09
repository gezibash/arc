package keys

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"

	"fiatjaf.com/nostr"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// The values in this file need the secret of the citizen key, and are not
// NIP-46 requests. A signer answers them as the requests of
// docs/devices/SPEC.md, section 3.7. A Key answers them itself.

// ReleaseDomain stands at the front of the bytes that a release publisher
// signs, before the schema number.
const ReleaseDomain = "ARC-RELEASE-CHANNEL-V"

// KeyedRoot derives the root of every keyed value of a citizen:
// HKDF-SHA256 of the secret key, with no salt and the info
// "arc-keyed-root-v1", 32 bytes.
func KeyedRoot(secret nostr.SecretKey) ([]byte, error) {
	return hkdf.Key(sha256.New, secret[:], nil, "arc-keyed-root-v1", 32)
}

// AccountKey derives the account key of a citizen for one storage citizen:
// HKDF-SHA256 of the citizen secret key, with no salt, and the info
// "arc-storage-account-v1" followed by the 32 bytes of the key of the
// storage citizen. Two storage citizens see two account keys that they
// cannot join.
func AccountKey(citizen nostr.SecretKey, storage nostr.PubKey) (Key, error) {
	raw, err := hkdf.Key(sha256.New, citizen[:], nil, "arc-storage-account-v1"+string(storage[:]), 32)
	if err != nil {
		return Key{}, err
	}
	var scalar btcec.ModNScalar
	if overflow := scalar.SetByteSlice(raw); overflow || scalar.IsZero() {
		return Key{}, errors.New("the account key for this storage citizen is not a valid secret key")
	}
	return FromSecret(nostr.SecretKey(raw)), nil
}

// JoinDigest is the message that the citizen key signs to show itself in
// a join: SHA-256("arc-storage-join-v1" || 0x00 || account key || storage
// key), with each key as its 32 bytes.
func JoinDigest(account, storage [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("arc-storage-join-v1"))
	h.Write([]byte{0})
	h.Write(account[:])
	h.Write(storage[:])
	var digest [32]byte
	h.Sum(digest[:0])
	return digest
}

// ReleaseDigest is the message that a release publisher signs:
// SHA-256(ReleaseDomain || schema in decimal || 0x00 || the canonical
// bytes of the unsigned channel document).
func ReleaseDigest(schema int64, encoded []byte) [32]byte {
	return sha256.Sum256(append([]byte(ReleaseDomain+strconv.FormatInt(schema, 10)+"\x00"), encoded...))
}

// signDigest makes a BIP-340 signature of digest, in hex.
func signDigest(secret nostr.SecretKey, digest [32]byte) (string, error) {
	priv, _ := btcec.PrivKeyFromBytes(secret[:])
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig.Serialize()), nil
}

// KeyedRoot is the keyed root of this key.
func (k Key) KeyedRoot(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return KeyedRoot(k.Secret)
}

// AccountKey is the account key of this key for one storage citizen.
func (k Key) AccountKey(ctx context.Context, storage nostr.PubKey) (Key, error) {
	if err := ctx.Err(); err != nil {
		return Key{}, err
	}
	return AccountKey(k.Secret, storage)
}

// SignJoin signs JoinDigest with this key, and returns the signature in hex.
func (k Key) SignJoin(ctx context.Context, account, storage nostr.PubKey) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return signDigest(k.Secret, JoinDigest(account, storage))
}

// SignRelease signs ReleaseDigest with this key, and returns the signature
// in hex.
func (k Key) SignRelease(ctx context.Context, schema int64, encoded []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return signDigest(k.Secret, ReleaseDigest(schema, encoded))
}
