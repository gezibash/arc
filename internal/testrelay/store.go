package testrelay

import (
	"iter"
	"slices"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/eventstore/slicestore"
)

// store keeps events in memory and is safe for concurrent use.
// slicestore.SliceStore reads its slice without a lock while SaveEvent
// writes to it, so a query on one connection races a save on another.
type store struct {
	mu    sync.RWMutex
	inner slicestore.SliceStore
}

var _ eventstore.Store = (*store)(nil)

func newStore(t *testing.T) *store {
	t.Helper()
	db := &store{}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	return db
}

func (s *store) Init() error { return s.inner.Init() }

func (s *store) Close() { s.inner.Close() }

// QueryEvents copies the matching events under the lock, then yields them.
// A consumer can then write to a slow connection without blocking a save.
func (s *store) QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		s.mu.RLock()
		events := slices.Collect(s.inner.QueryEvents(filter, maxLimit))
		s.mu.RUnlock()
		for _, event := range events {
			if !yield(event) {
				return
			}
		}
	}
}

func (s *store) CountEvents(filter nostr.Filter) (uint32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner.CountEvents(filter)
}

func (s *store) SaveEvent(event nostr.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.SaveEvent(event)
}

func (s *store) DeleteEvent(id nostr.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.DeleteEvent(id)
}

func (s *store) ReplaceEvent(event nostr.Event) ([]nostr.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.ReplaceEvent(event)
}
