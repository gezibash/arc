package mail

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	boltkv "github.com/gezibash/arc/adapters/kv/bolt"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/node"
)

// hookedView runs after each read of a store, before the read returns.
type hookedView struct {
	kv.Store
	after func()
}

func (h hookedView) View(fn func(kv.Tx) error) error {
	err := h.Store.View(fn)
	h.after()
	return err
}

// failedView is a store whose reads fail.
type failedView struct{ kv.Store }

func (failedView) View(func(kv.Tx) error) error { return errors.New("the disk failed") }

// readFirstMail is a mail on a store that wrap wraps, and the store itself.
func readFirstMail(t *testing.T, wrap func(kv.Store) kv.Store) (*Mail, kv.Store) {
	t.Helper()
	db := boltkv.Open(filepath.Join(t.TempDir(), "mail.db"))
	t.Cleanup(db.Close)
	m, err := New(wrap(db), keys.Generate(), &node.Node{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m, db
}

func outgoing(t *testing.T, db kv.Store, key string) Outgoing {
	t.Helper()
	var out Outgoing
	if err := db.View(func(tx kv.Tx) error { return json.Unmarshal(tx.Bucket(outboxBucket).Get([]byte(key)), &out) }); err != nil {
		t.Fatal(err)
	}
	return out
}

// A message that is delivered between the read and the write of attempted
// gets no new attempt: the write checks again.
func TestADeliveryBetweenTheReadAndTheWriteAddsNoAttempt(t *testing.T) {
	const key = "rumor:recipient"
	deliver := func() {}
	m, db := readFirstMail(t, func(s kv.Store) kv.Store { return hookedView{s, func() { deliver() }} })
	if err := db.Update(func(tx kv.Tx) error { return put(tx, outboxBucket, key, Outgoing{}) }); err != nil {
		t.Fatal(err)
	}
	deliver = func() {
		deliver = func() {}
		err := db.Update(func(tx kv.Tx) error {
			return put(tx, outboxBucket, key, Outgoing{Delivered: time.Unix(1, 0)})
		})
		if err != nil {
			t.Error(err)
		}
	}
	if err := m.attempted(key); err != nil {
		t.Fatal(err)
	}
	if got := outgoing(t, db, key).Attempts; got != 0 {
		t.Errorf("a message delivered before the write got %d attempts, want 0", got)
	}
}

// A read that fails in attempted or in unhold is an error, not a reason to
// skip the write.
func TestAReadThatFailsBeforeAWriteIsAnError(t *testing.T) {
	m, _ := readFirstMail(t, func(s kv.Store) kv.Store { return failedView{s} })
	if err := m.attempted("rumor:recipient"); err == nil {
		t.Error("attempted gave no error after a failed read")
	}
	if err := m.unhold(nostr.ID{1}); err == nil {
		t.Error("unhold gave no error after a failed read")
	}
}
