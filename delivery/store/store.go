// Package store holds the events that one node keeps.
//
// The store is the source of truth for a node. Transports write into it, and
// the capability layer reads from it. It refuses every event that fails
// verification, so nothing unverified ever reaches a reader.
package store

import (
	"bytes"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	if s.deleted(event) {
		return Result{Outcome: Refused, Reason: "its author deleted it"}, nil
	}

	if event.Kind.IsReplaceable() || event.Kind.IsAddressable() {
		if _, err := s.backend.ReplaceEvent(event); err != nil {
			return Result{}, fmt.Errorf("store: %w", err)
		}
		s.settle(event)
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
	if event.Kind == nostr.KindDeletion {
		s.applyDeletion(event)
	}
	return Result{Outcome: Stored}, nil
}

// deleted says whether a stored deletion request of the event's author, kind
// 5 of NIP-09, names the event. A request names an event by its ID, or an
// addressable event by its coordinate and every version up to its own time.
// A deleted event therefore does not come back from a stick or a relay.
func (s *Store) deleted(event nostr.Event) bool {
	if event.Kind == nostr.KindDeletion {
		return false
	}
	byID := nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}, Authors: []nostr.PubKey{event.PubKey},
		Tags: nostr.TagMap{"e": {event.ID.Hex()}}}
	for range s.backend.QueryEvents(byID, 1) {
		return true
	}
	if !event.Kind.IsAddressable() {
		return false
	}
	coordinate := fmt.Sprintf("%d:%s:%s", event.Kind, event.PubKey.Hex(), event.Tags.GetD())
	byAddress := nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}, Authors: []nostr.PubKey{event.PubKey},
		Tags: nostr.TagMap{"a": {coordinate}}, Since: event.CreatedAt}
	for range s.backend.QueryEvents(byAddress, 1) {
		return true
	}
	return false
}

// applyDeletion removes the events of its author that a deletion request
// names.
func (s *Store) applyDeletion(request nostr.Event) {
	var doomed []nostr.ID
	for _, tag := range request.Tags {
		if len(tag) < 2 {
			continue
		}
		var filter nostr.Filter
		switch tag[0] {
		case "e":
			id, err := nostr.IDFromHex(tag[1])
			if err != nil {
				continue
			}
			filter = nostr.Filter{IDs: []nostr.ID{id}}
		case "a":
			parts := strings.SplitN(tag[1], ":", 3)
			kind, err := strconv.Atoi(parts[0])
			if len(parts) != 3 || err != nil {
				continue
			}
			filter = nostr.Filter{Kinds: []nostr.Kind{nostr.Kind(kind)}, Tags: nostr.TagMap{"d": {parts[2]}}, Until: request.CreatedAt}
		default:
			continue
		}
		filter.Authors = []nostr.PubKey{request.PubKey}
		for event := range s.backend.QueryEvents(filter, MaxQuery) {
			// A query by ID ignores the authors, so check the author here.
			if event.PubKey == request.PubKey && event.Kind != nostr.KindDeletion {
				doomed = append(doomed, event.ID)
			}
		}
	}
	for _, id := range doomed {
		_ = s.backend.DeleteEvent(id)
	}
}

// settle keeps one version of a replaceable or addressable event: the
// newest, and of two at the same time, the one with the lower ID, as NIP-01
// defines. The backend keeps both versions of a tie.
func (s *Store) settle(event nostr.Event) {
	filter := nostr.Filter{Kinds: []nostr.Kind{event.Kind}, Authors: []nostr.PubKey{event.PubKey}}
	if event.Kind.IsAddressable() {
		filter.Tags = nostr.TagMap{"d": {event.Tags.GetD()}}
	}
	var versions []nostr.Event
	for version := range s.backend.QueryEvents(filter, MaxQuery) {
		versions = append(versions, version)
	}
	if len(versions) < 2 {
		return
	}
	winner := versions[0]
	for _, v := range versions[1:] {
		if v.CreatedAt > winner.CreatedAt || v.CreatedAt == winner.CreatedAt && bytes.Compare(v.ID[:], winner.ID[:]) < 0 {
			winner = v
		}
	}
	for _, v := range versions {
		if v.ID != winner.ID {
			_ = s.backend.DeleteEvent(v.ID)
		}
	}
}

// Has says whether the store keeps an event.
func (s *Store) Has(id nostr.ID) bool {
	for range s.backend.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}}, 1) {
		return true
	}
	return false
}

// QueryEvents lets the store stand as a nostr.Querier, for Negentropy.
func (s *Store) QueryEvents(filter nostr.Filter) iter.Seq[nostr.Event] {
	events := s.Query(filter)
	return func(yield func(nostr.Event) bool) {
		for _, event := range events {
			if !yield(event) {
				return
			}
		}
	}
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
