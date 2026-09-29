// Package boltjournal implements ARC journal transactions with Bolt.
package boltjournal

import (
	"time"

	"github.com/gezibash/arc/core/journal"
	"github.com/gezibash/arc/internal/boltlease"
	"go.etcd.io/bbolt"
)

type Store struct{ lease *boltlease.Lease[*bbolt.DB] }

// Open prepares the journal. The first transaction opens and validates the file.
func Open(path string) *Store {
	return &Store{boltlease.NewLease(path, func() (*bbolt.DB, error) {
		return bbolt.Open(path, 0600, &bbolt.Options{Timeout: 2 * time.Second})
	}, func(db *bbolt.DB) { db.Close() })}
}
func (s *Store) View(fn func(journal.Tx) error) error {
	return s.lease.Do(func(db *bbolt.DB) error { return db.View(func(t *bbolt.Tx) error { return fn(tx{t}) }) })
}
func (s *Store) Update(fn func(journal.Tx) error) error {
	return s.lease.Do(func(db *bbolt.DB) error { return db.Update(func(t *bbolt.Tx) error { return fn(tx{t}) }) })
}
func (s *Store) Close() { s.lease.Close() }

type tx struct{ *bbolt.Tx }

func (t tx) Bucket(name []byte) journal.Bucket {
	b := t.Tx.Bucket(name)
	if b == nil {
		return nil
	}
	return bucket{b}
}
func (t tx) CreateBucketIfNotExists(name []byte) (journal.Bucket, error) {
	b, err := t.Tx.CreateBucketIfNotExists(name)
	if err != nil {
		return nil, err
	}
	return bucket{b}, nil
}

type bucket struct{ *bbolt.Bucket }

func (b bucket) Cursor() journal.Cursor { return b.Bucket.Cursor() }
