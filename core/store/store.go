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
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"github.com/gezibash/arc/core/draft"
)

// MaxQuery caps the events that one query returns.
const MaxQuery = 1_000_000

// MaxEvent is the most bytes that the JSON of one event holds, in the relay
// form, as docs/delivery/SPEC.md section 3.4 defines. It applies to each
// regular, replaceable and addressable kind; no store keeps an ephemeral
// event. A gift wrap of kind 1059 has the smaller limit private.MaxWrap. The
// Bolt event store holds at most 65,535 bytes of content in one event, and
// common relays cap an event at 64 KiB. The JSON of an event holds its
// content and at least 300 bytes of other fields, so an event of MaxEvent
// bytes has content that the store holds.
const MaxEvent = 64 << 10

// ReasonUnlisted is the reason of a refusal of self data that a key signed
// which is not on the device list. The key can join the list later.
const ReasonUnlisted = "the signer is not on the device list of the citizen"

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

// Store applies ARC event rules to an injected persistence backend.
type Store struct {
	backend Access
	now     func() time.Time
	signers Signers
}

// Signers says which keys may write self data for a citizen. A device key
// on the device list of the citizen may. The store asks it while it holds
// the backend, so it must not use the store.
type Signers interface {
	Standing(citizen, signer nostr.PubKey) Standing
}

// Standing is what the device list says about one key of a citizen.
type Standing struct {
	// Listed means the key is on the device list, or is the key of this
	// machine and not revoked.
	Listed bool
	// RevokedAt is the time of the revocation of the key, or 0.
	RevokedAt nostr.Timestamp
	// RevokedGen is the first generation that the revoked key never held.
	RevokedGen uint32
	// NoList means that the citizen has no device list at all.
	NoList bool
}

// Source says where an event comes from. A storage citizen refuses each
// write of a revoked device key after its revocation, so only an event from
// a storage citizen can be the work of a revoked key before it.
type Source int

// The sources of an event.
const (
	// Elsewhere is a relay, a directory, or this machine.
	Elsewhere Source = iota
	// StorageCitizen is a storage citizen of the citizen.
	StorageCitizen
)

// Backend is the persistence used while an Access scope is held.
type Backend interface {
	QueryEvents(nostr.Filter, int) iter.Seq[nostr.Event]
	SaveEvent(nostr.Event) error
	ReplaceEvent(nostr.Event) ([]nostr.Event, error)
	DeleteEvent(nostr.ID) error
}

// Access keeps a backend open for the complete validation and persistence operation.
// Another process must not change the backend while this scope is held.
type Access interface {
	Do(func(Backend) error) error
	Close()
}

// BatchAccess is an Access that can run several writes with one sync to disk.
// A power loss during the batch can lose or damage what it wrote, so only
// events that a transport still holds use it.
type BatchAccess interface {
	Access
	DoBatch(func(Backend) error) error
}

// New wraps persistence with ARC verification and event retention rules.
func New(backend Access) *Store { return &Store{backend: backend, now: time.Now} }

// SetSigners gives the store the device list. Without it, the store keeps
// self data only from the citizen key. Call it before the first Save.
func (s *Store) SetSigners(signers Signers) { s.signers = signers }

// Close releases the injected backend.
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
	return s.SaveFrom(event, Elsewhere)
}

// SaveFrom is Save for an event from a known source.
func (s *Store) SaveFrom(event nostr.Event, from Source) (Result, error) {
	var result Result
	// One lease holds the checks and the write together, so no other
	// process writes between them.
	err := s.backend.Do(func(b Backend) error {
		var err error
		result, err = s.save(b, event, from)
		return err
	})
	return result, err
}

// SaveAll saves events that came from a transport. Each event gets the checks
// of Save. If the backend can batch, the events share one sync to disk.
func (s *Store) SaveAll(events []nostr.Event) ([]Result, error) {
	do := s.backend.Do
	if batch, ok := s.backend.(BatchAccess); ok {
		do = batch.DoBatch
	}
	results := make([]Result, 0, len(events))
	err := do(func(b Backend) error {
		for _, event := range events {
			result, err := s.save(b, event, Elsewhere)
			if err != nil {
				return err
			}
			results = append(results, result)
		}
		return nil
	})
	return results, err
}

func (s *Store) save(b Backend, event nostr.Event, from Source) (Result, error) {
	if err := Verify(event); err != nil {
		return Result{Outcome: Refused, Reason: err.Error()}, nil
	}
	if event.Kind.IsEphemeral() {
		return Result{Outcome: Refused, Reason: "an ephemeral event is never kept"}, nil
	}
	if Expired(event, s.now()) {
		return Result{Outcome: Refused, Reason: "the event has expired"}, nil
	}

	if draft.IsMoved(event) {
		return Result{Outcome: Refused, Reason: "a deletion of drafts that moved to a storage citizen is for relays only"}, nil
	}
	if has(b, event.ID) {
		return Result{Outcome: Duplicate}, nil
	}
	if reason := s.refusedSigner(event, from); reason != "" {
		return Result{Outcome: Refused, Reason: reason}, nil
	}
	if deleted(b, event) {
		return Result{Outcome: Refused, Reason: "its author deleted it"}, nil
	}

	if event.Kind.IsReplaceable() || event.Kind.IsAddressable() {
		if _, err := b.ReplaceEvent(event); err != nil {
			return Result{}, fmt.Errorf("store: %w", err)
		}
		settle(b, event)
		// The backend keeps the newer version without saying so. Look.
		if !has(b, event.ID) {
			return Result{Outcome: Superseded}, nil
		}
		return Result{Outcome: Stored}, nil
	}

	err := b.SaveEvent(event)
	if errors.Is(err, eventstore.ErrDupEvent) {
		return Result{Outcome: Duplicate}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("store: %w", err)
	}
	if event.Kind == nostr.KindDeletion {
		applyDeletion(b, event)
	}
	return Result{Outcome: Stored}, nil
}

// deleted says whether a stored deletion request of the event's owner, kind
// 5 of NIP-09, names the event. A request names an event by its ID, or an
// addressable event by its coordinate and every version up to its own time.
// A deleted event therefore does not come back from a stick or a relay. The
// owner of self data is the citizen that its p tag names, so a deletion that
// one device signs removes a page that another device wrote.
func deleted(b Backend, event nostr.Event) bool {
	if event.Kind == nostr.KindDeletion {
		return false
	}
	owner := draft.Owner(event)
	byID := nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}, Tags: nostr.TagMap{"e": {event.ID.Hex()}}}
	if !draft.Self(event) {
		byID.Authors = []nostr.PubKey{event.PubKey}
	}
	for request := range b.QueryEvents(byID, MaxQuery) {
		if draft.Owner(request) == owner {
			return true
		}
	}
	if !event.Kind.IsAddressable() {
		return false
	}
	coordinate := fmt.Sprintf("%d:%s:%s", event.Kind, owner.Hex(), event.Tags.GetD())
	byAddress := nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}, Tags: nostr.TagMap{"a": {coordinate}}, Since: event.CreatedAt}
	if !draft.Self(event) {
		byAddress.Authors = []nostr.PubKey{event.PubKey}
	}
	for request := range b.QueryEvents(byAddress, MaxQuery) {
		if draft.Owner(request) == owner {
			return true
		}
	}
	return false
}

// applyDeletion removes the events of its owner that a deletion request
// names.
func applyDeletion(b Backend, request nostr.Event) {
	owner := draft.Owner(request)
	self := draft.Self(request)
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
			if len(parts) != 3 || parts[1] != owner.Hex() {
				continue
			}
			kind, err := strconv.Atoi(parts[0])
			if err != nil {
				continue
			}
			filter = nostr.Filter{Kinds: []nostr.Kind{nostr.Kind(kind)}, Tags: nostr.TagMap{"d": {parts[2]}}, Until: request.CreatedAt}
		default:
			continue
		}
		if !self {
			filter.Authors = []nostr.PubKey{request.PubKey}
		}
		for event := range b.QueryEvents(filter, MaxQuery) {
			// A query by ID ignores the authors, so check the owner here.
			if event.Kind != nostr.KindDeletion && draft.Owner(event) == owner {
				doomed = append(doomed, event.ID)
			}
		}
	}
	for _, id := range doomed {
		_ = b.DeleteEvent(id)
	}
}

// refusedSigner says why the store refuses self data that a key other than
// its owner signed. It returns "" when the signer may write it.
func (s *Store) refusedSigner(event nostr.Event, from Source) string {
	if !draft.Self(event) {
		return ""
	}
	owner := draft.Owner(event)
	if owner == event.PubKey {
		return ""
	}
	if s.signers == nil {
		return "self data of another key, and this store has no device list"
	}
	st := s.signers.Standing(owner, event.PubKey)
	switch {
	case st.Listed:
		return ""
	case st.NoList:
		// With no device list, the signer admits each device that gives
		// its secret, and no key is ever revoked. The AEAD of a blob, or
		// the stick that the citizen carries, is the check. No relay
		// serves self data that a device key signed.
		return ""
	case st.RevokedAt != 0 && from != StorageCitizen:
		return "the device key was revoked, and only a storage citizen brings its older work"
	case st.RevokedAt != 0 && event.CreatedAt >= st.RevokedAt:
		return "the device key signed it after its revocation"
	case st.RevokedAt != 0:
		if gen, ok := draft.Gen(event); ok && gen >= st.RevokedGen && st.RevokedGen != 0 {
			return "the device key never held this generation"
		}
		return ""
	default:
		return ReasonUnlisted
	}
}

// settle keeps one version of a replaceable or addressable event: the
// newest, and of two at the same time, the one with the lower ID, as NIP-01
// defines. The backend keeps both versions of a tie.
func settle(b Backend, event nostr.Event) {
	filter := nostr.Filter{Kinds: []nostr.Kind{event.Kind}}
	self := draft.Self(event)
	if !self {
		filter.Authors = []nostr.PubKey{event.PubKey}
	}
	if event.Kind.IsAddressable() {
		filter.Tags = nostr.TagMap{"d": {event.Tags.GetD()}}
	}
	// Self data of one citizen is one version for each d tag, whichever
	// device signed it.
	owner := draft.Owner(event)
	var versions []nostr.Event
	for version := range b.QueryEvents(filter, MaxQuery) {
		if !self || draft.Owner(version) == owner {
			versions = append(versions, version)
		}
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
			_ = b.DeleteEvent(v.ID)
		}
	}
}

// Has says whether the store keeps an event.
func (s *Store) Has(id nostr.ID) (bool, error) {
	held := false
	err := s.backend.Do(func(b Backend) error {
		held = has(b, id)
		return nil
	})
	return held, err
}

func has(b Backend, id nostr.ID) bool {
	for range b.QueryEvents(nostr.Filter{IDs: []nostr.ID{id}}, 1) {
		return true
	}
	return false
}

// Events is a successfully read snapshot adapted to nostr.Querier. Read errors
// must be handled before using this adapter for reconciliation.
type Events []nostr.Event

func (snapshot *Events) QueryEvents(filter nostr.Filter) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		for _, event := range *snapshot {
			if filter.Matches(event) && !yield(event) {
				return
			}
		}
	}
}

// Query returns the events that match a filter, newest first. It leaves out
// an event whose expiration has passed, and removes it from the store.
func (s *Store) Query(filter nostr.Filter) ([]nostr.Event, error) {
	var out []nostr.Event
	err := s.backend.Do(func(b Backend) error {
		out = s.query(b, filter)
		return nil
	})
	return out, err
}

func (s *Store) query(b Backend, filter nostr.Filter) []nostr.Event {
	var out, expired []nostr.Event
	now := s.now()

	for event := range b.QueryEvents(filter, MaxQuery) {
		if Expired(event, now) {
			expired = append(expired, event)
			continue
		}
		out = append(out, event)
	}

	for _, event := range expired {
		_ = b.DeleteEvent(event.ID)
	}
	return out
}
