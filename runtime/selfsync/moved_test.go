package selfsync

import (
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/store"
)

// matchStore is a local store in memory that answers each filter.
type matchStore []nostr.Event

func (s matchStore) Save(nostr.Event) (store.Result, error) { return store.Result{}, nil }

func (s matchStore) Query(f nostr.Filter) ([]nostr.Event, error) {
	var out []nostr.Event
	for _, e := range s {
		if f.Matches(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

// A relay drops an old draft only when the storage citizens hold the
// record that replaces it on the device: the draft itself, a newer version
// of it, or a deletion of it (docs/storage/SPEC.md, section 4.2).
func TestMovable(t *testing.T) {
	citizen := keys.Generate()
	signed := func(e nostr.Event) nostr.Event {
		if err := e.Sign(citizen.Secret); err != nil {
			t.Fatal(err)
		}
		return e
	}
	oldWrap := signed(nostr.Event{Kind: 31234, CreatedAt: 100, Tags: nostr.Tags{{"d", "page"}}, Content: "v1"})
	newWrap := signed(nostr.Event{Kind: 31234, CreatedAt: 200, Tags: nostr.Tags{{"d", "page"}}, Content: "v2"})
	held := signed(nostr.Event{Kind: 1234, CreatedAt: 100, Tags: nostr.Tags{{"a", "x"}}, Content: "held"})
	gone := signed(nostr.Event{Kind: 1234, CreatedAt: 101, Tags: nostr.Tags{{"a", "x"}}, Content: "gone"})
	deletion := signed(nostr.Event{Kind: 5, CreatedAt: 300, Tags: nostr.Tags{{"e", gone.ID.Hex()}, {"k", "1234"}}})
	lost := signed(nostr.Event{Kind: 3275, CreatedAt: 100, Content: "lost"})
	list := signed(nostr.Event{Kind: 10013, CreatedAt: 100, Content: "list"})

	local := matchStore{newWrap, held, deletion}
	kept := map[nostr.ID]bool{newWrap.ID: true, held.ID: true, deletion.ID: true}
	for _, c := range []struct {
		name  string
		event nostr.Event
		kept  map[nostr.ID]bool
		want  bool
	}{
		{"a record that the storage citizens hold", held, kept, true},
		{"a record that no storage citizen holds", held, map[nostr.ID]bool{}, false},
		{"an older version of a kept draft", oldWrap, kept, true},
		{"an older version of a draft whose newer version waits", oldWrap, map[nostr.ID]bool{held.ID: true}, false},
		{"a record that a kept deletion names", gone, kept, true},
		{"a record that a waiting deletion names", gone, map[nostr.ID]bool{held.ID: true}, false},
		{"a part that the device does not hold", lost, kept, false},
		{"a private relay list", list, map[nostr.ID]bool{}, true},
	} {
		got, err := Movable(local, c.kept, c.event)
		if err != nil || got != c.want {
			t.Errorf("%s: movable %v (%v), want %v", c.name, got, err, c.want)
		}
	}
}

// The deletions for a relay carry the mark, name at most MaxMoved events
// each, and name their kinds. A deletion that the journal makes has no mark.
func TestMoves(t *testing.T) {
	var events []nostr.Event
	for i := range 250 {
		kind := nostr.Kind(1234)
		if i%2 == 0 {
			kind = 3275
		}
		e := nostr.Event{Kind: kind, CreatedAt: nostr.Timestamp(i), Tags: nostr.Tags{}}
		e.ID[0], e.ID[1] = byte(i), byte(i>>8)
		events = append(events, e)
	}
	moves := Moves(events, 1000)
	var counts []int
	for _, m := range moves {
		e, k := 0, 0
		for _, tag := range m.Tags {
			switch tag[0] {
			case "e":
				e++
			case "k":
				k++
			}
		}
		if !draft.IsMoved(m) || m.Kind != 5 || k != 2 {
			t.Errorf("a deletion has kind %d, %d k tags, and mark %v", m.Kind, k, draft.IsMoved(m))
		}
		counts = append(counts, e)
	}
	if len(counts) != 3 || counts[0] != 100 || counts[1] != 100 || counts[2] != 50 {
		t.Errorf("the deletions name %v events, want 100, 100 and 50", counts)
	}
	journal := nostr.Event{Kind: 5, Tags: nostr.Tags{{"e", events[0].ID.Hex()}, {"k", "1234"}}}
	if draft.IsMoved(journal) {
		t.Error("a deletion of the journal has the mark")
	}
}

// Report.Kept names a record only when the storage citizens hold k pieces
// of one seal of its blob. With fewer than k storage citizens that answer,
// the record waits, and is not kept.
func TestKept(t *testing.T) {
	ctx := t.Context()
	citizen := keys.Generate()
	local := &memStore{events: map[nostr.ID]nostr.Event{}}
	for n := range 3 {
		e := nostr.Event{Kind: 31234, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", string(rune('a' + n))}}, Content: "page"}
		if err := e.Sign(citizen.Secret); err != nil {
			t.Fatal(err)
		}
		local.events[e.ID] = e
	}
	sides := make([]Side, 3)
	for i := range sides {
		sides[i] = Side{Key: keys.Generate().Public, Label: string(rune('p' + i)), Conn: newMemStorage(), Active: true}
	}
	device := testSyncer(t, citizen, local)

	// Three storage citizens give k = 2. One answers.
	one := []Side{sides[0], sides[1], sides[2]}
	one[1].Conn, one[2].Conn = nil, nil
	r, err := device.SyncAll(ctx, one)
	if err != nil {
		t.Fatal(err)
	}
	if r.Waiting != 3 || len(r.Kept) != 0 {
		t.Fatalf("with one storage citizen of three, %d records wait and %d are kept; want 3 and 0", r.Waiting, len(r.Kept))
	}
	r, err = device.SyncAll(ctx, sides)
	if err != nil {
		t.Fatal(err)
	}
	for id := range local.events {
		if !r.Kept[id] {
			t.Errorf("record %s is not kept after a sync with each storage citizen", id.Hex()[:8])
		}
	}
}
