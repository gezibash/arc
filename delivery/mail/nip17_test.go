package mail_test

import (
	"context"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip17"
	"fiatjaf.com/nostr/nip59"
	"github.com/gezibash/arc/delivery/mail"
	"github.com/gezibash/arc/delivery/private"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport/relay"
)

// The proof of phase 3, part three: an ARC direct message opens in a NIP-17
// client that ARC did not write. The library's own NIP-17 code stands in for
// that client.
func TestAnARCMessageOpensInANIP17Client(t *testing.T) {
	ctx := context.Background()
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t)

	alice.send(t, bob, "hello from arc")

	batch, err := r.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {bob.key.Public.Hex()}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) == 0 {
		t.Fatal("no wrap for bob reached the relay")
	}

	client := keyer.NewPlainKeySigner(bob.key.Secret)
	rumor, err := nip59.GiftUnwrap(batch.Events[0], func(other nostr.PubKey, ciphertext string) (string, error) {
		return client.Decrypt(ctx, ciphertext, other)
	})
	if err != nil {
		t.Fatalf("the NIP-17 client could not open the message: %v", err)
	}
	if rumor.Kind != nostr.KindDirectMessage || rumor.Content != "hello from arc" || rumor.PubKey != alice.key.Public {
		t.Errorf("the NIP-17 client read kind %d %q from %s", rumor.Kind, rumor.Content, rumor.PubKey.Hex())
	}
	if tag := rumor.Tags.Find("p"); tag == nil || tag[1] != bob.key.Public.Hex() {
		t.Error("the message does not name its recipient, as NIP-17 requires")
	}
}

func TestANIP17MessageOpensInARC(t *testing.T) {
	ctx := context.Background()
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t), newCitizen(t, r)

	_, toBob, err := nip17.PrepareMessage(ctx, "hello from a nip-17 client", nil,
		keyer.NewPlainKeySigner(alice.key.Secret), bob.key.Public, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Send(ctx, toBob); err != nil {
		t.Fatal(err)
	}

	if got := bob.sync(t, r); got.Received != 1 {
		t.Fatalf("bob received %d messages, refused %v", got.Received, got.Refused)
	}
	msgs := bob.mail.Inbox()
	if len(msgs) != 1 || msgs[0].Text != "hello from a nip-17 client" || msgs[0].From != alice.key.Public {
		t.Errorf("bob's inbox: %+v", msgs)
	}
}

// Bob reads mail on one relay only. Alice reaches him there, because his
// NIP-17 relay list names it.
func TestAMessageFollowsTheRecipientsRelayList(t *testing.T) {
	ctx := context.Background()
	shared := relay.Relay{URL: testrelay.Start(t)}
	bobs := relay.Relay{URL: testrelay.Start(t)}

	alice, bob := newCitizen(t, shared), newCitizen(t, bobs)

	list, err := mail.RelayList(bob.key, []string{bobs.URL}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.Send(ctx, list); err != nil {
		t.Fatal(err)
	}

	alice.send(t, bob, "found you")
	if got := bob.sync(t, bobs); got.Received != 1 {
		t.Errorf("bob received %d messages on his own relay", got.Received)
	}
}

// The dm manifest sends a rumor of kind 14 with a p tag through SendRumor. A
// NIP-17 client opens it.
func TestARumorOfTheDMManifestOpensInANIP17Client(t *testing.T) {
	ctx := context.Background()
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t)

	if _, err := alice.mail.SendRumor(ctx, bob.key.Public, 14, "from the manifest", nostr.Tags{{"p", bob.key.Public.Hex()}}); err != nil {
		t.Fatal(err)
	}
	batch, err := r.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {bob.key.Public.Hex()}}})
	if err != nil || len(batch.Events) == 0 {
		t.Fatalf("no wrap for bob: %v", err)
	}
	client := keyer.NewPlainKeySigner(bob.key.Secret)
	rumor, err := nip59.GiftUnwrap(batch.Events[0], func(other nostr.PubKey, ciphertext string) (string, error) {
		return client.Decrypt(ctx, ciphertext, other)
	})
	if err != nil || rumor.Kind != nostr.KindDirectMessage || rumor.Content != "from the manifest" || rumor.PubKey != alice.key.Public {
		t.Errorf("the NIP-17 client read %+v %v", rumor, err)
	}

	// Bob's machine reads it back as a rumor, and so does Alice's.
	bob.sync(t, r)
	if got := bob.mail.Rumors([]nostr.Kind{14}); len(got) != 1 || got[0].Content != "from the manifest" {
		t.Errorf("bob's rumors: %+v", got)
	}
	if got := alice.mail.Rumors([]nostr.Kind{14}); len(got) != 1 || got[0].PubKey != alice.key.Public {
		t.Errorf("alice's rumors: %+v", got)
	}
}
