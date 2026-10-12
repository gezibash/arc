package selfsync

import (
	"encoding/hex"
	"testing"
)

// TestNewKeys checks the key derivation of the storage spec, section 3.4.
// The expected values come from Python's hmac and hashlib (HKDF-SHA256,
// RFC 5869, empty salt), not from this code:
//
//	prk = hmac.new(b"\0"*32, ikm, sha256).digest()
//	okm = hmac.new(prk, info + b"\x01", sha256).hexdigest()
//
// Generation 0 must keep the keys of the blobs that devices sealed before
// the data roots, and the ID key must not change with the generation.
func TestNewKeys(t *testing.T) {
	keyed := make([]byte, 32)
	data := make([]byte, 32)
	for i := range keyed {
		keyed[i] = byte(i)
		data[i] = byte(0xff - i)
	}
	const (
		blobOfKeyed = "a9ccb7b3233383428b96f11e2140353f0a4e11e3c3265532c437085d3bc33423"
		blobOfData  = "e1656d46e2b372d6abb361143588401d6e2b673af18c05a33a0110276361c41f"
		idOfKeyed   = "87d45cd08c311e53399ff00c9093bacf54ec2f2881d23acc69b426a7672eb078"
	)
	zero, err := NewKeys(0, keyed, keyed)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(zero.Blob); got != blobOfKeyed {
		t.Fatalf("the blob key of generation 0 is %s, want %s", got, blobOfKeyed)
	}
	if got := hex.EncodeToString(zero.ID); got != idOfKeyed {
		t.Fatalf("the ID key of generation 0 is %s, want %s", got, idOfKeyed)
	}
	two, err := NewKeys(2, data, keyed)
	if err != nil {
		t.Fatal(err)
	}
	if two.Gen != 2 {
		t.Fatalf("the keys are of generation %d, want 2", two.Gen)
	}
	if got := hex.EncodeToString(two.Blob); got != blobOfData {
		t.Fatalf("the blob key of generation 2 is %s, want %s", got, blobOfData)
	}
	if got := hex.EncodeToString(two.ID); got != idOfKeyed {
		t.Fatalf("the ID key of generation 2 is %s, want the ID key of the keyed root %s", got, idOfKeyed)
	}
}
