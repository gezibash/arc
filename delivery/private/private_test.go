package private_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/private"
)

func wrap(t *testing.T, from keys.Key, to nostr.PubKey, text string, form private.Form) nostr.Event {
	t.Helper()
	rumor := private.Rumor(from, 14, text, nostr.Tags{{"p", to.Hex()}}, time.Now())
	w, err := private.Wrap(from, to, rumor, form, private.WrapKind, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestOnlyTheRecipientOpensAWrap(t *testing.T) {
	alice, bob, carol := keys.Generate(), keys.Generate(), keys.Generate()
	w := wrap(t, alice, bob.Public, "hello bob", private.CourierForm)

	opened, err := private.Unwrap(bob, w)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Rumor.Content != "hello bob" || opened.Author() != alice.Public {
		t.Errorf("opened %q from %s", opened.Rumor.Content, opened.Author().Hex())
	}

	if _, err := private.Unwrap(carol, w); err == nil {
		t.Error("a third citizen opened the wrap")
	}
}

func TestTheCourierFormHidesTheRecipient(t *testing.T) {
	alice, bob := keys.Generate(), keys.Generate()
	w := wrap(t, alice, bob.Public, "hello", private.CourierForm)

	body, _ := json.Marshal(w)
	for _, secret := range []string{bob.Public.Hex(), alice.Public.Hex(), "hello"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("the courier form shows %s", secret)
		}
	}
	if w.Tags.Find("p") != nil {
		t.Error("the courier form carries a p tag")
	}
	if tag := w.Tags.Find("w"); tag == nil || tag[1] != private.RouteTag(bob.Public, time.Now()) {
		t.Error("the courier form does not carry today's route tag")
	}
	if w.PubKey == alice.Public {
		t.Error("the author signed the wrap, so the wrap names them")
	}
}

func TestTheRelayFormNamesTheRecipient(t *testing.T) {
	alice, bob := keys.Generate(), keys.Generate()
	w := wrap(t, alice, bob.Public, "hello", private.RelayForm)
	if tag := w.Tags.Find("p"); tag == nil || tag[1] != bob.Public.Hex() {
		t.Error("the relay form does not name the recipient")
	}
}

// A seal signed by Mallory, around a rumor that claims Alice wrote it.
func TestRefusesARumorThatClaimsAnotherAuthor(t *testing.T) {
	alice, bob, mallory := keys.Generate(), keys.Generate(), keys.Generate()

	forged := private.Rumor(alice, 14, "send money to mallory", nil, time.Now())
	conv, err := nip44.GenerateConversationKey(bob.Public, mallory.Secret)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := nip44.Encrypt(forged.String(), conv)
	if err != nil {
		t.Fatal(err)
	}
	seal := nostr.Event{Kind: private.SealKind, CreatedAt: nostr.Now(), Content: inner, Tags: nostr.Tags{}}
	if err := seal.Sign(mallory.Secret); err != nil {
		t.Fatal(err)
	}

	if _, err := private.OpenSeal(bob, seal); err == nil {
		t.Fatal("a rumor that names another author than its seal was opened")
	}
}

func TestRefusesAWrapThatHoldsNoSeal(t *testing.T) {
	bob := keys.Generate()
	if _, err := private.OpenSealJSON(`{"kind":1,"content":"x","tags":[]}`); err == nil {
		t.Error("a kind 1 event passed as a seal")
	}
	w := nostr.Event{Kind: 1, Content: "x"}
	if _, err := private.Unwrap(bob, w); err == nil {
		t.Error("a kind 1 event passed as a wrap")
	}
}

func TestRouteTagsChangeEachDay(t *testing.T) {
	bob := keys.Generate()
	day := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	today := private.RouteTag(bob.Public, day)
	if private.RouteTag(bob.Public, day.Add(11*time.Hour)) != today {
		t.Error("the route tag changed within one day")
	}
	if private.RouteTag(bob.Public, day.AddDate(0, 0, 1)) == today {
		t.Error("the route tag did not change the next day")
	}
	if len(today) != 32 {
		t.Errorf("the route tag has %d characters, want 32", len(today))
	}
}

func TestRouteTagsCoverTheLifeOfAWrap(t *testing.T) {
	bob := keys.Generate()
	now := time.Now()
	tags := private.RouteTags(bob.Public, now)

	for _, age := range []int{0, 1, 3, 6, 7} {
		tag := private.RouteTag(bob.Public, now.AddDate(0, 0, -age))
		found := false
		for _, t := range tags {
			found = found || t == tag
		}
		if !found {
			t.Errorf("a wrap %d days old is not in the recipient's tags", age)
		}
	}
}
