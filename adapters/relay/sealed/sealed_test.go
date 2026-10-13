package sealed_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/internal/testrelay"
)

var ctx = context.Background()

// An older client sent its drafts to the relay before the relay refused
// them. The relay still serves them to their author.
func TestARelayServesSealedDataOnlyToItsAuthor(t *testing.T) {
	author := nostr.Generate()
	k := keyer.NewPlainKeySigner(author)
	wrap, err := draft.Wrap(ctx, k, "x", nostr.Event{Kind: 30023, CreatedAt: 1, Content: "secret"}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	part, _ := draft.Part(ctx, k, "x", "more", nostr.Now())
	url := testrelay.StartHolding(t, wrap, part)
	mine := relay.Relay{URL: url, Signer: k}

	public := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: "hello"}
	public.Sign(author)
	if err := mine.Send(ctx, public); err != nil {
		t.Fatal(err)
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

	// The draft of the keyed root is the one draft that a relay still takes.
	wrap, _ := draft.Wrap(ctx, k, "arc-keyed-root", nostr.Event{Kind: 30078, CreatedAt: 1, Content: "live"}, nostr.Now())
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

// The relay refuses new drafts, checkpoints, parts and private relay lists
// (docs/storage/SPEC.md, section 7), and takes the draft of the keyed root.
// The author still deletes an old draft with NIP-09, and the deletion is
// sealed, because its e tags count the drafts.
func TestARelayRefusesNewDrafts(t *testing.T) {
	author := nostr.Generate()
	k := keyer.NewPlainKeySigner(author)
	old, _ := draft.Wrap(ctx, k, "old", nostr.Event{Kind: 30023, CreatedAt: 1, Content: "old"}, 1000)
	moved, _ := draft.Wrap(ctx, k, "moved", nostr.Event{Kind: 30023, CreatedAt: 1, Content: "moved"}, 1000)
	url := testrelay.StartHolding(t, old, moved)
	mine := relay.Relay{URL: url, Signer: k}

	wrap, _ := draft.Wrap(ctx, k, "x", nostr.Event{Kind: 30023, CreatedAt: 1, Content: "new"}, nostr.Now())
	checkpoint, _ := draft.Checkpoint(ctx, k, "x", nostr.Event{Kind: 30023, CreatedAt: 1, Content: "new"}, nostr.Now())
	part, _ := draft.Part(ctx, k, "x", "more", nostr.Now())
	list := nostr.Event{Kind: 10013, CreatedAt: nostr.Now(), Content: "sealed", Tags: nostr.Tags{}}
	list.Sign(author)
	for _, e := range []nostr.Event{wrap, checkpoint, part, list} {
		if err := mine.Send(ctx, e); err == nil || !strings.Contains(err.Error(), "keeps no drafts") {
			t.Errorf("the relay took a new event of kind %d: %v", e.Kind, err)
		}
	}
	root, _ := draft.Wrap(ctx, k, "arc-keyed-root", nostr.Event{Kind: 30078, CreatedAt: 1, Content: "root"}, nostr.Now())
	if err := mine.Send(ctx, root); err != nil {
		t.Errorf("the relay refused the draft of the keyed root: %v", err)
	}

	deletion := nostr.Event{Kind: nostr.KindDeletion, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"e", old.ID.Hex()}, {"k", "31234"}}}
	deletion.Sign(author)
	if err := mine.Send(ctx, deletion); err != nil {
		t.Fatalf("the relay refused the deletion of an old draft: %v", err)
	}
	// A deletion with the moved tag deletes, and then nobody reads it.
	gone := nostr.Event{Kind: nostr.KindDeletion, CreatedAt: nostr.Now(), Tags: nostr.Tags{draft.Moved, {"e", moved.ID.Hex()}, {"k", "31234"}}}
	gone.Sign(author)
	if err := mine.Send(ctx, gone); err != nil {
		t.Fatalf("the relay refused a moved deletion: %v", err)
	}
	batch, err := mine.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{draft.Kind, nostr.KindDeletion}, Authors: []nostr.PubKey{author.Public()}})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []nostr.Kind
	for _, e := range batch.Events {
		if e.ID == old.ID || e.ID == moved.ID {
			t.Error("the relay still serves a deleted draft")
		}
		if e.ID == gone.ID {
			t.Error("the relay serves a moved deletion to its author")
		}
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) != 2 {
		t.Errorf("the author reads kinds %v, want the keyed root and the deletion", kinds)
	}
	if got, err := mine.Tags(ctx); err != nil || !slices.Contains(got, draft.RetiredTag) {
		t.Errorf("the NIP-11 tags of the relay are %v (%v), want %s", got, err, draft.RetiredTag)
	}
	anonymous := relay.Relay{URL: url}
	batch, err = anonymous.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}, Authors: []nostr.PubKey{author.Public()}})
	if err != nil || len(batch.Events) != 0 {
		t.Errorf("an anonymous query read %d deletions of drafts: %v", len(batch.Events), err)
	}
}

// token is a Delegation with one fixed token.
type token struct {
	device nostr.Signer
	event  nostr.Event
}

func (d token) Token(context.Context) (nostr.Signer, nostr.Event, bool) {
	return d.device, d.event, true
}
func (d token) Answered(context.Context) {}

// stopped is a citizen signer that never answers.
type stopped struct{ nostr.Keyer }

func (stopped) SignEvent(context.Context, *nostr.Event) error {
	return errors.New("the signer is stopped")
}

// A device reads the sealed data of its citizen with a token, and the
// citizen key signs nothing. The relay refuses a token for another key, a
// token that has ended, and the data of a citizen who did not sign the
// token. It never keeps or broadcasts a token.
func TestADeviceReadsSealedDataWithAToken(t *testing.T) {
	citizen, device, other := nostr.Generate(), nostr.Generate(), nostr.Generate()
	wrap, err := draft.Wrap(ctx, keyer.NewPlainKeySigner(citizen), "x", nostr.Event{Kind: 30023, CreatedAt: 1, Content: "secret"}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	url := testrelay.StartHolding(t, wrap)
	drafts := nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{citizen.Public()}}
	signed := func(by nostr.SecretKey, to nostr.PubKey, at nostr.Timestamp) nostr.Event {
		event := devices.NewToken(to, at)
		if err := event.Sign(by); err != nil {
			t.Fatal(err)
		}
		return event
	}
	reader := func(event nostr.Event) relay.Relay {
		return relay.Relay{URL: url, Signer: stopped{}, Delegation: token{device: keyer.NewPlainKeySigner(device), event: event}}
	}

	given := signed(citizen, device.Public(), nostr.Now())
	everything, err := relay.Relay{URL: url}.Watch(ctx, nostr.Filter{Kinds: []nostr.Kind{devices.TokenKind}})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := reader(given).Fetch(ctx, drafts)
	if err != nil || len(batch.Events) != 1 || batch.Events[0].ID != wrap.ID {
		t.Fatalf("the device read %d drafts with a token: %v", len(batch.Events), err)
	}

	cases := []struct {
		name  string
		event nostr.Event
		want  string
	}{
		{"a token for another key", signed(citizen, other.Public(), nostr.Now()), "another device key"},
		{"a token that has ended", signed(citizen, device.Public(), nostr.Now()-devices.TokenLife-1), "has ended"},
		{"a token of another citizen", signed(other, device.Public(), nostr.Now()), "only their own"},
	}
	for _, c := range cases {
		if _, err := reader(c.event).Fetch(ctx, drafts); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}

	kept, err := relay.Relay{URL: url}.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{devices.TokenKind}})
	if err != nil || len(kept.Events) != 0 {
		t.Errorf("the relay keeps %d tokens: %v", len(kept.Events), err)
	}
	select {
	case event := <-everything:
		t.Errorf("a watch of kind %d got a token of %s", devices.TokenKind, event.PubKey.Hex())
	case <-time.After(200 * time.Millisecond):
	}
}
