package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
)

func TestStorageListRules(t *testing.T) {
	ctx := context.Background()
	citizen := keyer.NewPlainKeySigner(nostr.Generate())
	me, _ := citizen.GetPublicKey(ctx)
	charlie := nostr.Generate().Public()

	var list List
	list, err := list.Put(ListEntry{Key: charlie, Name: "charlie", State: StateActive, Since: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The same key comes back after gone: the entry is replaced, not added.
	list, err = list.Put(ListEntry{Key: charlie, Name: "charlie", State: StateActive, Since: 2})
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Since != 2 {
		t.Fatalf("a second put of one key gave %+v, %v; want one entry with since 2", list.Entries, err)
	}
	if _, err := list.Put(ListEntry{Key: nostr.Generate().Public(), Name: "charlie", State: StateActive}); err == nil {
		t.Fatal("a second key took the name charlie")
	}
	if _, err := list.Put(ListEntry{Key: nostr.Generate().Public(), Name: "dana", State: "paused"}); err == nil {
		t.Fatal("the state paused was taken")
	}
	for i := len(list.Entries); i < MaxEntries; i++ {
		if list, err = list.Put(ListEntry{Key: nostr.Generate().Public(), Name: fmt.Sprint("s", i), State: StateActive}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := list.Put(ListEntry{Key: nostr.Generate().Public(), Name: "seventeenth", State: StateActive}); err == nil {
		t.Fatal("the list took a 17th storage citizen")
	}

	event, err := SealList(ctx, citizen, list, 100)
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != 10273 || len(event.Tags) != 1 || event.Tags[0][0] != "-" || strings.Contains(event.Content, "charlie") {
		t.Fatalf("the list event is kind %d with tags %v; want kind 10273, the NIP-70 tag and sealed content", event.Kind, event.Tags)
	}
	opened, err := OpenList(ctx, citizen, me, event)
	if err != nil || len(opened.Entries) != MaxEntries || opened.At != 100 {
		t.Fatalf("the list opened as %d entries at %d, %v", len(opened.Entries), opened.At, err)
	}
	if _, err := OpenList(ctx, citizen, nostr.Generate().Public(), event); err == nil {
		t.Fatal("a list of another author opened")
	}

	// A list of a later version is refused, with the advice to update.
	text, _ := json.Marshal(map[string]any{"v": 2, "storage": []ListEntry{}})
	content, _ := citizen.Encrypt(ctx, string(text), me)
	later := nostr.Event{Kind: ListKind, CreatedAt: 200, Content: content, Tags: nostr.Tags{protected}}
	if err := citizen.SignEvent(ctx, &later); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenList(ctx, citizen, me, later); err == nil || !strings.Contains(err.Error(), "update arc") {
		t.Fatalf("a list of version 2 gave %v; want a refusal that says to update arc", err)
	}
}

// TestPruneGone gives the clock: a gone entry stays for 30 days after its
// since, and goes at 30 days. An old active or draining entry stays.
func TestPruneGone(t *testing.T) {
	since := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := nostr.Timestamp(since.Unix())
	list := List{At: 7, Entries: []ListEntry{
		{Key: nostr.Generate().Public(), Name: "charlie", State: StateGone, Since: at},
		{Key: nostr.Generate().Public(), Name: "dana", State: StateActive, Since: 1},
		{Key: nostr.Generate().Public(), Name: "erin", State: StateDraining, Since: 1},
	}}
	kept, removed := list.Prune(since.Add(30*24*time.Hour - time.Second))
	if len(removed) != 0 || len(kept.Entries) != 3 {
		t.Fatalf("29 days 23:59:59 after gone: removed %v, kept %d entries; want none removed", removed, len(kept.Entries))
	}
	kept, removed = list.Prune(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	if len(removed) != 1 || removed[0].Name != "charlie" {
		t.Fatalf("30 days after gone: removed %v; want charlie", removed)
	}
	if len(kept.Entries) != 2 || kept.Entries[0].Name != "dana" || kept.Entries[1].Name != "erin" || kept.At != 7 {
		t.Fatalf("30 days after gone: kept %+v; want dana and erin at 7", kept)
	}
}
