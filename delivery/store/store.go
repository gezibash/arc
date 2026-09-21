// Package store holds the events that one node keeps.
//
// The store is the source of truth for a node. Transports write into it, and
// the capability layer reads from it. It refuses every event that fails
// verification, so nothing unverified ever reaches a reader.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/eventstore/boltdb"
)

// MaxQuery caps the events that one query returns.
const MaxQuery = 1_000_000

// Outcome says what the store did with one event.
type Outcome int

// The outcomes of Save.
const (
	// Stored means that the store keeps the event now.
	Stored Outcome = iota
	// Duplicate means that the store already held this event.
	Duplicate
	// Superseded means that the store holds a newer version of the same
	// replaceable or addressable event, and keeps that one.
	Superseded
	// Refused means that the event failed a check. The reason says which.
	Refused
)

func (o Outcome) String() string {
	return [...]string{"stored", "duplicate", "superseded", "refused"}[o]
}

// Result is the outcome of one Save.
type Result struct {
	Outcome Outcome
	Reason  string
}

// Store keeps events in one file.
type Store struct {
	backend *boltdb.BoltBackend
	now     func() time.Time
}

// Open opens the store in a directory, and makes it when it is not there.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	backend := &boltdb.BoltBackend{Path: filepath.Join(dir, "events.db")}
	if err := backend.Init(); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &Store{backend: backend, now: time.Now}, nil
}

// Close releases the store.
func (s *Store) Close() { s.backend.Close() }

// Verify checks that the ID of an event follows from its fields, and that
// the signature over the ID is valid for its author.
func Verify(event nostr.Event) error {
	if !event.CheckID() {
		return errors.New("the id does not match the event")
	}
	if !event.VerifySignature() {
		return errors.New("the signature is not valid")
	}
	return nil
}

// Expired says whether the expiration tag of an event has passed, as NIP-40
// defines.
func Expired(event nostr.Event, now time.Time) bool {
	tag := event.Tags.Find("expiration")
	if len(tag) < 2 {
		return false
	}
	at, err := strconv.ParseInt(tag[1], 10, 64)
	return err == nil && at <= now.Unix()
}

// Save verifies one event and keeps it. An ephemeral event is never kept.
// A replaceable or addressable event replaces an older version, and loses
// to a newer one.
func (s *Store) Save(event nostr.Event) (Result, error) {
	if err := Verify(event); err != nil {
		return Result{Outcome: Refused, Reason: err.Error()}, nil
	}
	if event.Kind.IsEphemeral() {
		return Result{Outcome: Refused, Reason: "an ephemeral event is never kept"}, nil
	}
	if Expired(event, s.now()) {
		return Result{Outcome: Refused, Reason: "the event has expired"}, nil
	}

	if s.Has(event.ID) {
		return Result{Outcome: Duplicate}, nil
	}

	if event.Kind.IsReplaceable() || event.Kind.IsAddressable() {
		if _, err := s.backend.ReplaceEvent(event); err != nil {
			return Result{}, fmt.Errorf("store: %w", err)
		}
		// The backend keeps the newer version without saying so. Look.
		if !s.Has(event.ID) {
			return Result{Outcome: Superseded}, nil
		}
		return Result{Outcome: Stored}, nil
	}

	err := s.backend.SaveEvent(event)
	if errors.Is(err, eventstore.ErrDupEvent) {
		return Result{Outcome: Duplicate}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("store: %w", err)
	}
	return Result{Outcome: Stored}, nil
}

// Has says whether the store keeps an event.
func (s *Store) Has(id nostr.ID) bool {
	for range s.backend.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}}, 1) {
		return true
	}
	return false
}

// Query returns the events that match a filter, newest first. It leaves out
// an event whose expiration has passed, and removes it from the store.
func (s *Store) Query(filter nostr.Filter) []nostr.Event {
	var out, expired []nostr.Event
	now := s.now()

	for event := range s.backend.QueryEvents(filter, MaxQuery) {
		if Expired(event, now) {
			expired = append(expired, event)
			continue
		}
		out = append(out, event)
	}

	for _, event := range expired {
		_ = s.backend.DeleteEvent(event.ID)
	}
	return out
}
