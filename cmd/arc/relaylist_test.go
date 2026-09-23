package main

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/catalog"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/mail"
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

// An indexer takes the public relay lists of a citizen, NIP-65 and NIP-17,
// and they name only the citizen's relays. The private NIP-37 list stays off
// the indexer.
func TestAnIndexerHoldsOnlyThePublicRelayLists(t *testing.T) {
	relayURL := testrelay.Start(t)
	indexer := testrelay.Start(t)
	home := t.TempDir()
	for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", indexer, "--index"}, {"relay", "add", relayURL}} {
		if err := arc(t, home, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}

	// The relay serves a private relay list only to its author, so ask as
	// the citizen.
	mine, err := listCitizens(home)
	if err != nil || len(mine) != 1 {
		t.Fatalf("the home holds %d identities (%v), want 1", len(mine), err)
	}
	id, err := loadIdentity(context.Background(), filepath.Join(citizensDir(home), mine[0].name))
	if err != nil {
		t.Fatal(err)
	}
	batch, err := (relay.Relay{URL: indexer, Signer: id.keyer}).Fetch(context.Background(), nostr.Filter{Kinds: []nostr.Kind{10002, 10050, 10013}, Authors: []nostr.PubKey{id.key.Public}})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[nostr.Kind]nostr.Event{}
	for _, e := range batch.Events {
		kinds[e.Kind] = e
	}
	if _, ok := kinds[10013]; ok || len(kinds) != 2 {
		t.Fatalf("the indexer holds kinds %v, want 10002 and 10050 only", slices.Collect(maps.Keys(kinds)))
	}
	if read, _ := relaylist.Read(kinds[10002]); !slices.Equal(read, []string{relayURL}) {
		t.Fatalf("the NIP-65 list on the indexer reads %v, want only %s", read, relayURL)
	}
	if err := arc(t, home, "relay", "add", indexer); err == nil {
		t.Fatal("an indexer was added again as a relay")
	}
}

// A caller and a provider share no relay, and the provider's NIP-65 list is
// only on an indexer that both use. The live call finds the list there, and
// goes to the provider's relay.
func TestALiveCallFindsTheProviderThroughAnIndexer(t *testing.T) {
	callerRelay := testrelay.Start(t)
	providerRelay := testrelay.Start(t)
	indexer := testrelay.Start(t)
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
	announce := func(at time.Time) nostr.Event {
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
	send(callerRelay, announce(time.Now().Add(-10*time.Minute)))
	send(providerRelay, announce(time.Now()))
	send(indexer, list)

	home := t.TempDir()
	for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", callerRelay}, {"relay", "add", indexer, "--index"}, {"install", provider.Public.Hex(), "--yes"}} {
		if err := arc(t, home, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}

	err = arc(t, home, "call", provider.Public.Hex(), `{"argv":["true"]}`, "--timeout", "3s")
	if err == nil || strings.Contains(err.Error(), "peer_offline") || !strings.Contains(err.Error(), providerRelay) {
		t.Fatalf("the call returned %v, want a failure on the provider's relay %s", err, providerRelay)
	}
}

// A recipient's NIP-17 list is only on an indexer. A direct message still
// reaches the recipient's inbox relay.
func TestAMessageFindsTheInboxThroughAnIndexer(t *testing.T) {
	senderRelay := testrelay.Start(t)
	inbox := testrelay.Start(t)
	indexer := testrelay.Start(t)
	recipient := keys.Generate()
	list, err := mail.RelayList(recipient, []string{inbox}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := (relay.Relay{URL: indexer}).Send(context.Background(), list); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", senderRelay}, {"relay", "add", indexer, "--index"}, {"message", "send", recipient.Public.Hex(), "hello"}} {
		if err := arc(t, home, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}

	batch, err := (relay.Relay{URL: inbox}).Fetch(context.Background(), nostr.Filter{Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {recipient.Public.Hex()}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("the inbox relay holds %d gift wraps for the recipient, want 1", len(batch.Events))
	}
}
