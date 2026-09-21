package session_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/internal/vectors"
	"github.com/gezibash/arc/session"
)

func decode(t *testing.T, value string) []byte {
	t.Helper()
	out, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode %q: %v", value, err)
	}
	return out
}

// The Elixir implementation established this session. Go joins it from the
// ephemeral key and the session id, and must derive the same key.
func TestAcceptMatchesTheSharedVector(t *testing.T) {
	want := vectors.Load(t).Session

	responder, err := identity.FromSeedHex(want.ResponderSeed)
	if err != nil {
		t.Fatal(err)
	}
	initiator, err := identity.FromSeedHex(want.InitiatorSeed)
	if err != nil {
		t.Fatal(err)
	}

	joined, err := session.Accept(
		responder,
		initiator.PublicKey,
		decode(t, want.EphemeralPublic),
		decode(t, want.SessionID),
	)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	if got := hex.EncodeToString(joined.Key()); got != want.SessionKey {
		t.Errorf("session key = %s, want %s", got, want.SessionKey)
	}
}

func TestInitiatorAndResponderAgree(t *testing.T) {
	alice, _ := identity.Generate()
	bob, _ := identity.Generate()

	sending, err := session.Establish(alice, bob.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	receiving, err := session.Accept(bob, alice.PublicKey, sending.EphemeralPublic, sending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sending.Key(), receiving.Key()) {
		t.Fatal("the two sides derived different keys")
	}

	nonce, ciphertext, seq, err := sending.Encrypt([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Errorf("the first message used sequence %d, want 0", seq)
	}

	plaintext, err := receiving.Decrypt(nonce, ciphertext)
	if err != nil || string(plaintext) != "hello" {
		t.Fatalf("decrypt = %q, %v", plaintext, err)
	}
}

func TestEachSessionDiffers(t *testing.T) {
	alice, _ := identity.Generate()
	bob, _ := identity.Generate()

	first, _ := session.Establish(alice, bob.PublicKey)
	second, _ := session.Establish(alice, bob.PublicKey)

	if bytes.Equal(first.Key(), second.Key()) {
		t.Error("two sessions share one key")
	}
	if bytes.Equal(first.ID, second.ID) {
		t.Error("two sessions share one id")
	}
	if bytes.Equal(first.EphemeralPublic, second.EphemeralPublic) {
		t.Error("two sessions share one ephemeral key")
	}
}

func TestTheSequenceCounterRises(t *testing.T) {
	alice, _ := identity.Generate()
	bob, _ := identity.Generate()
	sending, _ := session.Establish(alice, bob.PublicKey)

	for want := uint64(0); want < 5; want++ {
		_, _, seq, err := sending.Encrypt([]byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		if seq != want {
			t.Fatalf("sequence = %d, want %d", seq, want)
		}
	}
	if sending.Seq() != 5 {
		t.Errorf("the next sequence is %d, want 5", sending.Seq())
	}
}

func TestAnotherSessionCannotDecrypt(t *testing.T) {
	alice, _ := identity.Generate()
	bob, _ := identity.Generate()
	carol, _ := identity.Generate()

	sending, _ := session.Establish(alice, bob.PublicKey)
	other, _ := session.Establish(alice, carol.PublicKey)

	nonce, ciphertext, _, err := sending.Encrypt([]byte("for bob"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := other.Decrypt(nonce, ciphertext); err == nil {
		t.Error("another session decrypted the message")
	}
}

func TestDecryptRefusesChangedOrShortInput(t *testing.T) {
	alice, _ := identity.Generate()
	bob, _ := identity.Generate()
	sending, _ := session.Establish(alice, bob.PublicKey)

	nonce, ciphertext, _, err := sending.Encrypt([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}

	changed := append([]byte(nil), ciphertext...)
	changed[0] ^= 0x01
	if _, err := sending.Decrypt(nonce, changed); err == nil {
		t.Error("a changed ciphertext decrypted")
	}

	wrongNonce := append([]byte(nil), nonce...)
	wrongNonce[0] ^= 0x01
	if _, err := sending.Decrypt(wrongNonce, ciphertext); err == nil {
		t.Error("a changed nonce decrypted")
	}

	if _, err := sending.Decrypt(nonce[:4], ciphertext); err == nil {
		t.Error("a short nonce decrypted")
	}
	if _, err := sending.Decrypt(nonce, []byte{1, 2, 3}); err == nil {
		t.Error("a short ciphertext decrypted")
	}
}

func TestEstablishRefusesAnInvalidKey(t *testing.T) {
	alice, _ := identity.Generate()

	for name, key := range map[string][]byte{
		"zero":  make([]byte, 32),
		"short": {1, 2, 3},
	} {
		if _, err := session.Establish(alice, key); err == nil {
			t.Errorf("%s: an invalid key established a session", name)
		}
	}
}
