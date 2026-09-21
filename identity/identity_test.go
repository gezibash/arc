package identity_test

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/internal/vectors"
)

func decode(t *testing.T, value string) []byte {
	t.Helper()
	out, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode %q: %v", value, err)
	}
	return out
}

// The Elixir implementation wrote these vectors. Every value here must match,
// or the two implementations disagree on the protocol.
func TestMatchesSharedVectors(t *testing.T) {
	file := vectors.Load(t)

	for _, want := range file.Identities {
		id, err := identity.FromSeedHex(want.Seed)
		if err != nil {
			t.Fatalf("seed %s: %v", want.Seed, err)
		}

		if got := id.EncodePublicKey(); got != want.PublicKey {
			t.Errorf("public key = %s, want %s", got, want.PublicKey)
		}

		xPublic, xSecret, err := id.ToX25519()
		if err != nil {
			t.Fatalf("to x25519: %v", err)
		}
		if got := hex.EncodeToString(xPublic); got != want.X25519Public {
			t.Errorf("x25519 public = %s, want %s", got, want.X25519Public)
		}
		if got := hex.EncodeToString(xSecret); got != want.X25519Secret {
			t.Errorf("x25519 secret = %s, want %s", got, want.X25519Secret)
		}

		derived, err := identity.PublicKeyToX25519(id.PublicKey)
		if err != nil {
			t.Fatalf("public key to x25519: %v", err)
		}
		if !bytes.Equal(derived, xPublic) {
			t.Errorf("the derived key and the secret side disagree for seed %s", want.Seed)
		}

		if got := id.Name(); got != want.Name {
			t.Errorf("name = %s, want %s", got, want.Name)
		}
		if got := id.ShortName(); got != want.ShortName {
			t.Errorf("short name = %s, want %s", got, want.ShortName)
		}

		if !identity.Verify(id.PublicKey, []byte("arc"), decode(t, want.SignatureOfArc)) {
			t.Errorf("the signature of the vector does not verify for seed %s", want.Seed)
		}
	}
}

func TestHKDFMatchesSharedVectors(t *testing.T) {
	for _, want := range vectors.Load(t).HKDF {
		got, err := hkdf.Key(sha256.New, decode(t, want.IKM), decode(t, want.Salt), want.Info, want.Length)
		if err != nil {
			t.Fatalf("hkdf: %v", err)
		}
		if hex.EncodeToString(got) != want.Output {
			t.Errorf("hkdf(%q) = %x, want %s", want.Info, got, want.Output)
		}
	}
}

func TestSignAndVerify(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	signature := id.Sign([]byte("hello arc"))
	if !identity.Verify(id.PublicKey, []byte("hello arc"), signature) {
		t.Error("a valid signature did not verify")
	}
	if identity.Verify(id.PublicKey, []byte("wrong"), signature) {
		t.Error("a signature verified for another message")
	}

	other, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if identity.Verify(other.PublicKey, []byte("hello arc"), signature) {
		t.Error("a signature verified for another key")
	}
}

func TestPublicKeyToX25519RefusesWeakKeys(t *testing.T) {
	prime := decode(t, "edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f")

	cases := map[string][]byte{
		"zero":            make([]byte, 32),
		"one":             append([]byte{1}, make([]byte, 31)...),
		"the field prime": prime,
		"short":           {1, 2, 3},
	}

	for name, key := range cases {
		if _, err := identity.PublicKeyToX25519(key); err == nil {
			t.Errorf("%s: a weak or invalid key returned a shared key", name)
		}
	}
}

func TestPublicKeyToX25519IgnoresTheSignBit(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	first, err := identity.PublicKeyToX25519(id.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	// The top bit of the last byte carries the sign of x. The map uses y.
	flipped := append([]byte(nil), id.PublicKey...)
	flipped[31] ^= 0x80

	second, err := identity.PublicKeyToX25519(flipped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the sign bit changed the X25519 key")
	}
}

func TestSeedRoundTrip(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	again, err := identity.FromSeed(id.Seed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(id.PublicKey, again.PublicKey) {
		t.Error("the same seed gave another public key")
	}

	if _, err := identity.FromSeed([]byte{1, 2, 3}); err == nil {
		t.Error("a short seed made an identity")
	}
}
