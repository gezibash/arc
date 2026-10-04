// Package boltkv implements the core key-value store with Bolt.
package boltkv

import (
	"time"

	"github.com/gezibash/arc/core/kv"
	"github.com/gezibash/arc/internal/boltlease"
	"go.etcd.io/bbolt"
)

type Store struct{ lease *boltlease.Lease[*bbolt.DB] }

// Open prepares the store. The first transaction opens and validates the file.
func Open(path string) *Store {
	return &Store{boltlease.NewLease(path, func() (*bbolt.DB, error) {
		return bbolt.Open(path, 0600, &bbolt.Options{Timeout: 2 * time.Second})
	}, func(db *bbolt.DB) { _ = db.Close() })}
}
func (s *Store) View(fn func(kv.Tx) error) error {
	return s.lease.Do(func(db *bbolt.DB) error { return db.View(func(t *bbolt.Tx) error { return fn(tx{t}) }) })
}
func (s *Store) Update(fn func(kv.Tx) error) error {
	return s.lease.Do(func(db *bbolt.DB) error { return db.Update(func(t *bbolt.Tx) error { return fn(tx{t}) }) })
}
func (s *Store) Close() { s.lease.Close() }

type tx struct{ *bbolt.Tx }

func (t tx) Bucket(name []byte) kv.Bucket {
	b := t.Tx.Bucket(name)
	if b == nil {
		return nil
	}
	return bucket{b}
}
func (t tx) CreateBucketIfNotExists(name []byte) (kv.Bucket, error) {
	b, err := t.Tx.CreateBucketIfNotExists(name)
	if err != nil {
		return nil, err
	}
	return bucket{b}, nil
}

type bucket struct{ *bbolt.Bucket }

func (b bucket) Cursor() kv.Cursor { return b.Bucket.Cursor() }
