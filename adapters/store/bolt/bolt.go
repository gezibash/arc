// Package boltstore supplies Bolt persistence to the core event store.
//
// The Bolt codec of the event store holds at most 65,535 bytes of content,
// and a gift wrap of full mail holds more. Backend therefore keeps larger
// content in its own bucket, keyed by the event ID, and stores the event
// with blank content. The other buckets keep their layout.
package boltstore

import (
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sync"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/eventstore/codec/betterbinary"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/internal/boltlease"
	"go.etcd.io/bbolt"
)

// largeContent holds the content that the codec cannot hold.
var largeContent = []byte("arcLargeContent")

// Backend is a Bolt event store that also holds content larger than the
// codec takes. The citizen store and arc relay serve both use it.
// ReplaceEvent is the one of BoltBackend, so a replaceable or addressable
// event holds at most 65,535 bytes of content. Issue #183 decides the limit
// of public events.
type Backend struct {
	*boltdb.BoltBackend
}

var _ eventstore.Store = (*Backend)(nil)

// NewBackend makes a backend for the file at path. Call Init to open it.
func NewBackend(path string) *Backend {
	return &Backend{BoltBackend: &boltdb.BoltBackend{Path: path}}
}

// Init opens the file and makes the bucket for large content.
func (b *Backend) Init() error {
	if err := b.BoltBackend.Init(); err != nil {
		return err
	}
	if b.ReadOnly {
		return nil
	}
	return b.DB.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(largeContent)
		return err
	})
}

// SaveEvent saves an event. Bolt cannot nest transactions, so large content
// takes two. The content goes first: a crash between them leaves only
// content that no event names, which the next save of the event uses again.
func (b *Backend) SaveEvent(event nostr.Event) error {
	if len(event.Content) <= betterbinary.MaxContentSize {
		return b.BoltBackend.SaveEvent(event)
	}
	err := b.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(largeContent).Put(event.ID[:], []byte(event.Content))
	})
	if err != nil {
		return err
	}
	event.Content = ""
	return b.BoltBackend.SaveEvent(event)
}

// DeleteEvent deletes an event, and then its large content.
func (b *Backend) DeleteEvent(id nostr.ID) error {
	if err := b.BoltBackend.DeleteEvent(id); err != nil {
		return err
	}
	return b.DB.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(largeContent).Delete(id[:])
	})
}

// QueryEvents returns the events that match the filter, with their large
// content. A query holds a read transaction while it yields, so the events
// are collected first and get their content in a transaction of their own.
func (b *Backend) QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		var events []nostr.Event
		for event := range b.BoltBackend.QueryEvents(filter, maxLimit) {
			events = append(events, event)
		}
		_ = b.DB.View(func(tx *bbolt.Tx) error {
			bucket := tx.Bucket(largeContent)
			if bucket == nil {
				return nil
			}
			for i := range events {
				if events[i].Content == "" {
					if content := bucket.Get(events[i].ID[:]); content != nil {
						events[i].Content = string(content)
					}
				}
			}
			return nil
		})
		for _, event := range events {
			if !yield(event) {
				return
			}
		}
	}
}

type access struct {
	lease *boltlease.Lease[*Backend]
	// batch keeps other calls out while a batch runs with the sync to disk
	// turned off.
	batch *sync.RWMutex
}

func (a access) Do(fn func(store.Backend) error) error {
	a.batch.RLock()
	defer a.batch.RUnlock()
	return a.lease.Do(func(b *Backend) error { return fn(b) })
}

// DoBatch runs fn with each commit unsynced, and syncs the file once at the
// end. On macOS one sync takes about 9 ms, so 2,001 commits spent 17.5 s on
// syncs alone. A power loss before the final sync can damage the file.
func (a access) DoBatch(fn func(store.Backend) error) error {
	a.batch.Lock()
	defer a.batch.Unlock()
	return a.lease.Do(func(b *Backend) error {
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
	lease := boltlease.NewLease(path, func() (*Backend, error) {
		backend := NewBackend(path)
		return backend, backend.Init()
	}, func(backend *Backend) { backend.Close() })
	if err := lease.Do(func(*Backend) error { return nil }); err != nil {
		lease.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	return store.New(access{lease: lease, batch: &sync.RWMutex{}}), nil
}
