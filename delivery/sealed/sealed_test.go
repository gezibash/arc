package sealed_test

import (
	"context"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/gezibash/arc/delivery/draft"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport/relay"
)

var ctx = context.Background()

func TestARelayServesSealedDataOnlyToItsAuthor(t *testing.T) {
	url := testrelay.Start(t)
	author := nostr.Generate()
	k := keyer.NewPlainKeySigner(author)
	mine := relay.Relay{URL: url, Signer: k}

	wrap, err := draft.Wrap(ctx, k, "x", nostr.Event{Kind: 30023, CreatedAt: 1, Content: "secret"}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	part, _ := draft.Part(ctx, k, "x", "more", nostr.Now())
	public := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: "hello"}
	public.Sign(author)
	for _, e := range []nostr.Event{wrap, part, public} {
		if err := mine.Send(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	// The author reads their drafts, after authentication.
	batch, err := mine.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{author.Public()}})
	if err != nil || len(batch.Events) != 1 {
		t.Fatalf("the author read %d drafts: %v", len(batch.Events), err)
	}
	batch, err = mine.Fetch(ctx, nostr.Filter{IDs: []nostr.ID{part.ID}, Kinds: []nostr.Kind{draft.PartKind}})
	if err == nil {
		t.Error("a query of parts with no author passed")
	}
	batch, err = mine.Fetch(ctx, nostr.Filter{IDs: []nostr.ID{part.ID}, Kinds: []nostr.Kind{draft.PartKind}, Authors: []nostr.PubKey{author.Public()}})
	if err != nil || len(batch.Events) != 1 {
		t.Errorf("the author read %d parts: %v", len(batch.Events), err)
	}

	// A stranger, and a citizen who does not authenticate, do not.
	anonymous := relay.Relay{URL: url}
	if _, err := anonymous.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{author.Public()}}); err == nil ||
		!strings.Contains(err.Error(), "authentication") {
		t.Errorf("an anonymous query of drafts: %v", err)
	}
	stranger := relay.Relay{URL: url, Signer: keyer.NewPlainKeySigner(nostr.Generate())}
	if _, err := stranger.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{author.Public()}}); err == nil ||
		!strings.Contains(err.Error(), "only their own") {
		t.Errorf("a stranger's query of drafts: %v", err)
	}

	// A query that names no kind leaves the sealed events out.
	for _, r := range []relay.Relay{anonymous, stranger} {
		batch, err := r.Fetch(ctx, nostr.Filter{IDs: []nostr.ID{wrap.ID, part.ID, public.ID}})
		if err != nil || len(batch.Events) != 1 || batch.Events[0].ID != public.ID {
			t.Errorf("a query by ID returned %d events: %v", len(batch.Events), err)
		}
		batch, err = r.Fetch(ctx, nostr.Filter{Authors: []nostr.PubKey{author.Public()}})
		if err != nil || len(batch.Events) != 1 {
			t.Errorf("a query by author returned %d events: %v", len(batch.Events), err)
		}
	}
}

func TestALiveWatchOfDraftsNeedsItsAuthor(t *testing.T) {
	url := testrelay.Start(t)
	author := nostr.Generate()
	k := keyer.NewPlainKeySigner(author)
	mine := relay.Relay{URL: url, Signer: k}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := mine.Watch(watchCtx, nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{author.Public()}, Since: nostr.Now()})
	if err != nil {
		t.Fatal(err)
	}
	// Anyone who watches everything sees no draft.
	everything, err := relay.Relay{URL: url}.Watch(watchCtx, nostr.Filter{Since: nostr.Now()})
	if err != nil {
		t.Fatal(err)
	}

	wrap, _ := draft.Wrap(ctx, k, "x", nostr.Event{Kind: 30023, CreatedAt: 1, Content: "live"}, nostr.Now())
	if err := mine.Send(ctx, wrap); err != nil {
		t.Fatal(err)
	}
	if got := <-events; got.ID != wrap.ID {
		t.Errorf("the author's watch got %v", got.ID)
	}
	public := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: "after"}
	public.Sign(author)
	mine.Send(ctx, public)
	if got := <-everything; got.ID != public.ID {
		t.Errorf("an anonymous watch saw %d before the public note", got.Kind)
	}
}
