package wire_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/internal/wire"
)

func TestFrameRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	bodies := [][]byte{nil, []byte("x"), bytes.Repeat([]byte("a"), 100000)}

	for _, body := range bodies {
		if err := wire.WriteFrame(&buffer, body); err != nil {
			t.Fatal(err)
		}
	}

	for _, want := range bodies {
		got, err := wire.ReadFrame(&buffer, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("a frame of %d bytes came back as %d", len(want), len(got))
		}
	}
}

func TestReadFrameRefusesAFrameOverTheCap(t *testing.T) {
	var buffer bytes.Buffer
	if err := wire.WriteFrame(&buffer, bytes.Repeat([]byte("a"), 100)); err != nil {
		t.Fatal(err)
	}

	if _, err := wire.ReadFrame(&buffer, 50); err != wire.ErrFrameTooLarge && err == nil {
		t.Error("a frame over the cap was read")
	}
}

func TestTheHandshakeProvesTheIdentity(t *testing.T) {
	me, _ := identity.Generate()
	relay, _ := identity.Generate()

	challenge := make([]byte, wire.ChallengeBytes)
	if _, err := rand.Read(challenge); err != nil {
		t.Fatal(err)
	}

	hello, err := wire.RelayHello(relay.PublicKey, challenge)
	if err != nil {
		t.Fatal(err)
	}
	if len(hello) != wire.RelayHelloBytes {
		t.Fatalf("the relay hello is %d bytes", len(hello))
	}

	relayKey, gotChallenge, err := wire.ReadRelayHello(bytes.NewReader(hello))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(relayKey, relay.PublicKey) || !bytes.Equal(gotChallenge, challenge) {
		t.Fatal("the relay hello did not come back")
	}

	answer, err := wire.ClientHello(me, relayKey, gotChallenge)
	if err != nil {
		t.Fatal(err)
	}

	publicKey, ok := wire.VerifyClientHello(answer, relay.PublicKey, challenge)
	if !ok || !bytes.Equal(publicKey, me.PublicKey) {
		t.Fatal("the client hello did not verify")
	}

	// Another challenge, another relay, or a changed signature must fail.
	other, _ := identity.Generate()
	if _, ok := wire.VerifyClientHello(answer, other.PublicKey, challenge); ok {
		t.Error("the hello verified against another relay")
	}

	changed := append([]byte(nil), answer...)
	changed[40] ^= 0x01
	if _, ok := wire.VerifyClientHello(changed, relay.PublicKey, challenge); ok {
		t.Error("a changed hello verified")
	}
}

func TestRelayInfo(t *testing.T) {
	for _, want := range []uint32{0, 1, 8 * 1024 * 1024} {
		got, ok := wire.DecodeRelayInfo(wire.RelayInfo(want))
		if !ok || got != want {
			t.Errorf("relay info %d came back as %d, %v", want, got, ok)
		}
	}

	if _, ok := wire.DecodeRelayInfo([]byte("ARC_RELAY_INFO_V1")); ok {
		t.Error("a short relay info decoded")
	}
	if _, ok := wire.DecodeRelayInfo([]byte("something else")); ok {
		t.Error("another frame decoded as relay info")
	}
}

func TestDirectoryPrefix(t *testing.T) {
	payload, ok := wire.IsDirectory(wire.Directory([]byte(`{"type":"status"}`)))
	if !ok || string(payload) != `{"type":"status"}` {
		t.Errorf("payload = %s, %v", payload, ok)
	}

	if _, ok := wire.IsDirectory([]byte("ARC")); ok {
		t.Error("a short frame passed as directory control")
	}
}
