package storage

import (
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

func newKey(t *testing.T) ([32]byte, string) {
	t.Helper()
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	priv, _ := btcec.PrivKeyFromBytes(secret[:])
	return secret, hex.EncodeToString(schnorr.SerializePubKey(priv.PubKey()))
}

func key32(t *testing.T, text string) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(text)
	if err != nil || len(raw) != 32 {
		t.Fatalf("%q is not a key", text)
	}
	return [32]byte(raw)
}

// TestVerifyJoin checks that the storage citizen takes the signature of the
// citizen key only over this account key and this storage key.
func TestVerifyJoin(t *testing.T) {
	citizen, citizenHex := newKey(t)
	_, accountHex := newKey(t)
	_, storageHex := newKey(t)
	_, otherHex := newKey(t)
	sig, err := SignJoin(citizen, key32(t, accountHex), key32(t, storageHex))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyJoin(citizenHex, sig, accountHex, storageHex); err != nil {
		t.Fatalf("a good signature: %v", err)
	}
	raw, _ := hex.DecodeString(sig)
	raw[10] ^= 1
	for name, args := range map[string][4]string{
		"a changed signature":   {citizenHex, hex.EncodeToString(raw), accountHex, storageHex},
		"another storage key":   {citizenHex, sig, accountHex, otherHex},
		"another account key":   {citizenHex, sig, otherHex, storageHex},
		"another citizen key":   {otherHex, sig, accountHex, storageHex},
		"a signature not hex":   {citizenHex, "zz", accountHex, storageHex},
		"a storage key not set": {citizenHex, sig, accountHex, ""},
	} {
		if err := VerifyJoin(args[0], args[1], args[2], args[3]); err == nil {
			t.Errorf("%s: the signature verified", name)
		}
	}
}
