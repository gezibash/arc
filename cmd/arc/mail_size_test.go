package main

import (
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/core/private"
)

// A message of the full mail size crosses the relay of arc relay serve, with
// its Bolt store and no flags. One byte more is refused before it is sent.
func TestAFullMessageCrossesTheRelay(t *testing.T) {
	rl, _, _, closeDB, err := newRelay(filepath.Join(t.TempDir(), "relay.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	server := httptest.NewServer(rl)
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http")

	alice, aliceKey := providerHome(t, url)
	bob, bobKey := providerHome(t, url)
	// Bob writes first, so Alice is no stranger to him, and her full
	// message needs no proof of work: this test is about the size.
	ok(t, bob, "", "message", "send", aliceKey, "hello")
	ok(t, alice, "", "sync")

	if out, err := run(t, alice, "", "message", "send", bobKey, strings.Repeat("x", private.MaxMailContent+1)); err == nil || !strings.Contains(err.Error(), "transfer") {
		t.Errorf("a message of 24 KiB + 1: %v\n%s", err, out)
	}

	full := strings.Repeat("x", private.MaxMailContent)
	ok(t, alice, "", "message", "send", bobKey, full)
	ok(t, bob, "", "sync")
	if inbox := ok(t, bob, "", "message", "inbox", "--json"); !strings.Contains(inbox, full) {
		t.Errorf("bob's inbox holds %d bytes, without the message of 24 KiB", len(inbox))
	}
}
