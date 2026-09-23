package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/catalog"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/relaylist"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport/relay"
)

// A citizen that adds a relay publishes its NIP-65 relay list there.
func TestRelayAddPublishesTheNIP65RelayList(t *testing.T) {
	url := testrelay.Start(t)
	home := t.TempDir()
	for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", url}} {
		if err := arc(t, home, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}

	batch, err := (relay.Relay{URL: url}).Fetch(context.Background(), nostr.Filter{Kinds: []nostr.Kind{relaylist.Kind}})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("the relay holds %d lists of kind 10002, want 1", len(batch.Events))
	}
	read, write := relaylist.Read(batch.Events[0])
	if len(read) != 1 || read[0] != url || len(write) != 1 || write[0] != url {
		t.Fatalf("the list reads %v and writes %v, want %s for both", read, write, url)
	}
}

// A caller and a provider share no relay. The provider's NIP-65 list, on the
// caller's relay, names the relay where the provider reads. The live call
// finds the provider's announcement there, and goes there
// (docs/delivery/SPEC.md, section 11.4).
func TestALiveCallGoesToTheReadRelaysOfTheProvider(t *testing.T) {
	callerRelay := testrelay.Start(t)
	providerRelay := testrelay.Start(t)
	manifest, err := os.ReadFile(filepath.Join("..", "exec-provider", "interface.json"))
	if err != nil {
		t.Fatal(err)
	}
	provider := keys.Generate()
	send := func(url string, event nostr.Event) {
		t.Helper()
		if err := (relay.Relay{URL: url}).Send(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	announcement := func(at time.Time) nostr.Event {
		t.Helper()
		event, err := catalog.AnnounceManifest(provider, manifest, nostr.Timestamp(at.Unix()))
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	list, err := relaylist.Make(provider, []string{providerRelay}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The caller's relay holds only an old announcement, enough to install.
	send(callerRelay, announcement(time.Now().Add(-10*time.Minute)))
	send(callerRelay, list)
	send(providerRelay, announcement(time.Now()))

	home := t.TempDir()
	for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", callerRelay}, {"install", provider.Public.Hex(), "--yes"}} {
		if err := arc(t, home, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}

	// Nobody serves, so each relay says that no one listened. Without the
	// provider's list, the call stops at once with peer_offline.
	err = arc(t, home, "call", provider.Public.Hex(), `{"argv":["true"]}`, "--timeout", "3s")
	if err == nil || strings.Contains(err.Error(), "peer_offline") || !strings.Contains(err.Error(), providerRelay) {
		t.Fatalf("the call returned %v, want a failure on the provider's relay %s", err, providerRelay)
	}
}
