package catalog_test

import (
	"os"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/capability"
	"github.com/gezibash/arc/delivery/catalog"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/store"
)

func echoPackage(t *testing.T) map[string]any {
	t.Helper()
	pkg, err := capability.LoadFile(filepath.Join("..", "..", "provider", "host", "testdata", "echo", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func TestAnnounceAndRead(t *testing.T) {
	k := keys.Generate()
	event, err := catalog.Announce(k, echoPackage(t), nostr.Now())
	if err != nil {
		t.Fatal(err)
	}

	offer, err := catalog.Read(event)
	if err != nil {
		t.Fatal(err)
	}
	if offer.Provider != k.Public || offer.ID != "primary" || offer.Scheme != "echo" || offer.Method != "ECHO" {
		t.Errorf("offer = %+v", offer)
	}
}

func TestRefusesAChangedAnnouncement(t *testing.T) {
	event, err := catalog.Announce(keys.Generate(), echoPackage(t), nostr.Now())
	if err != nil {
		t.Fatal(err)
	}

	changed := event
	changed.Content = `{"capability":{"id":"primary","scheme":"evil"}}`
	if _, err := catalog.Read(changed); err == nil {
		t.Error("a changed manifest was read")
	}

	// A provider cannot name one capability in d and another in the manifest.
	k := keys.Generate()
	wrong := nostr.Event{Kind: catalog.Kind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", "other"}}, Content: event.Content}
	if err := wrong.Sign(k.Secret); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Read(wrong); err == nil {
		t.Error("an announcement whose d tag names another capability was read")
	}
}

func TestSearchAndFind(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	k := keys.Generate()
	event, err := catalog.Announce(k, echoPackage(t), nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(event); err != nil {
		t.Fatal(err)
	}

	if got := catalog.Search(s, "echo"); len(got) != 1 {
		t.Errorf("a search for echo found %d offers", len(got))
	}
	if got := catalog.Search(s, "nothing-like-it"); len(got) != 0 {
		t.Errorf("a search for nothing found %d offers", len(got))
	}
	if _, err := catalog.Find(s, k.Public, ""); err != nil {
		t.Errorf("find: %v", err)
	}
	if _, err := catalog.Find(s, keys.Generate().Public, ""); err == nil {
		t.Error("found an offer of a provider that announced nothing")
	}
}

func TestInstalls(t *testing.T) {
	k := keys.Generate()
	event, err := catalog.Announce(k, echoPackage(t), nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	offer, err := catalog.Read(event)
	if err != nil {
		t.Fatal(err)
	}

	installs := catalog.Installs{Path: filepath.Join(t.TempDir(), "installs.json")}
	if installs.Trusted(k.Public, "primary") {
		t.Error("an offer was trusted before its install")
	}
	if err := installs.Add(offer, ""); err != nil {
		t.Fatal(err)
	}
	if err := installs.Add(offer, ""); err != nil {
		t.Fatal(err)
	}
	list, _ := installs.List()
	if len(list) != 1 || !installs.Trusted(k.Public, "primary") {
		t.Errorf("installs = %+v", list)
	}

	key, id, err := installs.Resolve(offer.Name())
	if err != nil || key != k.Public || id != "primary" {
		t.Errorf("resolve by name: %v %s %v", key, id, err)
	}
}

func TestAnnounceAManifestOfVersionOne(t *testing.T) {
	k := keys.Generate()
	data, err := os.ReadFile(filepath.Join("..", "..", "cmd", "exec-provider", "interface.json"))
	if err != nil {
		t.Fatal(err)
	}
	event, err := catalog.AnnounceManifest(k, data, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	offer, err := catalog.Read(event)
	if err != nil {
		t.Fatal(err)
	}
	if offer.Manifest == nil || offer.ID != "exec" || offer.Method != "EXEC" || offer.Path != "/" || len(offer.Manifest.Commands) != 3 {
		t.Fatalf("offer = %+v", offer)
	}

	installs := catalog.Installs{Path: filepath.Join(t.TempDir(), "installs.json")}
	if err := installs.Add(offer, "exec"); err != nil {
		t.Fatal(err)
	}
	if e, ok := installs.Named("exec"); !ok || e.Provider != k.Public.Hex() {
		t.Errorf("named: %+v %v", e, ok)
	}
	if key, id, err := installs.Resolve("exec"); err != nil || key != k.Public || id != "exec" {
		t.Errorf("resolve: %v %s %v", key, id, err)
	}

	// Another provider cannot take the same name.
	other, _ := catalog.AnnounceManifest(keys.Generate(), data, nostr.Now())
	offer2, _ := catalog.Read(other)
	if err := installs.Add(offer2, "exec"); err == nil {
		t.Error("two providers run as one name")
	}
	if err := installs.Add(offer2, "exec2"); err != nil {
		t.Error(err)
	}
}

func TestRefusesAManifestOfALaterVersion(t *testing.T) {
	k := keys.Generate()
	event := nostr.Event{Kind: catalog.Kind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", "x"}},
		Content: `{"interface": 2, "id": "x"}`}
	event.Sign(k.Secret)
	if _, err := catalog.Read(event); err == nil {
		t.Error("a manifest of version 2 was read")
	}
}
