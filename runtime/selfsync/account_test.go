package selfsync

import (
	"bytes"
	"encoding/hex"
	"testing"

	"fiatjaf.com/nostr"
)

// TestAccountKey checks the account key against HKDF-SHA256 computed with
// the Python standard library (hmac, hashlib), outside this code:
//
//	prk = HMAC-SHA256(32 zero bytes, 32 bytes of 0x01)
//	okm = HMAC-SHA256(prk, "arc-storage-account-v1" || 32 bytes of 0x02 || 0x01)
func TestAccountKey(t *testing.T) {
	var citizen nostr.SecretKey
	var storage, other nostr.PubKey
	for i := range 32 {
		citizen[i], storage[i], other[i] = 1, 2, 3
	}
	for _, c := range []struct {
		storage nostr.PubKey
		want    string
	}{
		{storage, "f871f62d2b36f14150d711786073120bf1167960b92377321fc817822e84c372"},
		{other, "d63addbf5ee74ef23b8091d1d282c9b3d74e17cdcfbe556b32d60cb29c195519"},
	} {
		k, err := AccountKey(citizen, c.storage)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := hex.DecodeString(c.want)
		if !bytes.Equal(k.Secret[:], want) {
			t.Fatalf("the account key for %x is %x, want %s", c.storage[:2], k.Secret[:], c.want)
		}
		if k.Public != k.Secret.Public() {
			t.Fatal("the public half does not follow from the secret")
		}
	}
}
