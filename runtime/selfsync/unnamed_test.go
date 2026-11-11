package selfsync

import (
	"context"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/store"
)

// pageKey is the content key of the app of the pages in these tests.
var pageKey = [32]byte{7}

// sealedPages lets the device of the bench open its pages, in the format
// of the data roots (interface spec, section 3.9.4).
func (b *pageBench) sealedPages() {
	b.device.Opener = func(kind string) (draft.Opener, bool) {
		if kind != "30023" {
			return draft.Opener{}, false
		}
		return draft.Opener{Citizen: b.citizen.Public, Key: func(context.Context, uint32) ([32]byte, error) { return pageKey, nil }}, true
	}
}

// sealer writes pages of the citizen of the bench, signed by signer.
func (b *pageBench) sealer(signer nostr.SecretKey) draft.Sealer {
	return draft.Sealer{Citizen: b.citizen.Public, Signer: keyer.NewPlainKeySigner(signer), Key: pageKey}
}

// save keeps events in the store of the device, as a publish does.
func (b *pageBench) save(events ...nostr.Event) {
	b.t.Helper()
	for _, e := range events {
		if r, err := b.local.Save(e); err != nil || r.Outcome != store.Stored {
			b.t.Fatalf("the store took kind %d as %v: %v", e.Kind, r.Outcome, err)
		}
	}
}

// parts seals n parts of the page d.
func (b *pageBench) parts(s draft.Sealer, d string, n int, at nostr.Timestamp) []nostr.Event {
	b.t.Helper()
	var out []nostr.Event
	for i := range n {
		part, err := s.Part(context.Background(), d, string(rune('a'+i)), at)
		if err != nil {
			b.t.Fatal(err)
		}
		out = append(out, part)
	}
	return out
}

// version seals a draft and its checkpoint of the page d that names the
// parts in a parts tag.
func (b *pageBench) version(s draft.Sealer, d string, parts []nostr.Event, at nostr.Timestamp) (nostr.Event, nostr.Event) {
	b.t.Helper()
	tag := nostr.Tag{"parts"}
	for _, p := range parts {
		tag = append(tag, p.ID.Hex())
	}
	inner := nostr.Event{Kind: 30023, CreatedAt: at, Tags: nostr.Tags{{"d", d}, tag}, Content: "head"}
	wrap, err := s.Wrap(context.Background(), d, inner, at)
	if err != nil {
		b.t.Fatal(err)
	}
	checkpoint, err := s.Checkpoint(context.Background(), d, inner, at)
	if err != nil {
		b.t.Fatal(err)
	}
	return wrap, checkpoint
}

// keeps checks that the store of the device still holds each event.
func (b *pageBench) keeps(events ...nostr.Event) {
	b.t.Helper()
	for _, e := range events {
		got, err := b.local.Query(nostr.Filter{IDs: []nostr.ID{e.ID}})
		if err != nil || len(got) != 1 {
			b.t.Fatalf("the store lost the event of kind %d: %v", e.Kind, err)
		}
	}
}

// deletions counts the deletion requests in the store of the device.
func (b *pageBench) deletions() int {
	b.t.Helper()
	got, err := b.local.Query(nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}})
	if err != nil {
		b.t.Fatal(err)
	}
	return len(got)
}

// A write that stopped after its parts and before its draft leaves parts
// that no page names. An older arc gave them to the storage citizens. A
// repair deletes the parts that this device signed, once the storage
// citizens hold them, with a deletion that names each part. The next
// repair frees their blobs, and says so in the words of journal delete. A
// page whose draft names its parts stays (issue #379).
func TestRepairFreesPartsThatNoPageNames(t *testing.T) {
	b := newPageBench(t)
	s := b.sealer(b.citizen.Secret)
	at := nostr.Now() - 10
	live := b.parts(s, "live", 2, at)
	wrap, checkpoint := b.version(s, "live", live, at)
	b.save(append(live, wrap, checkpoint)...)
	orphans := b.parts(s, "crashed", 3, at)
	b.save(orphans...)

	// A device that does not open its pages sends each part, as an older
	// arc did.
	b.sync(b.device, false, b.sides)
	b.held(7, 7, 7)
	b.sealedPages()
	r := b.sync(b.device, true, b.sides)
	if r.Repair.UnnamedParts != 3 || r.Repair.UnnamedCheckpoints != 0 {
		t.Fatalf("the repair deleted %d parts and %d checkpoints that no page names; want 3 and 0", r.Repair.UnnamedParts, r.Repair.UnnamedCheckpoints)
	}
	for _, p := range orphans {
		if got, _ := b.local.Query(nostr.Filter{IDs: []nostr.ID{p.ID}}); len(got) != 0 {
			t.Fatal("a part that no page names stays in the store after the repair")
		}
	}
	b.keeps(append(live, wrap, checkpoint)...)
	// The deletion went to the storage citizens in the same sync.
	b.held(8, 8, 8)
	r = b.sync(b.device, true, b.sides)
	if r.Repair.Unnamed != 3 || r.Repair.Replaced != 0 || r.Repair.Down != 0 {
		t.Fatalf("the next repair freed %d blobs of unnamed parts and %d of replaced records, and got %d bytes; want 3, 0 and 0",
			r.Repair.Unnamed, r.Repair.Replaced, r.Repair.Down)
	}
	b.held(5, 5, 5)
}

// A deletion of many unnamed parts goes in several requests, each smaller
// than one event may be (store.MaxEvent). One request for 1,000 parts would
// be about 72 KiB: the write could not put it, and each later sync would
// fail on it.
func TestRepairSplitsTheDeletionOfManyParts(t *testing.T) {
	b := newPageBench(t)
	const n = 1000
	orphans := b.parts(b.sealer(b.citizen.Secret), "crashed", n, nostr.Now()-10)
	b.save(orphans...)

	// As an older arc did, the first sync sends each part.
	b.sync(b.device, false, b.sides)
	b.sealedPages()
	r := b.sync(b.device, true, b.sides)
	if r.Repair.UnnamedParts != n {
		t.Fatalf("the repair deleted %d parts that no page names; want %d", r.Repair.UnnamedParts, n)
	}
	requests, err := b.local.Query(nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) < 2 {
		t.Fatalf("the repair made %d deletions for %d parts; want more than one", len(requests), n)
	}
	for _, request := range requests {
		if size := len(request.String()); size > store.MaxEvent {
			t.Fatalf("a deletion is %d bytes; one event holds at most %d", size, store.MaxEvent)
		}
	}
	r = b.sync(b.device, true, b.sides)
	if r.Repair.Unnamed != n {
		t.Fatalf("the next repair freed %d blobs; want %d", r.Repair.Unnamed, n)
	}
	b.held(len(requests), len(requests), len(requests))
}

// A part of another device can arrive in a sync before the draft that
// names it. The repair keeps it.
func TestRepairKeepsPartsOfAnotherDevice(t *testing.T) {
	b := newPageBench(t)
	b.local.SetSigners(noList{})
	b.sealedPages()
	other := keys.Generate()
	parts := b.parts(b.sealer(other.Secret), "elsewhere", 2, nostr.Now()-10)
	b.save(parts...)

	b.sync(b.device, false, b.sides)
	for range 2 {
		r := b.sync(b.device, true, b.sides)
		if r.Repair.UnnamedParts != 0 || r.Repair.Unnamed != 0 {
			t.Fatalf("the repair deleted %d parts of another device and freed %d blobs", r.Repair.UnnamedParts, r.Repair.Unnamed)
		}
	}
	b.keeps(parts...)
	if n := b.deletions(); n != 0 {
		t.Fatalf("the repair made %d deletions for the parts of another device", n)
	}
	b.held(2, 2, 2)
}

// A part of an older version is named only by the checkpoint of that
// version. The repair keeps the part and the checkpoint: they are the
// history of the page (interface spec, section 3.9.1).
func TestRepairKeepsPartsThatACheckpointNames(t *testing.T) {
	b := newPageBench(t)
	b.sealedPages()
	s := b.sealer(b.citizen.Secret)
	at := nostr.Now() - 10
	old := b.parts(s, "page", 2, at)
	_, oldCheckpoint := b.version(s, "page", old, at)
	b.save(old...)
	b.save(oldCheckpoint)
	now := b.parts(s, "page", 1, at+1)
	wrap, checkpoint := b.version(s, "page", now, at+1)
	b.save(append(now, wrap, checkpoint)...)

	b.sync(b.device, false, b.sides)
	for range 2 {
		r := b.sync(b.device, true, b.sides)
		if r.Repair.UnnamedParts != 0 || r.Repair.UnnamedCheckpoints != 0 || r.Repair.Unnamed != 0 {
			t.Fatalf("the repair deleted %d parts and %d checkpoints of the history, and freed %d blobs",
				r.Repair.UnnamedParts, r.Repair.UnnamedCheckpoints, r.Repair.Unnamed)
		}
	}
	b.keeps(append(append(old, oldCheckpoint), append(now, wrap, checkpoint)...)...)
	if n := b.deletions(); n != 0 {
		t.Fatalf("the repair made %d deletions for the history of a page", n)
	}
}

// A write that still runs has kept its parts and not yet its draft. Its
// parts are newer than the last sync, so the repair keeps them. When the
// draft comes, it names them.
func TestRepairKeepsPartsOfAWriteThatRuns(t *testing.T) {
	b := newPageBench(t)
	b.sealedPages()
	s := b.sealer(b.citizen.Secret)
	b.sync(b.device, false, b.sides)
	at := nostr.Now()
	parts := b.parts(s, "page", 2, at)
	b.save(parts...)

	r := b.sync(b.device, true, b.sides)
	if r.Repair.UnnamedParts != 0 {
		t.Fatalf("the repair deleted %d parts of a write that runs", r.Repair.UnnamedParts)
	}
	b.keeps(parts...)
	wrap, checkpoint := b.version(s, "page", parts, at)
	b.save(wrap, checkpoint)
	r = b.sync(b.device, true, b.sides)
	if r.Repair.UnnamedParts != 0 || r.Repair.Unnamed != 0 {
		t.Fatalf("the repair deleted %d parts of the finished write and freed %d blobs", r.Repair.UnnamedParts, r.Repair.Unnamed)
	}
	b.keeps(append(parts, wrap, checkpoint)...)
	if n := b.deletions(); n != 0 {
		t.Fatalf("the repair made %d deletions for a write that ran", n)
	}
}

// noList is a citizen with no device list: the store admits each device.
type noList struct{}

func (noList) Standing(nostr.PubKey, nostr.PubKey) store.Standing {
	return store.Standing{NoList: true}
}

// A write that stops after its parts and before its draft, as with
// SIGKILL, leaves parts that no page names. No sync sends them to the
// storage citizens. A repair keeps them while a write could still run,
// and deletes them after unnamedAge. A write that still runs when a sync
// starts has its parts held back, and the next sync sends them with the
// draft (#397).
func TestAStoppedWriteSendsNoParts(t *testing.T) {
	b := newPageBench(t)
	b.sealedPages()
	s := b.sealer(b.citizen.Secret)
	now := time.Now()
	b.device.Now = func() time.Time { return now }
	at := nostr.Timestamp(now.Unix() - 10)
	live := b.parts(s, "live", 2, at)
	wrap, checkpoint := b.version(s, "live", live, at)
	b.save(append(live, wrap, checkpoint)...)
	orphans := b.parts(s, "crashed", 3, at)
	b.save(orphans...)
	running := b.parts(s, "running", 2, at)
	b.save(running...)

	b.sync(b.device, false, b.sides)
	b.held(4, 4, 4)
	r := b.sync(b.device, true, b.sides)
	if r.Repair.UnnamedParts != 0 {
		t.Fatalf("a repair deleted %d young parts that a write could still name", r.Repair.UnnamedParts)
	}
	// It says that it kept the 3 orphans and the 2 parts of the write
	// that runs, and not the parts of the page that its draft names.
	if r.Repair.UnnamedKept != 5 {
		t.Errorf("a repair kept %d young parts that no page names, want 5", r.Repair.UnnamedKept)
	}
	b.keeps(orphans...)
	b.held(4, 4, 4)

	// The running write keeps its draft: the next sync sends its parts.
	runningWrap, runningCheckpoint := b.version(s, "running", running, at)
	b.save(runningWrap, runningCheckpoint)
	b.sync(b.device, false, b.sides)
	b.held(8, 8, 8)

	now = now.Add(2 * unnamedAge)
	// A write that runs now, however slow, can hold parts of any age.
	b.device.Writing = func() (bool, error) { return true, nil }
	if r = b.sync(b.device, true, b.sides); r.Repair.UnnamedParts != 0 {
		t.Fatalf("a repair during a write deleted %d parts with no blob", r.Repair.UnnamedParts)
	}
	b.keeps(orphans...)
	b.device.Writing = func() (bool, error) { return false, nil }
	r = b.sync(b.device, true, b.sides)
	if r.Repair.UnnamedParts != 3 {
		t.Fatalf("a repair after unnamedAge deleted %d parts that no page names; want 3", r.Repair.UnnamedParts)
	}
	for _, p := range orphans {
		if got, _ := b.local.Query(nostr.Filter{IDs: []nostr.ID{p.ID}}); len(got) != 0 {
			t.Fatal("a part of the stopped write stays in the store")
		}
	}
	b.keeps(append(append(live, wrap, checkpoint), append(running, runningWrap, runningCheckpoint)...)...)
	// Only the deletion goes out: 8 records and 1 deletion.
	b.held(9, 9, 9)
}
