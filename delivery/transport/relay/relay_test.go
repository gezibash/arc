package relay_test

import (
	"context"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport/relay"
)

func protected(t *testing.T, k keys.Key) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: "mine", Tags: nostr.Tags{{"-"}}}
	if err := event.Sign(k.Secret); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestAProtectedEventNeedsItsAuthor(t *testing.T) {
	url := testrelay.Start(t)
	k := keys.Generate()
	event := protected(t, k)
	ctx := context.Background()

	err := relay.Relay{URL: url}.Send(ctx, event)
	if err == nil || !strings.Contains(err.Error(), "auth-required") {
		t.Fatalf("a relay took a protected event without authentication: %v", err)
	}

	stranger := relay.Relay{URL: url, Signer: keyer.NewPlainKeySigner(keys.Generate().Secret)}
	if err := stranger.Send(ctx, event); err == nil {
		t.Fatal("a relay took a protected event from another citizen")
	}

	author := relay.Relay{URL: url, Signer: keyer.NewPlainKeySigner(k.Secret)}
	if err := author.Send(ctx, event); err != nil {
		t.Fatalf("the author could not publish: %v", err)
	}
	batch, err := author.Fetch(ctx, nostr.Filter{IDs: []nostr.ID{event.ID}})
	if err != nil || len(batch.Events) != 1 {
		t.Errorf("the relay does not hold the event: %v %d", err, len(batch.Events))
	}
}
