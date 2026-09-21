package node_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/delivery/transport/file"
	"github.com/gezibash/arc/delivery/transport/relay"
)

func newNode(t *testing.T) *node.Node {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return &node.Node{Store: s}
}

func note(t *testing.T, k keys.Key, text string) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: 3275, CreatedAt: nostr.Timestamp(time.Now().Unix()), Content: text}
	if err := event.Sign(k.Secret); err != nil {
		t.Fatal(err)
	}
	return event
}

// syncBoth checks one transport: what one node holds reaches the other.
func syncBoth(t *testing.T, tr transport.Transport) {
	ctx := context.Background()
	k := keys.Generate()
	filter := nostr.Filter{Authors: []nostr.PubKey{k.Public}}

	a, b := newNode(t), newNode(t)
	first := note(t, k, "one")
	second := note(t, k, "two")

	if _, err := a.Store.Save(first); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.Save(second); err != nil {
		t.Fatal(err)
	}

	up, err := a.Sync(ctx, filter, tr)
	if err != nil {
		t.Fatal(err)
	}
	if up.Sent != 1 {
		t.Errorf("a sent %d events, want 1", up.Sent)
	}

	down, err := b.Sync(ctx, filter, tr)
	if err != nil {
		t.Fatal(err)
	}
	if down.Received != 1 || down.Sent != 1 {
		t.Errorf("b received %d and sent %d, want 1 and 1", down.Received, down.Sent)
	}

	if _, err := a.Sync(ctx, filter, tr); err != nil {
		t.Fatal(err)
	}
	for _, id := range []nostr.ID{first.ID, second.ID} {
		if !a.Store.Has(id) || !b.Store.Has(id) {
			t.Errorf("event %s did not reach both nodes", id.Hex())
		}
	}
}

func TestSyncThroughADirectory(t *testing.T) {
	syncBoth(t, file.Dir{Path: t.TempDir()})
}

func TestSyncThroughARelay(t *testing.T) {
	syncBoth(t, relay.Relay{URL: testrelay.Start(t)})
}

func TestADirectoryCannotSmuggleAChangedEvent(t *testing.T) {
	ctx := context.Background()
	k := keys.Generate()
	dir := t.TempDir()
	stick := file.Dir{Path: dir}

	event := note(t, k, "the real text")
	if err := stick.Send(ctx, event); err != nil {
		t.Fatal(err)
	}

	// Someone changes the text on the stick.
	path := filepath.Join(dir, "events", event.ID.Hex()+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "the real text", "a forged text", 1))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events", "junk.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	b := newNode(t)
	report, err := b.Sync(ctx, nostr.Filter{Authors: []nostr.PubKey{k.Public}}, stick)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Refused) != 1 || report.Received != 0 {
		t.Errorf("refused %d and received %d, want 1 and 0", len(report.Refused), report.Received)
	}
	if report.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", report.Unreadable)
	}
	if b.Store.Has(event.ID) {
		t.Error("the store kept a changed event")
	}
}

func TestObtainAsksTheTransportsOnlyForWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	k := keys.Generate()
	url := testrelay.Start(t)
	r := relay.Relay{URL: url}

	a := newNode(t)
	event := note(t, k, "far away")
	if _, _, err := a.Publish(ctx, event, []transport.Transport{r}); err != nil {
		t.Fatal(err)
	}

	b := newNode(t)
	found, err := b.Obtain(ctx, []nostr.ID{event.ID}, []transport.Transport{r})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := found[event.ID]; !ok {
		t.Fatal("the event did not come from the relay")
	}
	if !b.Store.Has(event.ID) {
		t.Error("the obtained event is not in the store")
	}
}

func TestWatchPassesOnNewEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	k := keys.Generate()
	r := relay.Relay{URL: testrelay.Start(t)}
	b := newNode(t)

	events, err := b.Watch(ctx, nostr.Filter{Authors: []nostr.PubKey{k.Public}}, r)
	if err != nil {
		t.Fatal(err)
	}

	sent := note(t, k, "live")
	if err := r.Send(ctx, sent); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-events:
		if got.ID != sent.ID {
			t.Errorf("watch passed on %s, want %s", got.ID.Hex(), sent.ID.Hex())
		}
	case <-ctx.Done():
		t.Fatal("the new event never arrived")
	}
}

func TestSyncReconcilesWhenTheRelaySupportsIt(t *testing.T) {
	syncBoth(t, relay.Relay{URL: testrelay.StartPlain(t)})

	for _, c := range []struct {
		name       string
		url        string
		reconciled bool
	}{
		{"a relay with Negentropy", testrelay.Start(t), true},
		{"a relay without it", testrelay.StartPlain(t), false},
	} {
		k := keys.Generate()
		a, b := newNode(t), newNode(t)
		r := relay.Relay{URL: c.url}
		filter := nostr.Filter{Authors: []nostr.PubKey{k.Public}}

		for i := 0; i < 5; i++ {
			if _, err := a.Store.Save(note(t, k, fmt.Sprintf("note %d", i))); err != nil {
				t.Fatal(err)
			}
		}
		up, err := a.Sync(context.Background(), filter, r)
		if err != nil {
			t.Fatal(err)
		}
		down, err := b.Sync(context.Background(), filter, r)
		if err != nil {
			t.Fatal(err)
		}

		if up.Reconciled != c.reconciled || down.Reconciled != c.reconciled {
			t.Errorf("%s: reconciled %v and %v, want %v", c.name, up.Reconciled, down.Reconciled, c.reconciled)
		}
		if up.Sent != 5 || down.Received != 5 {
			t.Errorf("%s: sent %d and received %d, want 5 and 5", c.name, up.Sent, down.Received)
		}
	}
}

// When the relay dies, a watch must end. Before, it read empty events from
// the closed channel in a loop, and used a whole CPU core.
func TestAWatchEndsWhenTheRelayDies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	url, kill := testrelay.StartKillable(t)
	r := relay.Relay{URL: url}
	k := keys.Generate()
	filter := nostr.Filter{Authors: []nostr.PubKey{k.Public}}

	raw, err := r.Watch(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	watched, err := newNode(t).Watch(ctx, filter, r)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	kill()

	deadline := time.After(5 * time.Second)
	for raw != nil || watched != nil {
		select {
		case event, ok := <-raw:
			if !ok {
				raw = nil
				continue
			}
			if event.ID == (nostr.ID{}) {
				t.Fatal("the watch passed on an empty event after the relay died")
			}
		case _, ok := <-watched:
			if !ok {
				watched = nil
			}
		case <-deadline:
			t.Fatal("the watch did not end after the relay died")
		}
	}
}
