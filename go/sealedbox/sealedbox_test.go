package sealedbox_test

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/internal/vectors"
	"github.com/gezibash/arc/go/sealedbox"
)

// The Elixir implementation sealed this message. Go opens it, so the two
// implementations agree on the format and on the key derivation.
func TestOpensAnElixirSealedBox(t *testing.T) {
	want := vectors.Load(t).SealedBox

	id, err := identity.FromSeedHex(want.RecipientSeed)
	if err != nil {
		t.Fatal(err)
	}

	sealed, err := hex.DecodeString(want.Sealed)
	if err != nil {
		t.Fatal(err)
	}

	plaintext, err := sealedbox.Open(id, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(plaintext) != want.Plaintext {
		t.Errorf("plaintext = %q, want %q", plaintext, want.Plaintext)
	}
}

func TestRoundTrip(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	bodies := [][]byte{{}, []byte("x"), []byte("héllo ☃"), bytes.Repeat([]byte("a"), 1<<20)}
	for _, body := range bodies {
		sealed, err := sealedbox.SealTo(id.PublicKey, body)
		if err != nil {
			t.Fatalf("seal %d bytes: %v", len(body), err)
		}

		opened, err := sealedbox.Open(id, sealed)
		if err != nil {
			t.Fatalf("open %d bytes: %v", len(body), err)
		}
		if !bytes.Equal(opened, body) {
			t.Errorf("a body of %d bytes did not survive the round trip", len(body))
		}
	}
}

func TestSealsToAPublicKeyAlone(t *testing.T) {
	// A sender holds the Ed25519 public key and nothing else. The recipient
	// is offline, and publishes no key exchange record.
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	sealed, err := sealedbox.SealTo(id.PublicKey, []byte("offline"))
	if err != nil {
		t.Fatal(err)
	}

	opened, err := sealedbox.Open(id, sealed)
	if err != nil || string(opened) != "offline" {
		t.Fatalf("open = %q, %v", opened, err)
	}
}

func TestAnotherIdentityCannotOpen(t *testing.T) {
	bob, _ := identity.Generate()
	carol, _ := identity.Generate()

	sealed, err := sealedbox.SealTo(bob.PublicKey, []byte("for bob"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sealedbox.Open(carol, sealed); err == nil {
		t.Error("another identity opened the message")
	}
}

func TestRefusesAChangedOrShortMessage(t *testing.T) {
	id, _ := identity.Generate()
	sealed, err := sealedbox.SealTo(id.PublicKey, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}

	for index := range sealed {
		changed := append([]byte(nil), sealed...)
		changed[index] ^= 0x01
		if _, err := sealedbox.Open(id, changed); err == nil {
			t.Fatalf("a message with byte %d changed still opened", index)
		}
	}

	for _, short := range [][]byte{nil, {1}, sealed[:20]} {
		if _, err := sealedbox.Open(id, short); err == nil {
			t.Error("a short message opened")
		}
	}

	unknown := append([]byte(nil), sealed...)
	unknown[0] = 2
	if _, err := sealedbox.Open(id, unknown); err == nil {
		t.Error("another version opened")
	}
}

func TestTwoSealsOfOneBodyDiffer(t *testing.T) {
	id, _ := identity.Generate()

	first, _ := sealedbox.SealTo(id.PublicKey, []byte("same"))
	second, _ := sealedbox.SealTo(id.PublicKey, []byte("same"))

	if bytes.Equal(first, second) {
		t.Error("two seals of one body gave the same bytes")
	}
}

func TestRefusesAKeyOfSmallOrder(t *testing.T) {
	weak := make([]byte, 32)

	if _, err := sealedbox.SealTo(weak, []byte("x")); err == nil {
		t.Error("a key of small order sealed a message")
	}
	if err := errorText(sealedbox.SealTo(weak, []byte("x"))); !strings.Contains(err, "invalid public key") {
		t.Errorf("error = %q, want the invalid key error", err)
	}
}

func errorText(_ []byte, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
