// Package compact encodes Nostr events for small links, as delivery section
// 7.3 specifies. It preserves the signed fields and reconstructs the ID.
// Decoding is not authentication: the store must still verify the event.
package compact

import (
	"encoding/binary"
	"errors"
	"math"
	"unicode/utf8"

	"fiatjaf.com/nostr"
)

const (
	// Version is the compact event format, independent of mesh framing.
	Version byte = 1
	// MaxBytes bounds one compact event, including all its fields.
	MaxBytes = 1 << 20
)

var (
	ErrInvalid  = errors.New("invalid compact event")
	ErrVersion  = errors.New("unsupported compact event version")
	ErrTooLarge = errors.New("compact event exceeds 1 MiB")
)

// Encode preserves field and tag order, including empty tags and values.
// It refuses invalid text, negative timestamps and an inconsistent event ID.
// Signature verification remains the responsibility of the store.
func Encode(event nostr.Event) ([]byte, error) {
	if event.CreatedAt < 0 {
		return nil, ErrInvalid
	}
	size := 1 + len(event.PubKey) + len(event.Sig)
	add := func(n int) bool {
		if n > MaxBytes-size {
			return false
		}
		size += n
		return true
	}
	textSize := func(s string) error {
		if !utf8.ValidString(s) {
			return ErrInvalid
		}
		if !add(varintSize(uint64(len(s)))) || !add(len(s)) {
			return ErrTooLarge
		}
		return nil
	}
	if !add(varintSize(uint64(event.CreatedAt))) || !add(varintSize(uint64(event.Kind))) || !add(varintSize(uint64(len(event.Tags)))) {
		return nil, ErrTooLarge
	}
	for _, tag := range event.Tags {
		if !add(varintSize(uint64(len(tag)))) {
			return nil, ErrTooLarge
		}
		for _, value := range tag {
			if err := textSize(value); err != nil {
				return nil, err
			}
		}
	}
	if err := textSize(event.Content); err != nil {
		return nil, err
	}
	if !event.CheckID() {
		return nil, ErrInvalid
	}
	out := make([]byte, 0, size)
	out = append(out, Version)
	out = append(out, event.PubKey[:]...)
	out = append(out, event.Sig[:]...)
	out = binary.AppendUvarint(out, uint64(event.CreatedAt))
	out = binary.AppendUvarint(out, uint64(event.Kind))
	out = binary.AppendUvarint(out, uint64(len(event.Tags)))
	for _, tag := range event.Tags {
		out = binary.AppendUvarint(out, uint64(len(tag)))
		for _, value := range tag {
			out = appendText(out, value)
		}
	}
	return appendText(out, event.Content), nil
}

// Decode accepts exactly one event, rejects trailing bytes and noncanonical
// integers, and computes its NIP-01 ID. It does not verify the signature,
// expiration or application permissions. Never dispatch its result directly.
func Decode(data []byte) (nostr.Event, error) {
	var event nostr.Event
	if len(data) > MaxBytes {
		return event, ErrTooLarge
	}
	if len(data) == 0 {
		return event, ErrInvalid
	}
	if data[0] != Version {
		return event, ErrVersion
	}
	const fixed = 1 + 32 + 64
	if len(data) < fixed {
		return event, ErrInvalid
	}
	copy(event.PubKey[:], data[1:33])
	copy(event.Sig[:], data[33:fixed])
	d := decoder{data: data[fixed:]}
	at, ok := d.number()
	if !ok || at > math.MaxInt64 {
		return nostr.Event{}, ErrInvalid
	}
	kind, ok := d.number()
	if !ok || kind > math.MaxUint16 {
		return nostr.Event{}, ErrInvalid
	}
	count, ok := d.number()
	// Every tag needs at least one count byte; leave a byte for content length.
	if !ok || len(d.data) == 0 || count > uint64(len(d.data)-1) {
		return nostr.Event{}, ErrInvalid
	}
	event.CreatedAt = nostr.Timestamp(at)
	event.Kind = nostr.Kind(kind)
	event.Tags = make(nostr.Tags, 0, int(count))
	for range count {
		fields, ok := d.number()
		if !ok || len(d.data) == 0 || fields > uint64(len(d.data)-1) {
			return nostr.Event{}, ErrInvalid
		}
		tag := make(nostr.Tag, 0, int(fields))
		for range fields {
			value, ok := d.text()
			if !ok {
				return nostr.Event{}, ErrInvalid
			}
			tag = append(tag, value)
		}
		event.Tags = append(event.Tags, tag)
	}
	content, ok := d.text()
	if !ok || len(d.data) != 0 {
		return nostr.Event{}, ErrInvalid
	}
	event.Content = content
	event.SetID()
	return event, nil
}

type decoder struct{ data []byte }

func (d *decoder) number() (uint64, bool) {
	value, n := binary.Uvarint(d.data)
	if n <= 0 || n != varintSize(value) {
		return 0, false
	}
	d.data = d.data[n:]
	return value, true
}

func (d *decoder) text() (string, bool) {
	size, ok := d.number()
	if !ok || size > uint64(len(d.data)) {
		return "", false
	}
	body := d.data[:int(size)]
	if !utf8.Valid(body) {
		return "", false
	}
	d.data = d.data[int(size):]
	return string(body), true
}

func appendText(out []byte, text string) []byte {
	out = binary.AppendUvarint(out, uint64(len(text)))
	return append(out, text...)
}

func varintSize(value uint64) int {
	var b [binary.MaxVarintLen64]byte
	return binary.PutUvarint(b[:], value)
}
