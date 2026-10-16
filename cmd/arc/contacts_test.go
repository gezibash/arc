package main

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
)

// The proof of the filter through the normal CLI. An older sender, which
// makes no work, writes to bob. arc holds the message, says so on standard
// error, and lists it. arc contacts add moves it to the inbox.
func TestContactsReleaseHeldMail(t *testing.T) {
	rl, _, _, closeDB, err := newRelay(filepath.Join(t.TempDir(), "relay.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	server := httptest.NewServer(rl)
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	bob, bobHex := providerHome(t, url)
	bobKey, err := nostr.PubKeyFromHex(bobHex)
	if err != nil {
		t.Fatal(err)
	}

	older := keys.Generate()
	rumor := private.Rumor(older, 14, "an offer you cannot refuse", nostr.Tags{{"p", bobHex}}, time.Now())
	wrap, err := private.Wrap(context.Background(), older, bobKey, rumor, private.RelayForm, private.WrapKind, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := (relay.Relay{URL: url, Signer: older}).Send(context.Background(), wrap); err != nil {
		t.Fatal(err)
	}

	ok(t, bob, "", "sync")
	var stderr strings.Builder
	command := root()
	command.SetOut(&strings.Builder{})
	command.SetErr(&stderr)
	command.SetArgs([]string{"--home", bob, "message", "inbox", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "1 message from a stranger waits") {
		t.Errorf("the inbox said nothing of the held message on standard error: %q", stderr.String())
	}
	if inbox := ok(t, bob, "", "message", "inbox", "--json"); inbox != "" {
		t.Errorf("bob's inbox holds a message from a stranger: %s", inbox)
	}
	if held := ok(t, bob, "", "message", "held", "--json"); !strings.Contains(held, "an offer you cannot refuse") || !strings.Contains(held, older.Public.Hex()) {
		t.Errorf("arc message held: %s", held)
	}

	if out := ok(t, bob, "", "contacts", "add", older.Public.Hex()); !strings.Contains(out, "moved 1 held messages") {
		t.Errorf("arc contacts add: %s", out)
	}
	if inbox := ok(t, bob, "", "message", "inbox", "--json"); !strings.Contains(inbox, "an offer you cannot refuse") {
		t.Errorf("bob's inbox after contacts add: %s", inbox)
	}
	if held := ok(t, bob, "", "message", "held"); !strings.Contains(held, "no held messages") {
		t.Errorf("arc message held after contacts add: %s", held)
	}
	if list := ok(t, bob, "", "contacts"); !strings.Contains(list, older.Public.Hex()) {
		t.Errorf("arc contacts: %s", list)
	}
	ok(t, bob, "", "contacts", "rm", older.Public.Hex())
	if list := ok(t, bob, "", "contacts"); strings.Contains(list, older.Public.Hex()) {
		t.Errorf("arc contacts after rm: %s", list)
	}
}

// A message that this citizen sends puts the recipient on its contact list.
func TestASendAddsAContact(t *testing.T) {
	rl, _, _, closeDB, err := newRelay(filepath.Join(t.TempDir(), "relay.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	server := httptest.NewServer(rl)
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	alice, _ := providerHome(t, url)
	bob, bobKey := providerHome(t, url)

	ok(t, alice, "", "message", "send", bobKey, "hello bob")
	if list := ok(t, alice, "", "contacts"); !strings.Contains(list, bobKey) {
		t.Errorf("alice's contacts after the send: %s", list)
	}
	ok(t, bob, "", "sync")
	if inbox := ok(t, bob, "", "message", "inbox", "--json"); !strings.Contains(inbox, "hello bob") {
		t.Errorf("bob's inbox: %s", inbox)
	}
}
