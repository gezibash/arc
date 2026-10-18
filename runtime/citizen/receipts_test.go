package citizen

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
	boltkv "github.com/gezibash/arc/adapters/kv/bolt"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/private"
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
