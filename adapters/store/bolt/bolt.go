// Package boltstore supplies Bolt persistence to the core event store.
package boltstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"fiatjaf.com/nostr/eventstore/boltdb"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/internal/boltlease"
)

type access struct {
	lease *boltlease.Lease[*boltdb.BoltBackend]
	// batch keeps other calls out while a batch runs with the sync to disk
	// turned off.
	batch *sync.RWMutex
}

func (a access) Do(fn func(store.Backend) error) error {
	a.batch.RLock()
	defer a.batch.RUnlock()
	return a.lease.Do(func(b *boltdb.BoltBackend) error { return fn(b) })
}

// DoBatch runs fn with each commit unsynced, and syncs the file once at the
// end. On macOS one sync takes about 9 ms, so 2,001 commits spent 17.5 s on
// syncs alone. A power loss before the final sync can damage the file.
func (a access) DoBatch(fn func(store.Backend) error) error {
	a.batch.Lock()
	defer a.batch.Unlock()
	return a.lease.Do(func(b *boltdb.BoltBackend) error {
		b.DB.NoSync = true
		err := fn(b)
		b.DB.NoSync = false
		if synced := b.DB.Sync(); err == nil {
			err = synced
		}
		return err
	})
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
	return store.New(access{lease: lease, batch: &sync.RWMutex{}}), nil
}
