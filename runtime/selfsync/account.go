package selfsync

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"

	"fiatjaf.com/nostr"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gezibash/arc/core/keys"
)

// AccountKey derives the account key of a citizen for one storage citizen:
// HKDF-SHA256 of the citizen secret key, with no salt, and the info
// "arc-storage-account-v1" followed by the 32 bytes of the key of the
// storage citizen. Only a machine with the citizen key derives it. Two
// storage citizens see two account keys that they cannot join.
func AccountKey(citizen nostr.SecretKey, storage nostr.PubKey) (keys.Key, error) {
	raw, err := hkdf.Key(sha256.New, citizen[:], nil, "arc-storage-account-v1"+string(storage[:]), 32)
	if err != nil {
		return keys.Key{}, err
	}
	var scalar btcec.ModNScalar
	if overflow := scalar.SetByteSlice(raw); overflow || scalar.IsZero() {
		return keys.Key{}, errors.New("the account key for this storage citizen is not a valid secret key")
	}
	return keys.FromSecret(nostr.SecretKey(raw)), nil
}
