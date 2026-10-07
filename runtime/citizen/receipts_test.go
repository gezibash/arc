package citizen

import (
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
