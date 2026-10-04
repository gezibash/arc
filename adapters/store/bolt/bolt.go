// Package boltstore supplies Bolt persistence to the core event store.
package boltstore

import (
	"fmt"
	"os"
	"path/filepath"

	"fiatjaf.com/nostr/eventstore/boltdb"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/internal/boltlease"
)

type access struct {
	lease *boltlease.Lease[*boltdb.BoltBackend]
}

func (a access) Do(fn func(store.Backend) error) error {
	return a.lease.Do(func(b *boltdb.BoltBackend) error { return fn(b) })
}
func (a access) Close() { a.lease.Close() }

// Open opens the existing events.db layout under a cross-process lease.
func Open(dir string) (*store.Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	path := filepath.Join(dir, "events.db")
	lease := boltlease.NewLease(path, func() (*boltdb.BoltBackend, error) {
		backend := &boltdb.BoltBackend{Path: path}
		return backend, backend.Init()
	}, func(backend *boltdb.BoltBackend) { backend.Close() })
	if err := lease.Do(func(*boltdb.BoltBackend) error { return nil }); err != nil {
		lease.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	return store.New(access{lease}), nil
}
