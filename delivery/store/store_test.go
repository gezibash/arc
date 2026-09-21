package store_test

import (
	"strconv"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func signed(t *testing.T, k keys.Key, kind nostr.Kind, at int64, content string, tags ...nostr.Tag) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: kind, CreatedAt: nostr.Timestamp(at), Content: content, Tags: tags}
	if err := event.Sign(k.Secret); err != nil {
		t.Fatal(err)
	}
	return event
}

func save(t *testing.T, s *store.Store, event nostr.Event) store.Result {
	t.Helper()
	result, err := s.Save(event)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStoresAndDeduplicates(t *testing.T) {
	s := open(t)
	event := signed(t, keys.Generate(), 3275, time.Now().Unix(), "hello")

	if r := save(t, s, event); r.Outcome != store.Stored {
		t.Fatalf("first save: %v %s", r.Outcome, r.Reason)
	}
	if r := save(t, s, event); r.Outcome != store.Duplicate {
		t.Errorf("second save: %v, want duplicate", r.Outcome)
	}
	if !s.Has(event.ID) {
		t.Error("the store does not hold the event")
	}
}

func TestRefusesAChangedEvent(t *testing.T) {
	s := open(t)
	event := signed(t, keys.Generate(), 3275, time.Now().Unix(), "the real text")

	changed := event
	changed.Content = "a forged text"
	if r := save(t, s, changed); r.Outcome != store.Refused {
		t.Errorf("a changed event gave %v, want refused", r.Outcome)
	}

	// A new ID over the changed fields still needs the author's signature.
	changed.SetID()
	if r := save(t, s, changed); r.Outcome != store.Refused {
		t.Errorf("a changed event with a new id gave %v, want refused", r.Outcome)
	}
	if s.Has(changed.ID) || s.Has(event.ID) {
		t.Error("the store kept a refused event")
	}
}

func TestReplaceableKeepsTheNewest(t *testing.T) {
	s := open(t)
	k := keys.Generate()
	d := nostr.Tag{"d", "page"}
	now := time.Now().Unix()

	older := signed(t, k, 30078, now-10, "old", d)
	newer := signed(t, k, 30078, now, "new", d)

	if r := save(t, s, newer); r.Outcome != store.Stored {
		t.Fatalf("newer: %v", r.Outcome)
	}
	if r := save(t, s, older); r.Outcome != store.Superseded {
		t.Errorf("older after newer: %v, want superseded", r.Outcome)
	}

	got := s.Query(nostr.Filter{Kinds: []nostr.Kind{30078}, Authors: []nostr.PubKey{k.Public}})
	if len(got) != 1 || got[0].ID != newer.ID {
		t.Errorf("the store holds %d heads, want only the newer one", len(got))
	}
}

func TestNeverKeepsEphemeralOrExpiredEvents(t *testing.T) {
	s := open(t)
	k := keys.Generate()
	now := time.Now().Unix()

	if r := save(t, s, signed(t, k, 21059, now, "live")); r.Outcome != store.Refused {
		t.Errorf("an ephemeral event gave %v, want refused", r.Outcome)
	}

	past := nostr.Tag{"expiration", strconv.FormatInt(now-1, 10)}
	if r := save(t, s, signed(t, k, 3275, now-100, "gone", past)); r.Outcome != store.Refused {
		t.Errorf("an expired event gave %v, want refused", r.Outcome)
	}
}

func TestADeletionRemovesAndKeepsOut(t *testing.T) {
	s := open(t)
	k, other := keys.Generate(), keys.Generate()

	note := signed(t, k, 1, 100, "a")
	theirs := signed(t, other, 1, 100, "b")
	draft := signed(t, k, 31234, 100, "v1", nostr.Tag{"d", "x"})
	save(t, s, note)
	save(t, s, theirs)
	save(t, s, draft)

	request := signed(t, k, nostr.KindDeletion, 200, "",
		nostr.Tag{"e", note.ID.Hex()}, nostr.Tag{"e", theirs.ID.Hex()},
		nostr.Tag{"a", "31234:" + k.Public.Hex() + ":x"})
	if got := save(t, s, request); got.Outcome != store.Stored {
		t.Fatalf("the request was %s", got.Outcome)
	}

	if s.Has(note.ID) || s.Has(draft.ID) {
		t.Error("a deleted event is still kept")
	}
	if !s.Has(theirs.ID) {
		t.Error("a request deleted the event of another author")
	}

	// The deleted events do not come back.
	if got := save(t, s, note); got.Outcome != store.Refused {
		t.Errorf("the deleted note came back: %s", got.Outcome)
	}
	if got := save(t, s, draft); got.Outcome != store.Refused {
		t.Errorf("the deleted draft came back: %s", got.Outcome)
	}
	// A newer version of the coordinate is not deleted.
	if got := save(t, s, signed(t, k, 31234, 201, "v2", nostr.Tag{"d", "x"})); got.Outcome != store.Stored {
		t.Errorf("a newer version was %s", got.Outcome)
	}
}

func TestATieKeepsTheLowerID(t *testing.T) {
	s := open(t)
	k := keys.Generate()
	a := signed(t, k, 30000, 100, "a", nostr.Tag{"d", "x"})
	b := signed(t, k, 30000, 100, "b", nostr.Tag{"d", "x"})
	save(t, s, a)
	save(t, s, b)
	got := s.Query(nostr.Filter{Kinds: []nostr.Kind{30000}})
	winner := a
	if string(b.ID[:]) < string(a.ID[:]) {
		winner = b
	}
	if len(got) != 1 || got[0].ID != winner.ID {
		t.Errorf("the store keeps %d versions", len(got))
	}
}
