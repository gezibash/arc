package selfsync

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"

	"fiatjaf.com/nostr"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/store"
)

// pageBench is one page of a citizen that changes, in the real store of a
// device, and three storage apps in memory: k = 2, t = 3, so each blob is
// one piece on each storage app.
type pageBench struct {
	t       *testing.T
	citizen keys.Key
	keys    Keys
	local   *store.Store
	stores  []*memStorage
	sides   []Side
	device  *Syncer
	// drafts holds each version of the page, oldest first.
	drafts []nostr.Event
}

func newPageBench(t *testing.T) *pageBench {
	t.Helper()
	b := &pageBench{t: t, citizen: keys.Generate(), keys: testKeys(t, 0)}
	b.local = b.newStore()
	for i := range 3 {
		b.stores = append(b.stores, newMemStorage())
		b.sides = append(b.sides, Side{Key: keys.Generate().Public, Label: fmt.Sprint("s", i), Conn: b.stores[i], Active: true})
	}
	b.device = b.syncer(b.local)
	return b
}

func (b *pageBench) newStore() *store.Store {
	b.t.Helper()
	s, err := boltstore.Open(b.t.TempDir())
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(s.Close)
	return s
}

func (b *pageBench) syncer(local Store) *Syncer {
	return &Syncer{
		Citizen: b.citizen.Public, Device: b.citizen.Secret, Store: local, State: b.t.TempDir(),
		Newest:  func(context.Context) (Keys, error) { return b.keys, nil },
		Keys:    func(context.Context, uint32) (Keys, error) { return b.keys, nil },
		Devices: func(context.Context) (devices.List, bool, error) { return devices.List{}, false, nil },
	}
}

// edit writes version n of the page and its checkpoint, as a publish of
// a sealed kind does (interface spec, section 3.9.1).
func (b *pageBench) edit(n int) {
	b.t.Helper()
	at := nostr.Timestamp(1_800_000_000 + n)
	for _, e := range []nostr.Event{
		{Kind: draft.Kind, CreatedAt: at, Tags: nostr.Tags{{"d", "page"}}, Content: fmt.Sprint("version ", n)},
		{Kind: draft.CheckpointKind, CreatedAt: at, Tags: nostr.Tags{{"a", draft.Coordinate(b.citizen.Public, "page")}}, Content: fmt.Sprint("version ", n)},
	} {
		if err := e.Sign(b.citizen.Secret); err != nil {
			b.t.Fatal(err)
		}
		if r, err := b.local.Save(e); err != nil || r.Outcome != store.Stored {
			b.t.Fatalf("the store took version %d as %v: %v", n, r.Outcome, err)
		}
		if e.Kind == draft.Kind {
			b.drafts = append(b.drafts, e)
		}
	}
}

func (b *pageBench) sync(s *Syncer, repair bool, sides []Side) Report {
	b.t.Helper()
	s.Repair = repair
	r, err := s.SyncAll(context.Background(), sides)
	if err != nil {
		b.t.Fatal(err)
	}
	return r
}

func (b *pageBench) held(want ...int) {
	b.t.Helper()
	got := make([]int, len(b.stores))
	for i, s := range b.stores {
		got[i] = len(s.data)
	}
	if !slices.Equal(got, want) {
		b.t.Fatalf("the storage apps hold %v pieces; want %v", got, want)
	}
}

// reads checks that a new device reads the newest version and each
// checkpoint.
func (b *pageBench) reads(checkpoints int) {
	b.t.Helper()
	fresh := b.newStore()
	r := b.sync(b.syncer(fresh), false, b.sides)
	if len(r.Unreadable) != 0 {
		b.t.Fatalf("a new device cannot read %v", r.Unreadable)
	}
	got, err := fresh.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}})
	if err != nil || len(got) != 1 || got[0].ID != b.drafts[len(b.drafts)-1].ID {
		b.t.Fatalf("a new device holds the drafts %v: %v; want the newest version", got, err)
	}
	got, err = fresh.Query(nostr.Filter{Kinds: []nostr.Kind{draft.CheckpointKind}})
	if err != nil || len(got) != checkpoints {
		b.t.Fatalf("a new device holds %d checkpoints: %v; want %d", len(got), err, checkpoints)
	}
}

// TestReplaced checks section 4.7 of the storage spec. A sync that
// repairs deletes the blob of a replaced version only when the storage
// citizens hold the newer version. A sync that does not repair counts it.
// The checkpoints stay, and a new device reads the newest version and
// each checkpoint.
func TestReplaced(t *testing.T) {
	b := newPageBench(t)
	b.edit(1)
	b.sync(b.device, false, b.sides)
	b.held(2, 2, 2)
	b.edit(2)
	// The sync gets version 1 back, and the store says that version 2
	// replaced it. Version 2 goes in the same sync, so the listings of
	// this sync do not hold it yet: version 1 waits.
	if r := b.sync(b.device, true, b.sides); r.Repair.Replaced != 0 {
		t.Fatalf("a repair deleted %d replaced blobs before the newer version was listed", r.Repair.Replaced)
	}
	b.held(4, 4, 4)
	if r := b.sync(b.device, false, b.sides); r.Repair.Replaced != 1 {
		t.Fatalf("a sync with no repair counted %d replaced blobs; want 1", r.Repair.Replaced)
	}
	b.held(4, 4, 4)
	if r := b.sync(b.device, true, b.sides); r.Repair.Replaced != 1 || r.Repair.Deleted != 3 || r.Repair.Down != 0 {
		t.Fatalf("the repair removed %d replaced blobs, deleted %d pieces and got %d bytes; want 1, 3 and 0", r.Repair.Replaced, r.Repair.Deleted, r.Repair.Down)
	}
	b.held(3, 3, 3)
	b.reads(2)
}

// TestReplacedInterrupted checks that a deletion that stops on one
// storage citizen ends at the next repair, and loses nothing.
func TestReplacedInterrupted(t *testing.T) {
	b := newPageBench(t)
	b.edit(1)
	b.sync(b.device, false, b.sides)
	b.edit(2)
	b.sync(b.device, false, b.sides)
	cut := slices.Clone(b.sides)
	cut[2].Conn = &failing{memStorage: b.stores[2], deletes: true}
	b.sync(b.device, true, cut)
	b.held(3, 3, 4)
	b.reads(2)
	if r := b.sync(b.device, true, b.sides); r.Repair.Replaced != 1 || r.Repair.Deleted != 1 {
		t.Fatalf("the next repair removed %d replaced blobs and deleted %d pieces; want 1 and 1", r.Repair.Replaced, r.Repair.Deleted)
	}
	b.held(3, 3, 3)
	b.reads(2)
}

// TestReplacedTURN checks that a deletion runs on a pipe through TURN with
// no --turn: it moves no bytes of the blob (Otto, 2026-10-19; storage
// spec, section 4.6). A rebuild still waits, see TestRepairTURN.
func TestReplacedTURN(t *testing.T) {
	b := newPageBench(t)
	b.edit(1)
	b.sync(b.device, false, b.sides)
	b.edit(2)
	b.sync(b.device, false, b.sides)
	relayed := slices.Clone(b.sides)
	relayed[0].Relayed = true
	r := b.sync(b.device, true, relayed)
	if r.Repair.WaitDirect != 0 || r.Repair.Replaced != 1 {
		t.Fatalf("a repair through TURN counted %d blobs that wait and removed %d replaced blobs; want 0 and 1", r.Repair.WaitDirect, r.Repair.Replaced)
	}
	b.held(3, 3, 3)
}

// TestReplacedLearns checks a device that got the blob of version 1
// before this version: it kept no note of it. The first repair gets the
// blob again, the store says that it is replaced, and the repair deletes
// it.
func TestReplacedLearns(t *testing.T) {
	b := newPageBench(t)
	b.edit(1)
	b.sync(b.device, false, b.sides)
	b.edit(2)
	b.sync(b.device, false, b.sides)
	if err := os.Remove(b.device.oldPath()); err != nil {
		t.Fatal(err)
	}
	if r := b.sync(b.device, false, b.sides); r.Repair.Replaced != 0 {
		t.Fatalf("a device with no note counted %d replaced blobs with no repair", r.Repair.Replaced)
	}
	r := b.sync(b.device, true, b.sides)
	if r.Repair.Replaced != 1 || r.Repair.Down == 0 || r.Held != 0 {
		t.Fatalf("the repair removed %d replaced blobs, got %d bytes, and counted %d records held; want 1, the pieces of one blob, and 0", r.Repair.Replaced, r.Repair.Down, r.Held)
	}
	b.held(3, 3, 3)
}

// frozen is the store of a device that takes no record: it holds version
// 1 of the page, and cannot learn of version 2.
type frozen struct{ held []nostr.Event }

func (f frozen) Save(nostr.Event) (store.Result, error) {
	return store.Result{Outcome: store.Refused, Reason: "the store is frozen"}, nil
}

func (f frozen) SaveFrom(e nostr.Event, _ store.Source) (store.Result, error) { return f.Save(e) }

func (f frozen) Query(nostr.Filter) ([]nostr.Event, error) { return f.held, nil }

// TestReplacedReturns checks a deleted blob that comes back: a device
// whose store still holds version 1, and does not take version 2, puts
// version 1 again with a new seal. The next repair of another device
// deletes it again.
func TestReplacedReturns(t *testing.T) {
	b := newPageBench(t)
	b.edit(1)
	b.sync(b.device, false, b.sides)
	checkpoint, err := b.local.Query(nostr.Filter{Kinds: []nostr.Kind{draft.CheckpointKind}})
	if err != nil {
		t.Fatal(err)
	}
	old := b.syncer(frozen{held: append([]nostr.Event{b.drafts[0]}, checkpoint...)})
	b.edit(2)
	b.sync(b.device, false, b.sides)
	b.sync(b.device, true, b.sides)
	b.held(3, 3, 3)
	b.sync(old, false, b.sides)
	b.held(4, 4, 4)
	if r := b.sync(b.device, true, b.sides); r.Repair.Replaced != 1 {
		t.Fatalf("the repair removed %d replaced blobs after version 1 came back; want 1", r.Repair.Replaced)
	}
	b.held(3, 3, 3)
	b.reads(2)
}

// TestARepairOverTURNDeletesWhileARebuildWaits checks decision 5 of the
// storage spec, section 4.6: with no --turn, a repair over TURN still
// removes the blob of a replaced record, and the rebuild onto a new
// storage citizen behind TURN waits, so the device counts it and says so.
func TestARepairOverTURNDeletesWhileARebuildWaits(t *testing.T) {
	b := newPageBench(t)
	b.edit(1)
	b.sync(b.device, false, b.sides)
	b.edit(2)
	b.sync(b.device, false, b.sides)
	extra := newMemStorage()
	b.stores = append(b.stores, extra)
	sides := append(slices.Clone(b.sides), Side{Key: keys.Generate().Public, Label: "s3", Conn: extra, Active: true, Relayed: true})
	sides[0].Relayed = true
	r := b.sync(b.device, true, sides)
	if r.Repair.Replaced != 1 {
		t.Errorf("the repair removed %d replaced blobs; want 1", r.Repair.Replaced)
	}
	if r.Repair.WaitDirect == 0 {
		t.Error("the rebuild onto the new storage citizen behind TURN did not wait")
	}
	b.held(3, 3, 3, 0)
}
