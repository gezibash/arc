package selfsync

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/sdk/storage"
)

// putOld puts a whole blob of format 1 of each record on each storage app
// given, with no tag, as a device before the pieces did.
func (b *repairBench) putOld(events []nostr.Event, stores ...*memStorage) {
	b.t.Helper()
	for _, e := range events {
		id, blob, err := Seal(testKeys(b.t, 0), b.citizen.Secret, e, nil)
		if err != nil {
			b.t.Fatal(err)
		}
		for _, s := range stores {
			if _, err := s.PutMany(context.Background(), []storage.Blob{{ID: id, Data: blob}}); err != nil {
				b.t.Fatal(err)
			}
		}
	}
}

func (b *repairBench) events() []nostr.Event {
	var out []nostr.Event
	for _, e := range b.local.events {
		out = append(out, e)
	}
	return out
}

// whole is the number of present entries with no tag on a storage app.
func whole(s *memStorage) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.entries {
		if !e.Deleted && e.Tag == "" {
			n++
		}
	}
	return n
}

// failing is a storage app whose puts or deletes fail, as a pipe that
// ends in the middle of a sync.
type failing struct {
	*memStorage
	puts, deletes bool
}

func (f *failing) PutMany(ctx context.Context, blobs []storage.Blob) ([]storage.PutResult, error) {
	if f.puts {
		return nil, errors.New("the pipe ended")
	}
	return f.memStorage.PutMany(ctx, blobs)
}

func (f *failing) DeleteMany(ctx context.Context, ids []string) ([]storage.Item, error) {
	if f.deletes {
		return nil, errors.New("the pipe ended")
	}
	return f.memStorage.DeleteMany(ctx, ids)
}

// syncSome syncs and accepts the failure of a side.
func (b *repairBench) syncSome(s *Syncer, sides []Side) Report {
	b.t.Helper()
	r, err := s.SyncAll(context.Background(), sides)
	if err != nil {
		b.t.Fatal(err)
	}
	return r
}

// TestRepairFormat1 checks section 4.6 of the storage spec for blobs of
// format 1 on two storage citizens: a sync with no repair counts them and
// changes nothing; a repair codes each one into pieces from the record,
// with no download, and deletes the whole copies. Then each storage
// citizen holds pieces only, and a new device reads each record.
func TestRepairFormat1(t *testing.T) {
	b := newRepairBench(t, 4, 2)
	b.putOld(b.events(), b.stores...)
	b.device.Repair = false
	r := b.sync(b.device, b.sides)
	if r.Repair.Old != 4 || r.Repair.Pending != 4 || whole(b.stores[0]) != 4 || whole(b.stores[1]) != 4 {
		t.Fatalf("a sync with no repair: %d blobs of format 1 remain, %d need repair; want 4 and 4, and no change", r.Repair.Old, r.Repair.Pending)
	}
	b.device.Repair = true
	r = b.sync(b.device, b.sides)
	if r.Repair.Coded != 4 || r.Repair.Old != 0 || r.Repair.Down != 0 || r.Repair.Deleted != 8 {
		t.Fatalf("the repair coded %d blobs of format 1, left %d, got %d bytes and deleted %d; want 4, 0, 0 and 8", r.Repair.Coded, r.Repair.Old, r.Repair.Down, r.Repair.Deleted)
	}
	for i, s := range b.stores {
		if whole(s) != 0 || len(s.data) != 4 {
			t.Fatalf("storage citizen %d holds %d whole blobs and %d entries; want pieces only, 4", i, whole(s), len(s.data))
		}
	}
	b.reads(only(b.sides, 0))
	b.reads(only(b.sides, 1))
	if r := b.sync(b.device, b.sides); r.Repair.Coded != 0 || r.Repair.Deleted != 0 || r.Repair.Old != 0 {
		t.Fatalf("a second repair coded %d and deleted %d", r.Repair.Coded, r.Repair.Deleted)
	}
}

// TestRepairFormat1Interrupted checks that a move to pieces that stops
// loses nothing, and that the next repair ends it: a pipe that ends before
// its puts deletes no whole copy; a pipe that ends before its deletes
// leaves the whole copy beside the pieces. A new device reads each record
// at each point.
func TestRepairFormat1Interrupted(t *testing.T) {
	b := newRepairBench(t, 3, 2)
	b.putOld(b.events(), b.stores...)
	broken := &failing{memStorage: b.stores[1], puts: true}
	sides := slices.Clone(b.sides)
	sides[1].Conn = broken
	r := b.syncSome(b.device, sides)
	if r.Repair.Coded != 0 || whole(b.stores[0]) != 3 || whole(b.stores[1]) != 3 {
		t.Fatalf("with the puts of one side failed, the repair coded %d blobs, and left %d and %d whole copies; want 0, 3 and 3", r.Repair.Coded, whole(b.stores[0]), whole(b.stores[1]))
	}
	b.reads(only(b.sides, 0))
	b.reads(only(b.sides, 1))

	broken.puts, broken.deletes = false, true
	r = b.syncSome(b.device, sides)
	if whole(b.stores[1]) != 3 || r.Repair.Old != 3 {
		t.Fatalf("with the deletes of one side failed, %d whole copies stay there, and the sync counts %d; want 3 and 3", whole(b.stores[1]), r.Repair.Old)
	}
	b.reads(only(b.sides, 0))
	b.reads(only(b.sides, 1))

	r = b.sync(b.device, b.sides)
	if r.Repair.Old != 0 || whole(b.stores[0]) != 0 || whole(b.stores[1]) != 0 {
		t.Fatalf("the next repair left %d blobs of format 1", r.Repair.Old)
	}
	if got := b.held(); !slices.Equal(got, []int{3, 3}) {
		t.Fatalf("after the move the storage citizens hold %v pieces; want 3 each", got)
	}
	b.reads(only(b.sides, 0))
	b.reads(only(b.sides, 1))
}

// TestRepairFormat1Drain checks that the whole copies of a draining storage
// citizen move to pieces on the active ones, and leave it empty. One copy
// is of a record that the device does not hold, so the device codes the
// copy itself. A copy with a changed byte on another storage citizen is
// not coded, and is deleted.
func TestRepairFormat1Drain(t *testing.T) {
	b := newRepairBench(t, 2, 3)
	b.putOld(b.events(), b.stores[2])
	// A record of another kind: the store refuses it, so the device codes
	// the whole copy.
	other := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: "not self data"}
	if err := other.Sign(b.citizen.Secret); err != nil {
		t.Fatal(err)
	}
	b.putOld([]nostr.Event{other}, b.stores[1], b.stores[2])
	id := testKeys(t, 0).BlobID(other.ID)
	good := slices.Clone(b.stores[2].data[id])
	b.stores[1].mu.Lock()
	b.stores[1].data[id] = slices.Clone(b.stores[1].data[id])
	b.stores[1].data[id][100] ^= 1
	b.stores[1].mu.Unlock()
	sides := slices.Clone(b.sides)
	sides[2].Active = false
	r := b.sync(b.device, sides)
	if r.Repair.Coded != 3 || r.Repair.Old != 0 || !r.Sides[2].Empty {
		t.Fatalf("the drain coded %d blobs of format 1 and left %d; empty %v; refused %v", r.Repair.Coded, r.Repair.Old, r.Sides[2].Empty, r.Refused)
	}
	if whole(b.stores[1]) != 0 || len(b.stores[2].data) != 0 {
		t.Fatal("a whole copy stays")
	}
	if got := b.held(); !slices.Equal(got, []int{3, 3, 0}) {
		t.Fatalf("after the drain the storage citizens hold %v pieces", got)
	}
	// The piece of the other record holds the good copy.
	k := testKeys(t, 0)
	found := false
	for pid, e := range b.stores[0].entries {
		tag, _ := base64.StdEncoding.DecodeString(e.Tag)
		h, err := OpenTag(k, tag)
		if err != nil || h.Blob != id {
			continue
		}
		shard, err := OpenPiece(k, tag, h, b.stores[0].data[pid])
		if err != nil || !bytes.Equal(shard, good) {
			t.Fatalf("the piece of the whole copy does not hold the good copy: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatal("no piece of the whole copy with no record")
	}
	b.reads(only(sides, 0))
	b.reads(only(sides, 1))
}

// TestRepairFormat1Bound checks that one sync moves at most migrateMax
// blobs of format 1, and says how many remain. The next sync moves the
// rest.
func TestRepairFormat1Bound(t *testing.T) {
	b := newRepairBench(t, migrateMax+5, 1)
	b.putOld(b.events(), b.stores...)
	r := b.sync(b.device, b.sides)
	if r.Repair.Coded != migrateMax || r.Repair.Old != 5 {
		t.Fatalf("the first sync coded %d blobs of format 1 and left %d; want %d and 5", r.Repair.Coded, r.Repair.Old, migrateMax)
	}
	r = b.sync(b.device, b.sides)
	if r.Repair.Coded != 5 || r.Repair.Old != 0 {
		t.Fatalf("the second sync coded %d and left %d; want 5 and 0", r.Repair.Coded, r.Repair.Old)
	}
	if whole(b.stores[0]) != 0 {
		t.Fatal(fmt.Sprint(whole(b.stores[0]), " whole copies stay"))
	}
}
