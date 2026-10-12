package selfsync

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
)

// repairBench is a citizen with records in a local store, and storage
// apps in memory.
type repairBench struct {
	t       *testing.T
	citizen keys.Key
	local   *memStore
	stores  []*memStorage
	sides   []Side
	device  *Syncer
	now     time.Time
}

func newRepairBench(t *testing.T, records, storages int) *repairBench {
	t.Helper()
	b := &repairBench{t: t, citizen: keys.Generate(), local: &memStore{events: map[nostr.ID]nostr.Event{}}, now: time.Unix(1_800_000_000, 0)}
	for n := range records {
		e := nostr.Event{Kind: draft.Kind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", fmt.Sprintf("page/%d", n)}}, Content: fmt.Sprintf("page %d", n)}
		if err := e.Sign(b.citizen.Secret); err != nil {
			t.Fatal(err)
		}
		b.local.events[e.ID] = e
	}
	for i := range storages {
		b.stores = append(b.stores, newMemStorage())
		b.sides = append(b.sides, Side{Key: keys.Generate().Public, Label: fmt.Sprint("s", i), Conn: b.stores[i], Active: true})
	}
	b.device = testSyncer(t, b.citizen, b.local)
	b.device.Repair = true
	b.device.Now = func() time.Time { return b.now }
	return b
}

func (b *repairBench) sync(s *Syncer, sides []Side) Report {
	b.t.Helper()
	r, err := s.SyncAll(context.Background(), sides)
	if err != nil {
		b.t.Fatal(err)
	}
	for i, side := range r.Sides {
		if side.Err != nil {
			b.t.Fatalf("side %d: %v", i, side.Err)
		}
	}
	return r
}

// held is the number of pieces that each storage app holds now.
func (b *repairBench) held() []int {
	out := make([]int, len(b.stores))
	for i, s := range b.stores {
		out[i] = len(s.data)
	}
	return out
}

// reads checks that a new device reads each record from the sides given.
func (b *repairBench) reads(sides []Side) {
	b.t.Helper()
	reader := testSyncer(b.t, b.citizen, &memStore{events: map[nostr.ID]nostr.Event{}})
	r := b.sync(reader, sides)
	if r.Received != len(b.local.events) || len(r.Unreadable) != 0 {
		b.t.Fatalf("a new device took %d of %d records; unreadable %v, refused %v", r.Received, len(b.local.events), r.Unreadable, r.Refused)
	}
}

// only returns the sides with the pipes of the indices given; each other
// side has no pipe.
func only(sides []Side, open ...int) []Side {
	out := slices.Clone(sides)
	for i := range out {
		if !slices.Contains(open, i) {
			out[i].Conn = nil
		}
	}
	return out
}

// TestRepairGone checks section 4.6 of the storage spec for a storage
// citizen that the user marks gone: a sync without repair rebuilds
// nothing and counts the blobs; a repair puts the missing piece of each
// blob on the free storage citizen; a new device reads each record from
// two storage citizens, one of them the new one.
func TestRepairGone(t *testing.T) {
	b := newRepairBench(t, 3, 4)
	// Three storage citizens: k = 2, t = 3. The fourth joins later.
	b.sync(b.device, b.sides[:3])
	if got := b.held(); !slices.Equal(got, []int{3, 3, 3, 0}) {
		t.Fatalf("the first sync left %v pieces", got)
	}
	// s0 is gone: S = 3 again, and each blob has 2 pieces of 3.
	gone := b.sides[1:]
	phone := *b.device
	phone.Repair = false
	r := b.sync(&phone, gone)
	if r.Repair.Pending != 3 || b.stores[3].puts != 0 {
		t.Fatalf("a sync without repair put %d pieces, and counted %d blobs to repair; want none and 3", b.stores[3].puts, r.Repair.Pending)
	}
	r = b.sync(b.device, gone)
	if r.Repair.Rebuilt != 3 || b.stores[3].puts != 3 {
		t.Fatalf("the repair rebuilt %d pieces, and put %d on the new storage citizen; want 3 and 3", r.Repair.Rebuilt, b.stores[3].puts)
	}
	// k = 2: each rebuilt piece costs 2 pieces down and one up.
	piece := int64(len(b.stores[3].data[slices.Collect(maps.Keys(b.stores[3].data))[0]]))
	if r.Repair.Down != 6*piece || r.Repair.Up != 3*piece {
		t.Fatalf("the repair moved %d bytes down and %d up; want %d and %d", r.Repair.Down, r.Repair.Up, 6*piece, 3*piece)
	}
	b.reads(only(b.sides, 2, 3))
	// The next repair has nothing to do.
	if r = b.sync(b.device, gone); r.Repair != (RepairReport{}) {
		t.Fatalf("a second repair did %+v", r.Repair)
	}
}

// TestRepairTURN checks decision 7: repair waits for a direct path, and
// says so. With TURN allowed, it runs.
func TestRepairTURN(t *testing.T) {
	b := newRepairBench(t, 2, 4)
	b.sync(b.device, b.sides[:3])
	gone := slices.Clone(b.sides[1:])
	gone[2].Relayed = true
	r := b.sync(b.device, gone)
	if b.stores[3].puts != 0 || r.Repair.WaitDirect != 2 {
		t.Fatalf("a repair through TURN put %d pieces and counted %d blobs that wait; want 0 and 2", b.stores[3].puts, r.Repair.WaitDirect)
	}
	b.device.TURN = true
	r = b.sync(b.device, gone)
	if b.stores[3].puts != 2 || r.Repair.WaitDirect != 0 {
		t.Fatalf("a repair with TURN allowed put %d pieces; want 2", b.stores[3].puts)
	}
}

// corrupt changes one byte of n pieces of a storage app.
func corrupt(s *memStorage, n int) {
	ids := slices.Sorted(maps.Keys(s.data))
	for _, id := range ids[:n] {
		s.data[id][len(s.data[id])/2] ^= 1
	}
}

// TestRepairAudit checks the audits: a changed piece fails the next audit,
// with no download for a piece whose proof the device kept, and with a
// get and an open of a piece whose proof it spent. Repair replaces it. As
// each storage citizen holds a piece of each blob, the replacement needs
// a new seal: the same index on the same storage citizen has the same ID.
// Three failures in one day mark the storage citizen as suspect.
func TestRepairAudit(t *testing.T) {
	b := newRepairBench(t, 4, 3)
	b.sync(b.device, b.sides)
	// The first audit runs at once and spends each kept proof.
	if r := b.sync(b.device, b.sides); r.Sides[0].Audited != 4 || r.Sides[0].AuditFailed != 0 || b.stores[0].audits != 4 {
		t.Fatalf("the first audit checked %d pieces, %d failed, %d by proof; want 4, 0 and 4", r.Sides[0].Audited, r.Sides[0].AuditFailed, b.stores[0].audits)
	}
	corrupt(b.stores[1], 1)
	// No audit before a day passes.
	if r := b.sync(b.device, b.sides); r.Sides[1].Audited != 0 {
		t.Fatalf("an audit ran %d checks within a day", r.Sides[1].Audited)
	}
	b.now = b.now.Add(AuditEvery)
	r := b.sync(b.device, b.sides)
	if r.Sides[1].Audited != 4 || r.Sides[1].AuditFailed != 1 || r.Sides[1].Suspect {
		t.Fatalf("the audit of s1 checked %d pieces, %d failed, suspect %v; want 4, 1, false", r.Sides[1].Audited, r.Sides[1].AuditFailed, r.Sides[1].Suspect)
	}
	if r.Repair.Sealed != 1 || r.Repair.Deleted != 3 {
		t.Fatalf("the repair sealed %d records again and deleted %d pieces; want 1 and 3", r.Repair.Sealed, r.Repair.Deleted)
	}
	if got := b.held(); !slices.Equal(got, []int{4, 4, 4}) {
		t.Fatalf("after the repair the storage citizens hold %v pieces", got)
	}
	b.reads(only(b.sides, 0, 1))
	b.reads(only(b.sides, 1, 2))

	// Two more changed pieces on the same day: suspect.
	b.now = b.now.Add(time.Hour)
	st, _ := b.device.load(b.sides[1].Key.Hex())
	st.Audited = 0
	_ = b.device.save(b.sides[1].Key.Hex(), st)
	corrupt(b.stores[1], 2)
	r = b.sync(b.device, b.sides)
	if r.Sides[1].AuditFailed != 2 || r.Sides[1].FailedToday != 3 || !r.Sides[1].Suspect {
		t.Fatalf("the second audit of s1: %d failed, %d today, suspect %v; want 2, 3, true", r.Sides[1].AuditFailed, r.Sides[1].FailedToday, r.Sides[1].Suspect)
	}
	b.reads(only(b.sides, 1, 2))
	// The next day starts the count again.
	b.now = b.now.Add(AuditEvery)
	if r = b.sync(b.device, b.sides); r.Sides[1].Suspect || r.Sides[1].FailedToday != 0 {
		t.Fatalf("the next day s1 is suspect %v with %d failures", r.Sides[1].Suspect, r.Sides[1].FailedToday)
	}
}

// TestRepairRule2 checks rule 2 of section 6.7 of the proposal: with 2
// of the 3 pieces of a blob lost, no seal has k pieces. The device seals
// the record again, puts 3 new pieces, and deletes the old ones. The new
// pieces have new IDs, so no put gets held or tombstoned.
func TestRepairRule2(t *testing.T) {
	b := newRepairBench(t, 1, 3)
	b.sync(b.device, b.sides)
	corrupt(b.stores[0], 1)
	corrupt(b.stores[1], 1)
	r := b.sync(b.device, b.sides)
	if r.Repair.Sealed != 1 || r.Repair.Deleted != 3 || r.Repair.Down != 0 {
		t.Fatalf("rule 2 sealed %d records again, deleted %d pieces and got %d bytes; want 1, 3 and 0", r.Repair.Sealed, r.Repair.Deleted, r.Repair.Down)
	}
	for i, s := range b.stores {
		if s.puts != 2 || s.deletes != 1 {
			t.Fatalf("s%d took %d puts and %d deletes; want 2 and 1", i, s.puts, s.deletes)
		}
	}
	b.reads(only(b.sides, 0, 1))
}

// TestRepairDrain checks the drain: the piece of a draining storage
// citizen moves, with the same bytes, to the active one that holds no
// piece of the blob, and goes from the draining one. Then the draining one
// holds nothing, and the device says so.
func TestRepairDrain(t *testing.T) {
	b := newRepairBench(t, 3, 4)
	b.sync(b.device, b.sides[:3])
	before := slices.Collect(maps.Values(b.stores[2].data))
	sides := slices.Clone(b.sides)
	sides[2].Active = false
	r := b.sync(b.device, sides)
	if r.Repair.Moved != 3 || r.Repair.Rebuilt != 0 || r.Repair.Deleted != 3 {
		t.Fatalf("the drain moved %d pieces, rebuilt %d and deleted %d; want 3, 0 and 3", r.Repair.Moved, r.Repair.Rebuilt, r.Repair.Deleted)
	}
	if got := b.held(); !slices.Equal(got, []int{3, 3, 0, 3}) {
		t.Fatalf("after the drain the storage citizens hold %v pieces", got)
	}
	for _, data := range before {
		if !slices.ContainsFunc(slices.Collect(maps.Values(b.stores[3].data)), func(d []byte) bool { return bytes.Equal(d, data) }) {
			t.Fatal("a piece of the draining storage citizen did not move with the same bytes")
		}
	}
	if !r.Sides[2].Empty {
		t.Fatal("the sync does not say that the draining storage citizen is empty")
	}
	b.reads(only(sides, 0, 3))
	b.reads(only(sides, 1, 3))
}

// TestRepairExtraPiece checks the extra piece of two repairs at the same
// time: one index on two storage citizens. The next repair deletes the
// piece on the storage citizen of the lower rank.
func TestRepairExtraPiece(t *testing.T) {
	b := newRepairBench(t, 1, 4)
	b.sync(b.device, b.sides)
	// Copy the piece of s0 to s1 under the ID that s1 has for it.
	k := testKeys(t, 0)
	id := slices.Collect(maps.Keys(b.stores[0].data))[0]
	tag, _ := base64.StdEncoding.DecodeString(b.stores[0].entries[id].Tag)
	h, err := OpenTag(k, tag)
	if err != nil {
		t.Fatal(err)
	}
	idKey, _ := k.PieceIDKey(b.sides[1].Key)
	copied := PieceID(idKey, h.Blob, h.Seal, h.Index)
	b.stores[1].mu.Lock()
	b.stores[1].seq++
	b.stores[1].data[copied] = b.stores[0].data[id]
	b.stores[1].entries[copied] = b.stores[0].entries[id]
	e := b.stores[1].entries[copied]
	e.ID, e.Seq = copied, b.stores[1].seq
	b.stores[1].entries[copied] = e
	b.stores[1].mu.Unlock()

	r := b.sync(b.device, b.sides)
	if r.Repair.Deleted != 1 {
		t.Fatalf("the repair deleted %d pieces; want the 1 extra piece", r.Repair.Deleted)
	}
	order := k.Rank(h.Blob, []nostr.PubKey{b.sides[0].Key, b.sides[1].Key})
	lower := 1
	if order[0] == b.sides[1].Key {
		lower = 0
	}
	if b.stores[lower].deletes != 1 {
		t.Fatalf("the repair deleted the piece of the storage citizen of the higher rank")
	}
}

// TestRepairMirror checks k = 1 with the record on the device: repair seals
// the record again, and puts the whole blob with no download.
func TestRepairMirror(t *testing.T) {
	b := newRepairBench(t, 3, 3)
	b.sync(b.device, b.sides[:2])
	r := b.sync(b.device, b.sides[1:])
	if r.Repair.Rebuilt != 3 || r.Repair.Down != 0 || b.stores[2].puts != 3 {
		t.Fatalf("the repair of a mirror rebuilt %d pieces with %d bytes down; want 3 with none", r.Repair.Rebuilt, r.Repair.Down)
	}
	b.reads(only(b.sides, 2))
}
