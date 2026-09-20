package client_test

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/frame"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/packet"
	"github.com/gezibash/arc/session"
)

// A citizen of one relay reaches a citizen of another relay, where the two
// relays are of different implementations.
//
// To run this test: mise run go.federation
func TestAcrossTheFederation(t *testing.T) {
	address, relayKey := relay(t)

	providerKey := os.Getenv("ARC_PROVIDER_KEY")
	if providerKey == "" {
		t.Skip("no provider: run mise run go.federation")
	}

	peer, err := hex.DecodeString(providerKey)
	if err != nil {
		t.Fatal(err)
	}

	seed := os.Getenv("ARC_CALLER_SEED")
	if seed == "" {
		t.Skip("no caller: run mise run go.federation")
	}

	me, err := identity.FromSeedHex(seed)
	if err != nil {
		t.Fatal(err)
	}
	connection := dial(t, address, relayKey, me)

	// The catalog of the partner carries the citizen to this relay.
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)

	var found bool
	for time.Now().Before(deadline) {
		entries, err := connection.Resolve(ctx, providerKey)
		if err == nil && len(entries) == 1 {
			found = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !found {
		t.Fatal("the citizen of the other relay never appeared")
	}

	// A packet reaches it across the two relays.
	talk, err := session.Establish(me, peer)
	if err != nil {
		t.Fatal(err)
	}

	body, err := frame.Encode(frame.Request, frame.NewRequestID(), map[string]any{"path": "/"},
		[]byte("hello across the federation"))
	if err != nil {
		t.Fatal(err)
	}

	nonce, ciphertext, seq, err := talk.Encrypt(body)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := packet.Encode(me, peer, talk.ID, seq, nonce, ciphertext,
		packet.WithEphemeralKey(talk.EphemeralPublic))
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SendPacket(raw); err != nil {
		t.Fatal(err)
	}

	// The citizen of the other relay answers, and its answer travels back
	// over the same two relays.
	select {
	case arrived := <-connection.Packets():
		got, err := packet.Decode(arrived)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(got.Src) != string(peer) {
			t.Errorf("the answer came from %s", identity.Name(got.Src))
		}
		plaintext, err := talk.Decrypt(got.Nonce, got.Ciphertext)
		if err != nil {
			t.Fatalf("the answer did not decrypt: %v", err)
		}

		answer, err := frame.Decode(plaintext)
		if err != nil {
			t.Fatalf("the answer is not a frame: %v", err)
		}
		if !strings.Contains(string(answer.Body), "answers") {
			t.Errorf("the answer is %q", answer.Body)
		}

	case <-time.After(30 * time.Second):
		t.Fatal("no answer crossed the federation")
	}
}
