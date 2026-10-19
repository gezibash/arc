package mail_test

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"fiatjaf.com/nostr"
	boltkv "github.com/gezibash/arc/adapters/kv/bolt"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
)

// countedStore counts the write transactions that reach a store.
type countedStore struct {
	kv.Store
	updates atomic.Int64
}

func (c *countedStore) Update(fn func(kv.Tx) error) error {
	c.updates.Add(1)
	return c.Store.Update(fn)
}

// rereadWrites is the number of write transactions of a sync that reads
// again n messages from a contact, all of them known: a filtering citizen
// admits each and tries to unhold it.
func rereadWrites(t *testing.T, n int) int64 {
	t.Helper()
	alice := newCitizen(t)
	courier := stick(t)

	dir := t.TempDir()
	events, err := boltstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(events.Close)
	db := &countedStore{Store: boltkv.Open(filepath.Join(dir, "mail.db"))}
	t.Cleanup(db.Close)
	key := keys.Generate()
	m, err := mail.New(db, key, &node.Node{Store: events}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Contacts = &contactList{keys: []nostr.PubKey{alice.key.Public}}
	bob := citizen{dir: dir, key: key, node: &node.Node{Store: events}, mail: m}

	for i := range n {
		alice.send(t, bob, fmt.Sprintf("message %d", i))
	}
	alice.sync(t, courier)
	if got := bob.sync(t, courier); got.Received != n {
		t.Fatalf("the first sync received %d, want %d", got.Received, n)
	}
	before := db.updates.Load()
	if got := bob.sync(t, courier); got.Received != 0 {
		t.Fatalf("the second sync received %d, want 0", got.Received)
	}
	return db.updates.Load() - before
}

// Each write transaction ends with an fsync, and a sync reads again each
// seal that a relay still holds. So the writes of a sync that brings
// nothing new must not grow with the seals that it reads again.
func TestTheWritesOfASyncDoNotGrowWithKnownSeals(t *testing.T) {
	one, six := rereadWrites(t, 1), rereadWrites(t, 6)
	t.Logf("write transactions of a sync that reads again 1 message: %d; 6 messages: %d", one, six)
	if six != one {
		t.Errorf("a sync that read again 6 known messages made %d write transactions, and one with 1 made %d: the writes grow with the seals", six, one)
	}
}
