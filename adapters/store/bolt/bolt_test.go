package boltstore_test

import (
	"path/filepath"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"go.etcd.io/bbolt"
)

func open(t *testing.T, path string) *boltstore.Backend {
	t.Helper()
	b := boltstore.NewBackend(path)
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}
	return b
}

func get(t *testing.T, b *boltstore.Backend, id nostr.ID) []nostr.Event {
	t.Helper()
	var out []nostr.Event
	for e := range b.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}}, 10) {
		out = append(out, e)
	}
	return out
}

// The Bolt codec holds 65,535 bytes of content. A larger event survives a
// reopen whole, and its deletion leaves nothing behind.
func TestContentLargerThanTheCodecSurvives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	key := nostr.Generate()
	large := nostr.Event{Kind: 1059, CreatedAt: nostr.Now(), Content: strings.Repeat("A", 1<<17)}
	large.Sign(key)
	small := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: "small"}
	small.Sign(key)

	b := open(t, path)
	for _, e := range []nostr.Event{large, small} {
		if err := b.SaveEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	b.Close()

	b = open(t, path)
	defer b.Close()
	for _, e := range []nostr.Event{large, small} {
		got := get(t, b, e.ID)
		if len(got) != 1 || got[0].Content != e.Content || !got[0].VerifySignature() {
			t.Errorf("kind %d with %d bytes of content did not come back whole", e.Kind, len(e.Content))
		}
	}

	if err := b.DeleteEvent(large.ID); err != nil {
		t.Fatal(err)
	}
	if got := get(t, b, large.ID); len(got) != 0 {
		t.Errorf("the deleted event is still served")
	}
	_ = b.DB.View(func(tx *bbolt.Tx) error {
		if n := tx.Bucket([]byte("arcLargeContent")).Stats().KeyN; n != 0 {
			t.Errorf("the bucket of large content holds %d entries after the deletion", n)
		}
		return nil
	})
}
