package testrelay

import (
	"sync"
	"testing"

	"fiatjaf.com/nostr"
)

// TestStoreQueriesWhileSaving runs queries while other goroutines save
// events, as a relay does when two connections publish and subscribe. Run it
// with -race.
func TestStoreQueriesWhileSaving(t *testing.T) {
	db := newStore(t)
	var wg sync.WaitGroup
	for writer := range 4 {
		wg.Go(func() {
			for i := range 200 {
				event := nostr.Event{CreatedAt: nostr.Timestamp(writer*1000 + i), Kind: 1}
				event.ID[0], event.ID[1] = byte(writer), byte(i)
				if err := db.SaveEvent(event); err != nil {
					t.Error(err)
				}
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 200 {
				for range db.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{1}}, 500) {
				}
			}
		})
	}
	wg.Wait()

	count, err := db.CountEvents(nostr.Filter{Kinds: []nostr.Kind{1}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 800 {
		t.Fatalf("store has %d events, want 800", count)
	}
}
