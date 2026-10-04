package citizen

import (
	"context"
	"encoding/binary"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
)

// receiptBucket holds the order in which this machine first stored each seal
// from another citizen. A key is the ID of a seal, and its value is a
// position, 8 bytes big-endian. The key "next" holds the next position.
var (
	receiptBucket = []byte("receipts")
	receiptNext   = []byte("next")
)

// receipts records the order in which this machine stores the seals of other
// citizens. The clock of a sender does not change this order. Each arc
// command that opens a home records it, because Open composes it.
type receipts struct {
	node.EventStore
	db kv.Store
	me nostr.PubKey
}

// Save records the receipt, and then saves the event. If the process stops
// between the two writes, a receipt with no seal stays. It matches no
// message.
func (r receipts) Save(event nostr.Event) (store.Result, error) {
	if event.Kind == private.SealKind && event.PubKey != r.me {
		err := r.db.Update(func(tx kv.Tx) error {
			b, err := tx.CreateBucketIfNotExists(receiptBucket)
			if err != nil {
				return err
			}
			if b.Get(event.ID[:]) != nil {
				return nil
			}
			next := uint64(1)
			if value := b.Get(receiptNext); value != nil {
				next = binary.BigEndian.Uint64(value)
			}
			if err := b.Put(receiptNext, binary.BigEndian.AppendUint64(nil, next+1)); err != nil {
				return err
			}
			return b.Put(event.ID[:], binary.BigEndian.AppendUint64(nil, next))
		})
		if err != nil {
			return store.Result{}, err
		}
	}
	return r.EventStore.Save(event)
}

// ReceiptOrder gives the position in which this machine first stored each
// received private event, by the ID of its rumor. A higher position arrived
// later. An event has no position if this machine stored it before arc kept
// receipts.
func (s *Session) ReceiptOrder(ctx context.Context) (map[string]uint64, error) {
	positions := map[nostr.ID]uint64{}
	err := s.receipts.View(func(tx kv.Tx) error {
		b := tx.Bucket(receiptBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(key, value []byte) error {
			if len(key) == len(nostr.ID{}) && len(value) == 8 {
				positions[nostr.ID(key)] = binary.BigEndian.Uint64(value)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	seals, err := s.Node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{private.SealKind}})
	if err != nil {
		return nil, err
	}
	order := map[string]uint64{}
	for _, seal := range seals {
		position, ok := positions[seal.ID]
		if !ok || seal.PubKey == s.Signer.PublicKey() {
			continue
		}
		rumor, err := private.OpenSeal(ctx, s.Signer, seal)
		if err != nil {
			return nil, err
		}
		if held, ok := order[rumor.ID.Hex()]; !ok || position < held {
			order[rumor.ID.Hex()] = position
		}
	}
	return order, nil
}
