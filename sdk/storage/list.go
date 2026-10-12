package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"fiatjaf.com/nostr"
)

// ListKind is the replaceable event that holds the storage list of a
// citizen: the storage citizens that its devices sync with. The citizen key
// signs it and seals it to itself. docs/storage/SPEC.md, section 3.8,
// defines the rules.
const ListKind nostr.Kind = 10273

// Limits keep a hostile or broken list small.
const (
	MaxEntries   = 16
	MaxEntryName = 64
	MaxRelays    = 8
)

// The states of an entry.
const (
	// StateActive is a storage citizen that each device syncs with.
	StateActive = "active"
	// StateDraining is a storage citizen that the citizen leaves. Each
	// device still syncs with it.
	StateDraining = "draining"
	// StateGone is a storage citizen that no device uses. Each device
	// removes its install.
	StateGone = "gone"
)

// protected is the NIP-70 tag: a relay takes the event only from its author.
var protected = nostr.Tag{"-"}

// ListEntry is one storage citizen on the list. Name is the installed name of
// its storage app on each device. Since is the time of the last change of
// State.
type ListEntry struct {
	Key    nostr.PubKey    `json:"key"`
	Name   string          `json:"name"`
	Relays []string        `json:"relays"`
	State  string          `json:"state"`
	Since  nostr.Timestamp `json:"since"`
}

// Used reports whether a device syncs with the storage citizen.
func (e ListEntry) Used() bool { return e.State == StateActive || e.State == StateDraining }

// List is the storage list. At is the time of the event that holds it.
type List struct {
	Entries []ListEntry
	At      nostr.Timestamp
}

type listBody struct {
	Version int         `json:"v"`
	Storage []ListEntry `json:"storage"`
}

// Find returns the entry with this name, or with this key as hex.
func (l List) Find(text string) (ListEntry, bool) {
	for _, e := range l.Entries {
		if e.Name == text || e.Key.Hex() == text {
			return e, true
		}
	}
	return ListEntry{}, false
}

// Put returns a list with the entry. An entry with the same key is
// replaced, so a storage citizen that the citizen left can come back. A
// name that another key holds is refused.
func (l List) Put(e ListEntry) (List, error) {
	if err := checkEntry(e); err != nil {
		return l, err
	}
	out := List{At: l.At}
	for _, have := range l.Entries {
		if have.Key == e.Key {
			continue
		}
		if have.Name == e.Name {
			return l, fmt.Errorf("the storage list names another storage citizen %q already", e.Name)
		}
		out.Entries = append(out.Entries, have)
	}
	if len(out.Entries) >= MaxEntries {
		return l, fmt.Errorf("the storage list holds %d storage citizens, the most it can", MaxEntries)
	}
	out.Entries = append(out.Entries, e)
	return out, nil
}

func checkEntry(e ListEntry) error {
	if e.Name == "" || len(e.Name) > MaxEntryName || strings.ContainsFunc(e.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("a storage name has 1 to %d characters and no control characters", MaxEntryName)
	}
	if !slices.Contains([]string{StateActive, StateDraining, StateGone}, e.State) {
		return fmt.Errorf("the state %q of %s is not active, draining or gone", e.State, e.Name)
	}
	if len(e.Relays) > MaxRelays {
		return fmt.Errorf("%s names more than %d relays", e.Name, MaxRelays)
	}
	return nil
}

func checkList(entries []ListEntry) error {
	if len(entries) > MaxEntries {
		return fmt.Errorf("the storage list holds more than %d storage citizens", MaxEntries)
	}
	keys, names := map[nostr.PubKey]bool{}, map[string]bool{}
	for _, e := range entries {
		if err := checkEntry(e); err != nil {
			return err
		}
		if keys[e.Key] || names[e.Name] {
			return fmt.Errorf("the storage list holds %s twice", e.Name)
		}
		keys[e.Key], names[e.Name] = true, true
	}
	return nil
}

// SealList makes the event that holds the list. The signer is the citizen
// key: it encrypts the list to itself with NIP-44 and signs the event. The
// event is later than the list before it, so that a relay replaces the old
// one.
func SealList(ctx context.Context, k nostr.Keyer, l List, now nostr.Timestamp) (nostr.Event, error) {
	if err := checkList(l.Entries); err != nil {
		return nostr.Event{}, err
	}
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return nostr.Event{}, err
	}
	text, err := json.Marshal(listBody{Version: 1, Storage: append([]ListEntry{}, l.Entries...)})
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := k.Encrypt(ctx, string(text), me)
	if err != nil {
		return nostr.Event{}, err
	}
	at := max(now, l.At+1)
	event := nostr.Event{Kind: ListKind, CreatedAt: at, Content: content, Tags: nostr.Tags{protected}}
	if err := k.SignEvent(ctx, &event); err != nil {
		return nostr.Event{}, err
	}
	return event, nil
}

// OpenList checks that the citizen signed the event, and decrypts the list
// with the citizen key.
func OpenList(ctx context.Context, k nostr.Keyer, citizen nostr.PubKey, event nostr.Event) (List, error) {
	if event.Kind != ListKind {
		return List{}, fmt.Errorf("storage: kind %d is not a storage list", event.Kind)
	}
	if event.PubKey != citizen {
		return List{}, errors.New("storage: the citizen did not sign the storage list")
	}
	if !event.VerifySignature() {
		return List{}, errors.New("storage: the signature of the storage list is not valid")
	}
	text, err := k.Decrypt(ctx, event.Content, citizen)
	if err != nil {
		return List{}, fmt.Errorf("storage: the storage list does not open: %w", err)
	}
	var b listBody
	if err := json.Unmarshal([]byte(text), &b); err != nil {
		return List{}, fmt.Errorf("storage: the storage list is not valid: %w", err)
	}
	if b.Version != 1 {
		return List{}, fmt.Errorf("storage: the storage list has version %d; this arc knows version 1; update arc", b.Version)
	}
	if err := checkList(b.Storage); err != nil {
		return List{}, fmt.Errorf("storage: %w", err)
	}
	return List{Entries: b.Storage, At: event.CreatedAt}, nil
}

// ListFilter matches the storage list of a citizen.
func ListFilter(citizen nostr.PubKey) nostr.Filter {
	return nostr.Filter{Kinds: []nostr.Kind{ListKind}, Authors: []nostr.PubKey{citizen}}
}
