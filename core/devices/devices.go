// Package devices holds the device list of a citizen: the device keys that
// may ask the signer to sign and decrypt as the citizen. The citizen key
// signs the list and seals it to itself, so only the citizen reads it.
// docs/devices/SPEC.md defines the rules.
package devices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"fiatjaf.com/nostr"
)

// Kind is the replaceable event that holds the sealed device list.
const Kind nostr.Kind = 10274

// Limits keep a hostile or broken list small.
const (
	MaxDevices = 64
	MaxName    = 64
	// MaxRevoked bounds the revocations that one list keeps.
	MaxRevoked = 256
)

// protected is the NIP-70 tag: a relay takes the event only from its author.
var protected = nostr.Tag{"-"}

// Device is one device key on the list.
type Device struct {
	Key   nostr.PubKey    `json:"key"`
	Name  string          `json:"name"`
	Added nostr.Timestamp `json:"added"`
}

// Revocation records a device key that the citizen removed. A reader
// accepts self data of the key only when it is older than At, and only from
// a storage citizen.
type Revocation struct {
	Key nostr.PubKey    `json:"key"`
	At  nostr.Timestamp `json:"at"`
	// Gen is the first generation of the data roots that the key never
	// held, or 0 when the signer did not know it.
	Gen uint32 `json:"gen"`
}

// List is the device list. At is the time of the event that holds it.
type List struct {
	Devices []Device
	Revoked []Revocation
	At      nostr.Timestamp
}

type body struct {
	Devices []Device     `json:"devices"`
	Revoked []Revocation `json:"revoked,omitempty"`
}

// RevokedAt returns the revocation of a key, if the list records one.
func (l List) RevokedAt(key nostr.PubKey) (Revocation, bool) {
	for _, r := range l.Revoked {
		if r.Key == key {
			return r, true
		}
	}
	return Revocation{}, false
}

// Has reports whether a device key is on the list.
func (l List) Has(key nostr.PubKey) bool {
	return slices.ContainsFunc(l.Devices, func(d Device) bool { return d.Key == key })
}

// Find returns the device with this name, or with this key as hex.
func (l List) Find(text string) (Device, bool) {
	for _, d := range l.Devices {
		if d.Name == text || d.Key.Hex() == text {
			return d, true
		}
	}
	return Device{}, false
}

// Add returns a list with one more device. A key or a name that is on the
// list already is refused.
func (l List) Add(d Device) (List, error) {
	if err := checkName(d.Name); err != nil {
		return l, err
	}
	if _, ok := l.RevokedAt(d.Key); ok {
		return l, errors.New("the device key was revoked: the device must make a new key")
	}
	for _, have := range l.Devices {
		if have.Key == d.Key {
			return l, fmt.Errorf("the device key is on the list already, as %q", have.Name)
		}
		if have.Name == d.Name {
			return l, fmt.Errorf("a device is named %q already", d.Name)
		}
	}
	if len(l.Devices) >= MaxDevices {
		return l, fmt.Errorf("the list holds %d devices, the most it can", MaxDevices)
	}
	out := List{Devices: append(slices.Clone(l.Devices), d), Revoked: l.Revoked, At: l.At}
	return out, nil
}

// Revoke returns a list without the device that has this key, and records
// its revocation at the time at. gen is the first generation that the key
// never holds. The oldest revocation goes when the list holds MaxRevoked.
func (l List) Revoke(key nostr.PubKey, at nostr.Timestamp, gen uint32) List {
	revoked := slices.DeleteFunc(slices.Clone(l.Revoked), func(r Revocation) bool { return r.Key == key })
	revoked = append(revoked, Revocation{Key: key, At: at, Gen: gen})
	if len(revoked) > MaxRevoked {
		revoked = revoked[len(revoked)-MaxRevoked:]
	}
	return List{
		Devices: slices.DeleteFunc(slices.Clone(l.Devices), func(d Device) bool { return d.Key == key }),
		Revoked: revoked, At: l.At,
	}
}

func checkName(name string) error {
	if name == "" || len(name) > MaxName || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("a device name has 1 to %d characters and no control characters", MaxName)
	}
	return nil
}

// Seal makes the event that holds the list. The signer is the citizen key:
// it encrypts the list to itself with NIP-44 and signs the event. The event
// is later than the list before it, so that a relay replaces the old one.
func Seal(ctx context.Context, k nostr.Keyer, l List, now nostr.Timestamp) (nostr.Event, error) {
	for _, d := range l.Devices {
		if err := checkName(d.Name); err != nil {
			return nostr.Event{}, err
		}
	}
	if len(l.Devices) > MaxDevices {
		return nostr.Event{}, fmt.Errorf("the list holds more than %d devices", MaxDevices)
	}
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return nostr.Event{}, err
	}
	if len(l.Revoked) > MaxRevoked {
		return nostr.Event{}, fmt.Errorf("the list holds more than %d revocations", MaxRevoked)
	}
	text, err := json.Marshal(body{Devices: append([]Device{}, l.Devices...), Revoked: l.Revoked})
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := k.Encrypt(ctx, string(text), me)
	if err != nil {
		return nostr.Event{}, err
	}
	at := max(now, l.At+1)
	event := nostr.Event{Kind: Kind, CreatedAt: at, Content: content, Tags: nostr.Tags{protected}}
	if err := k.SignEvent(ctx, &event); err != nil {
		return nostr.Event{}, err
	}
	return event, nil
}

// Open checks that the citizen signed the event, and decrypts the list
// with the citizen key.
func Open(ctx context.Context, k nostr.Keyer, citizen nostr.PubKey, event nostr.Event) (List, error) {
	if event.Kind != Kind {
		return List{}, fmt.Errorf("devices: kind %d is not a device list", event.Kind)
	}
	if event.PubKey != citizen {
		return List{}, errors.New("devices: the citizen did not sign the list")
	}
	if !event.VerifySignature() {
		return List{}, errors.New("devices: the signature of the list is not valid")
	}
	text, err := k.Decrypt(ctx, event.Content, citizen)
	if err != nil {
		return List{}, fmt.Errorf("devices: the list does not open: %w", err)
	}
	var b body
	if err := json.Unmarshal([]byte(text), &b); err != nil {
		return List{}, fmt.Errorf("devices: the list is not valid: %w", err)
	}
	if len(b.Devices) > MaxDevices {
		return List{}, fmt.Errorf("devices: the list holds more than %d devices", MaxDevices)
	}
	for _, d := range b.Devices {
		if err := checkName(d.Name); err != nil {
			return List{}, fmt.Errorf("devices: %w", err)
		}
	}
	if len(b.Revoked) > MaxRevoked {
		return List{}, fmt.Errorf("devices: the list holds more than %d revocations", MaxRevoked)
	}
	return List{Devices: b.Devices, Revoked: b.Revoked, At: event.CreatedAt}, nil
}

// Newer reports whether event a replaces event b, as NIP-01 orders
// replaceable events: the later one, and on a tie the lower ID.
func Newer(a, b nostr.Event) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID.Hex() < b.ID.Hex()
}

// Filter matches the device list of a citizen.
func Filter(citizen nostr.PubKey) nostr.Filter {
	return nostr.Filter{Kinds: []nostr.Kind{Kind}, Authors: []nostr.PubKey{citizen}}
}
