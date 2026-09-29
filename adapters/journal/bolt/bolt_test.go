package boltjournal_test

import (
	"errors"
	"path/filepath"
	"testing"

	boltjournal "github.com/gezibash/arc/adapters/journal/bolt"
	"github.com/gezibash/arc/core/journal"
)

// A pending receipt and its outbox entry must commit together, or neither may
// become visible. The journal contract in docs/ARCHITECTURE.md requires this.
func TestTransactionsCommitTogetherAndSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	db := boltjournal.Open(path)
	defer db.Close()
	requests, outbox := []byte("requests"), []byte("outbox")
	if err := db.Update(func(tx journal.Tx) error {
		for _, name := range [][]byte{requests, outbox} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	refused := errors.New("second state transition failed")
	err := db.Update(func(tx journal.Tx) error {
		if err := tx.Bucket(requests).Put([]byte("failed"), []byte("pending")); err != nil {
			return err
		}
		if err := tx.Bucket(outbox).Put([]byte("failed"), []byte("reply")); err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("update error=%v, want the callback failure", err)
	}
	if err := db.View(func(tx journal.Tx) error {
		for _, name := range [][]byte{requests, outbox} {
			if value := tx.Bucket(name).Get([]byte("failed")); value != nil {
				t.Errorf("failed transaction retained %s=%q", name, value)
			}
		}
		if err := tx.Bucket(requests).Put([]byte("read-only"), []byte("mutation")); err == nil {
			t.Error("a read-only view accepted a write")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.Update(func(tx journal.Tx) error {
		if err := tx.Bucket(requests).Put([]byte("committed"), []byte("completed")); err != nil {
			return err
		}
		return tx.Bucket(outbox).Put([]byte("committed"), []byte("stored reply"))
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = boltjournal.Open(path)
	defer db.Close()
	if err := db.View(func(tx journal.Tx) error {
		if got := string(tx.Bucket(requests).Get([]byte("committed"))); got != "completed" {
			t.Errorf("reopened request=%q, want completed", got)
		}
		if got := string(tx.Bucket(outbox).Get([]byte("committed"))); got != "stored reply" {
			t.Errorf("reopened outbox=%q, want stored reply", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
