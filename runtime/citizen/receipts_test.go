package citizen

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	boltkv "github.com/gezibash/arc/adapters/kv/bolt"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
)

// A sync saves a fetch in one batch. The batch must still record the
// receipt of each seal from another citizen, or the inbox loses its order.
func TestABatchRecordsTheReceiptOfEachSeal(t *testing.T) {
	dir := t.TempDir()
	events, err := boltstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	receipts := boltkv.Open(filepath.Join(dir, "receipts.db"))
	defer receipts.Close()
	r := receiptStore{EventStore: events, db: receipts, me: nostr.Generate().Public()}

	other := nostr.Generate()
	var seals []nostr.Event
	for _, text := range []string{"one", "two"} {
		seal := nostr.Event{Kind: private.SealKind, CreatedAt: nostr.Now(), Content: text}
		if err := seal.Sign(other); err != nil {
			t.Fatal(err)
		}
		seals = append(seals, seal)
	}
	if _, err := r.SaveAll(seals); err != nil {
		t.Fatal(err)
	}

	err = receipts.View(func(tx kv.Tx) error {
		b := tx.Bucket(receiptBucket)
		for i, seal := range seals {
			if b == nil || b.Get(seal.ID[:]) == nil {
				t.Errorf("seal %d has no receipt", i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// countingStore counts the write transactions that reach a store.
type countingStore struct {
	kv.Store
	updates int
}

func (c *countingStore) Update(fn func(kv.Tx) error) error {
	c.updates++
	return c.Store.Update(fn)
}

// Each write transaction of bolt ends with an fsync, even one that changes
// nothing. A sync saves again each seal that the relays send, so a seal
// that already has its receipt must cost no write transaction.
func TestAKnownSealCostsNoWrite(t *testing.T) {
	dir := t.TempDir()
	events, err := boltstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	receipts := boltkv.Open(filepath.Join(dir, "receipts.db"))
	defer receipts.Close()
	counted := &countingStore{Store: receipts}
	r := receiptStore{EventStore: events, db: counted, me: nostr.Generate().Public()}

	other := nostr.Generate()
	seal := nostr.Event{Kind: private.SealKind, CreatedAt: nostr.Now(), Content: "one"}
	if err := seal.Sign(other); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Save(seal); err != nil {
		t.Fatal(err)
	}
	if counted.updates != 1 {
		t.Fatalf("the first save of a seal made %d write transactions, want 1", counted.updates)
	}
	for range 3 {
		if _, err := r.Save(seal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.SaveAll([]nostr.Event{seal}); err != nil {
		t.Fatal(err)
	}
	if counted.updates != 1 {
		t.Errorf("4 more saves of the same seal made %d more write transactions, want 0", counted.updates-1)
	}

	// A new seal still gets the next position.
	second := nostr.Event{Kind: private.SealKind, CreatedAt: nostr.Now(), Content: "two"}
	if err := second.Sign(other); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Save(second); err != nil {
		t.Fatal(err)
	}
	err = receipts.View(func(tx kv.Tx) error {
		b := tx.Bucket(receiptBucket)
		for want, s := range []nostr.Event{seal, second} {
			got := b.Get(s.ID[:])
			if got == nil || binary.BigEndian.Uint64(got) != uint64(want+1) {
				t.Errorf("seal %d has position %v, want %d", want+1, got, want+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// openReceipts is a receipt store on bolt files in a new directory, with
// db wrapped by wrap.
func openReceipts(t *testing.T, wrap func(kv.Store) kv.Store) (receiptStore, kv.Store, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	events, err := boltstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(events.Close)
	receipts := boltkv.Open(filepath.Join(dir, "receipts.db"))
	t.Cleanup(receipts.Close)
	return receiptStore{EventStore: events, db: wrap(receipts), me: nostr.Generate().Public()}, receipts, events
}

func sealFrom(t *testing.T, author nostr.SecretKey, text string) nostr.Event {
	t.Helper()
	seal := nostr.Event{Kind: private.SealKind, CreatedAt: nostr.Now(), Content: text}
	if err := seal.Sign(author); err != nil {
		t.Fatal(err)
	}
	return seal
}

// A receipt with no seal stays when arc stops between the two writes. The
// next save of that seal, one by one or in a batch, must save the seal: a
// known receipt is no reason to skip the event.
func TestASealWhoseReceiptCameFirstIsStillSaved(t *testing.T) {
	plain := func(s kv.Store) kv.Store { return s }
	for _, batch := range []bool{false, true} {
		r, _, events := openReceipts(t, plain)
		seal := sealFrom(t, nostr.Generate(), "one")
		if err := r.receipt(seal); err != nil { // arc stopped here
			t.Fatal(err)
		}
		var err error
		if batch {
			_, err = r.SaveAll([]nostr.Event{seal})
		} else {
			_, err = r.Save(seal)
		}
		if err != nil {
			t.Fatal(err)
		}
		if has, err := events.Has(seal.ID); err != nil || !has {
			t.Errorf("batch %v: after a receipt with no seal, the save left no seal (%v)", batch, err)
		}
	}
}

// failingView is a store whose reads fail.
type failingView struct{ kv.Store }

func (failingView) View(func(kv.Tx) error) error { return errors.New("the disk failed") }

// A receipt that cannot be read is an error, not a known receipt or a new
// one.
func TestAReceiptThatCannotBeReadIsAnError(t *testing.T) {
	r, _, events := openReceipts(t, func(s kv.Store) kv.Store { return failingView{s} })
	seal := sealFrom(t, nostr.Generate(), "one")
	if _, err := r.Save(seal); err == nil || !strings.Contains(err.Error(), "the disk failed") {
		t.Fatalf("Save gave %v, want the read error", err)
	}
	if has, _ := events.Has(seal.ID); has {
		t.Error("the seal was saved with no receipt")
	}
}

// gatedView lets each read through only when two reads have come, so two
// saves of one seal both read before either writes.
type gatedView struct {
	kv.Store
	both *sync.WaitGroup
}

func (g gatedView) View(fn func(kv.Tx) error) error {
	err := g.Store.View(fn)
	g.both.Done()
	g.both.Wait()
	return err
}

// Two saves of one seal that both read before they write, as two arc
// processes can, give the seal one position: the write checks again.
func TestTwoSavesOfASealGiveItOnePosition(t *testing.T) {
	both := &sync.WaitGroup{}
	both.Add(2)
	r, receipts, _ := openReceipts(t, func(s kv.Store) kv.Store { return gatedView{s, both} })
	seal := sealFrom(t, nostr.Generate(), "one")
	var saves sync.WaitGroup
	for range 2 {
		saves.Go(func() {
			if err := r.receipt(seal); err != nil {
				t.Error(err)
			}
		})
	}
	saves.Wait()
	err := receipts.View(func(tx kv.Tx) error {
		b := tx.Bucket(receiptBucket)
		position, next := binary.BigEndian.Uint64(b.Get(seal.ID[:])), binary.BigEndian.Uint64(b.Get(receiptNext))
		if position != 1 || next != 2 {
			t.Errorf("the seal has position %d and next is %d, want 1 and 2", position, next)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
