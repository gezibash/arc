package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/internal/testrelay"
	"github.com/gezibash/arc/runtime/catalog"
)

func arc(t *testing.T, home string, args ...string) error {
	t.Helper()
	command := root()
	command.SetArgs(append([]string{"--home", home}, args...))
	return command.Execute()
}

// When the relay is down, install says so. It does not say that the
// provider announced nothing.
func TestInstallSaysWhenNoRelayAnswered(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "ws://" + l.Addr().String()
	l.Close()

	home := t.TempDir()
	if err := arc(t, home, "keys", "gen"); err != nil {
		t.Fatal(err)
	}
	if err := arc(t, home, "relay", "add", dead); err != nil {
		t.Fatal(err)
	}
	provider := keys.Generate().Public.Hex()
	err = arc(t, home, "install", provider, "journal", "--yes")
	if err == nil || !strings.Contains(err.Error(), "no relay answered") || !strings.Contains(err.Error(), dead) {
		t.Fatalf("install returned %v, want an error that names the relay %s", err, dead)
	}
}

// A new announcement replaces an older one of the same second, and one that
// a clock ahead signed later, which only the relay holds
// (docs/delivery/SPEC.md, section 4.7).
func TestAnnounceReplacesAnAnnouncementOfTheSameSecond(t *testing.T) {
	path := filepath.Join("..", "..", "apps", "files", "manifest.json")
	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(manifest, []byte(`"title": "Files"`), []byte(`"title": "Old files"`), 1)
	for _, ahead := range []time.Duration{0, 2 * time.Second} {
		url := testrelay.Start(t)
		home := t.TempDir()
		for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", url}} {
			if err := arc(t, home, args...); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
		}
		mine, err := listCitizens(home)
		if err != nil || len(mine) != 1 {
			t.Fatalf("the home holds %d identities (%v), want 1", len(mine), err)
		}
		id, err := loadIdentity(context.Background(), filepath.Join(citizensDir(home), mine[0].name))
		if err != nil {
			t.Fatal(err)
		}
		before, err := catalog.AnnounceManifest(context.Background(), id.signer, old, nostr.Timestamp(time.Now().Add(ahead).Unix()))
		if err != nil {
			t.Fatal(err)
		}
		if err := (relay.Relay{URL: url}).Send(context.Background(), before); err != nil {
			t.Fatal(err)
		}

		if err := arc(t, home, "announce", path); err != nil {
			t.Fatal(err)
		}
		batch, err := (relay.Relay{URL: url}).Fetch(context.Background(), nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{id.key.Public}})
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Events) != 1 || strings.Contains(batch.Events[0].Content, "Old files") || batch.Events[0].CreatedAt <= before.CreatedAt {
			t.Fatalf("%s ahead: the relay holds %d announcements, and the old one was signed at %d; want only the new one, signed later", ahead, len(batch.Events), before.CreatedAt)
		}
	}
}

// identityOf loads the one identity of a home.
func identityOf(t *testing.T, home string) signerIdentity {
	t.Helper()
	mine, err := listCitizens(home)
	if err != nil || len(mine) != 1 {
		t.Fatalf("the home holds %d identities (%v), want 1", len(mine), err)
	}
	id, err := loadIdentity(context.Background(), filepath.Join(citizensDir(home), mine[0].name))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// With no relay that answers, arc announce cannot know the newest
// announcement that the relays hold, so it refuses, and keeps nothing: one
// of the same second could tie with an older one (#422).
func TestAnnounceRefusesWhenNoRelayAnswers(t *testing.T) {
	url, kill := testrelay.StartKillable(t)
	home := t.TempDir()
	for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", url}} {
		if err := arc(t, home, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	kill()
	err := arc(t, home, "announce", filepath.Join("..", "..", "apps", "files", "manifest.json"))
	if err == nil || !strings.Contains(err.Error(), "no relay answered, so arc cannot know the newest announcement") {
		t.Fatalf("arc announce with no relay gave %v, want a refusal", err)
	}
}

// arc serve signs its announcement after the newest one that it knows,
// here one that the relay holds from 2 seconds ahead: else the relay keeps
// the older manifest (#422).
func TestServeAnnouncesAfterTheNewestAnnouncement(t *testing.T) {
	url := testrelay.Start(t)
	home := t.TempDir()
	for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", url}} {
		if err := arc(t, home, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	id := identityOf(t, home)
	manifest, err := os.ReadFile(filepath.Join("..", "..", "apps", "exec", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(manifest, []byte(`"title": "Exec"`), []byte(`"title": "Old exec"`), 1)
	before, err := catalog.AnnounceManifest(context.Background(), id.signer, old, nostr.Timestamp(time.Now().Add(2*time.Second).Unix()))
	if err != nil {
		t.Fatal(err)
	}
	if err := (relay.Relay{URL: url}).Send(context.Background(), before); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	out, done := serving(t, ctx, home)
	t.Cleanup(func() { cancel(); <-done })
	if !waitFor(out, "serves", 20*time.Second) {
		t.Fatalf("arc serve did not serve: %s", out.String())
	}
	batch, err := (relay.Relay{URL: url}).Fetch(context.Background(), nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{id.key.Public}})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 1 || batch.Events[0].ID == before.ID || batch.Events[0].CreatedAt <= before.CreatedAt {
		t.Fatalf("the relay holds %d announcements, the old one signed at %d; want only the one of arc serve, signed later", len(batch.Events), before.CreatedAt)
	}
}
