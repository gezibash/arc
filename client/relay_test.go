package client_test

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/announce"
	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/frame"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/packet"
	"github.com/gezibash/arc/session"
)

// relay reads the address and the key of a running Elixir relay. The test
// stops when they are not set.
//
// To run these tests: mise run go.conformance
func relay(t *testing.T) (string, []byte) {
	t.Helper()

	address := os.Getenv("ARC_RELAY_ADDRESS")
	key := os.Getenv("ARC_RELAY_KEY")
	if address == "" || key == "" {
		t.Skip("no relay: run mise run go.conformance")
	}

	publicKey, err := hex.DecodeString(key)
	if err != nil {
		t.Fatalf("ARC_RELAY_KEY is not hex: %v", err)
	}
	return address, publicKey
}

func dial(t *testing.T, address string, relayKey []byte, me *identity.Identity) *client.Client {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connection, err := client.Dial(ctx, address, client.Options{Identity: me, RelayPublicKey: relayKey})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

func TestElixirRelayAnswersStatusAndObserve(t *testing.T) {
	address, relayKey := relay(t)
	me, _ := identity.Generate()
	connection := dial(t, address, relayKey, me)

	ctx := context.Background()

	status, err := connection.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status["role"] != "relay" || status["state"] != "running" {
		t.Errorf("status = %v", status)
	}
	if status["public_key"] != hex.EncodeToString(relayKey) {
		t.Errorf("the relay names another key: %v", status["public_key"])
	}

	observed, err := connection.Observe(ctx)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if observed.Host == "" || observed.Port == 0 {
		t.Errorf("observed = %+v", observed)
	}
}

func TestElixirRelayRefusesAWrongPin(t *testing.T) {
	address, _ := relay(t)
	me, _ := identity.Generate()
	other, _ := identity.Generate()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.Dial(ctx, address, client.Options{Identity: me, RelayPublicKey: other.PublicKey}); err == nil {
		t.Error("the client accepted a relay that is not the pinned relay")
	}
}

func TestElixirRelayTakesAnAnnouncementAndFindsIt(t *testing.T) {
	address, relayKey := relay(t)
	me, _ := identity.Generate()
	connection := dial(t, address, relayKey, me)

	ctx := context.Background()
	record, err := announce.Create(me, []announce.Capability{{
		ID:      "exec",
		Kind:    "tool",
		Scheme:  "exec",
		Title:   "Run a command",
		Summary: "Runs one command on this machine.",
	}}, announce.Options{})
	if err != nil {
		t.Fatal(err)
	}

	if err := connection.Announce(ctx, record); err != nil {
		t.Fatalf("announce: %v", err)
	}

	entries, err := connection.Resolve(ctx, me.ShortName())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("resolve found %d entries, want 1", len(entries))
	}
	if entries[0]["public_key"] != me.EncodePublicKey() {
		t.Errorf("resolve found another citizen: %v", entries[0]["public_key"])
	}

	// The relay verified the signature of the record, so the announcement that
	// Go wrote is valid to the Elixir implementation.
	entry, err := announce.Verify(entries[0], time.Now())
	if err != nil {
		t.Fatalf("the relay returned a record that does not verify: %v", err)
	}
	if entry.Capabilities[0].ID != "exec" {
		t.Errorf("the entry holds %+v", entry.Capabilities)
	}

	page, err := connection.Search(ctx, "run a command", 10, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(page.Entries) == 0 {
		t.Fatal("search found nothing")
	}
	if page.Total < 1 {
		t.Errorf("total = %d", page.Total)
	}
}

func TestElixirRelayRefusesABadAnnouncement(t *testing.T) {
	address, relayKey := relay(t)
	me, _ := identity.Generate()
	other, _ := identity.Generate()
	connection := dial(t, address, relayKey, me)

	// The record is valid, but it belongs to another citizen. A relay takes an
	// announcement only from the identity that signed it.
	record, err := announce.Create(other, []announce.Capability{{ID: "exec"}}, announce.Options{})
	if err != nil {
		t.Fatal(err)
	}

	err = connection.Announce(context.Background(), record)
	if err == nil {
		t.Fatal("the relay took an announcement of another citizen")
	}
	if !strings.Contains(err.Error(), "invalid_announcement") {
		t.Errorf("error = %v, want invalid_announcement", err)
	}
}

// Two Go clients meet through the Elixir relay. The relay forwards the packet
// and never reads it.
func TestElixirRelayCarriesAPacketBetweenTwoCitizens(t *testing.T) {
	address, relayKey := relay(t)

	alice, _ := identity.Generate()
	bob, _ := identity.Generate()
	from := dial(t, address, relayKey, alice)
	to := dial(t, address, relayKey, bob)

	sending, err := session.Establish(alice, bob.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	body, err := frame.Encode(frame.Request, frame.NewRequestID(), map[string]any{
		"method": "GET",
		"path":   "/hello",
	}, []byte("over the relay"))
	if err != nil {
		t.Fatal(err)
	}

	nonce, ciphertext, seq, err := sending.Encrypt(body)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := packet.Encode(alice, bob.PublicKey, sending.ID, seq, nonce, ciphertext,
		packet.WithEphemeralKey(sending.EphemeralPublic))
	if err != nil {
		t.Fatal(err)
	}

	if err := from.SendPacket(raw); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case arrived := <-to.Packets():
		got, err := packet.Decode(arrived)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}

		joined, err := session.Accept(bob, got.Src, got.EphemeralPublic, got.SessionID)
		if err != nil {
			t.Fatal(err)
		}

		plaintext, err := joined.Decrypt(got.Nonce, got.Ciphertext)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}

		decoded, err := frame.Decode(plaintext)
		if err != nil {
			t.Fatalf("frame: %v", err)
		}
		if string(decoded.Body) != "over the relay" {
			t.Errorf("body = %q", decoded.Body)
		}
		if decoded.Meta["path"] != "/hello" {
			t.Errorf("meta = %v", decoded.Meta)
		}

	case <-time.After(10 * time.Second):
		t.Fatal("the packet did not arrive")
	case <-to.Done():
		t.Fatalf("the connection ended: %v", to.Err())
	}
}
