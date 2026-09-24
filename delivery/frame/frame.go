// Package frame packs compact events for small links. It does not operate a
// radio or authenticate events. Completed events must pass store verification.
package frame

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/compact"
)

const (
	Event               byte = 1
	Fragment            byte = 2
	HeaderBytes              = 2
	FragmentHeaderBytes      = 14 // type, hops, ID[8], index uint16, total uint16
	MaxFragments             = 4096
)

var (
	ErrInvalid  = errors.New("invalid event frame")
	ErrSize     = errors.New("frame or event exceeds size limits")
	ErrCapacity = errors.New("fragment assembly capacity reached")
	ErrConflict = errors.New("conflicting fragments")
)

// Frame's hops are outside the signed event. Zero hops permits local receipt;
// forwarding policy belongs to the future mesh transport.
// Types 3–6 are reserved for sync, announce, handshake and session.
type Frame struct {
	Type  byte
	Hops  byte
	ID    [8]byte
	Index uint16
	Total uint16
	Body  []byte
}

func (f Frame) validate() error {
	if len(f.Body) == 0 {
		return ErrInvalid
	}
	if len(f.Body) > compact.MaxBytes {
		return ErrSize
	}
	switch f.Type {
	case Event:
		if f.ID != ([8]byte{}) || f.Index != 0 || f.Total != 0 {
			return ErrInvalid
		}
	case Fragment:
		if f.Total < 2 || f.Total > MaxFragments || f.Index >= f.Total {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// Encode returns one frame. The transport must preserve frame boundaries.
func Encode(f Frame) ([]byte, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	n := HeaderBytes
	if f.Type == Fragment {
		n = FragmentHeaderBytes
	}
	out := make([]byte, n, len(f.Body)+n)
	out[0], out[1] = f.Type, f.Hops
	if f.Type == Fragment {
		copy(out[2:10], f.ID[:])
		binary.BigEndian.PutUint16(out[10:12], f.Index)
		binary.BigEndian.PutUint16(out[12:14], f.Total)
	}
	return append(out, f.Body...), nil
}

// Decode copies the body so later changes to the input cannot change the frame.
// The body extends to the end of this frame; it has no separate length field.
func Decode(wire []byte) (Frame, error) {
	if len(wire) < HeaderBytes {
		return Frame{}, ErrInvalid
	}
	f := Frame{Type: wire[0], Hops: wire[1]}
	n := HeaderBytes
	if f.Type == Fragment {
		n = FragmentHeaderBytes
		if len(wire) < n {
			return Frame{}, ErrInvalid
		}
		copy(f.ID[:], wire[2:10])
		f.Index = binary.BigEndian.Uint16(wire[10:12])
		f.Total = binary.BigEndian.Uint16(wire[12:14])
	}
	f.Body = wire[n:]
	if err := f.validate(); err != nil {
		return Frame{}, err
	}
	f.Body = append([]byte(nil), f.Body...)
	return f, nil
}

func fragmentID(body []byte) [8]byte {
	sum := sha256.Sum256(body)
	return [8]byte(sum[:8])
}

// Split encodes an event and chooses whole-event or fragment frames. maxFrame
// is the link's usable application payload size, including our frame headers,
// not the BLE ATT MTU. No Bluetooth-specific packet size is assumed.
func Split(event nostr.Event, maxFrame int, hops byte) ([]Frame, error) {
	if maxFrame <= HeaderBytes {
		return nil, ErrSize
	}
	body, err := compact.Encode(event)
	if err != nil {
		return nil, err
	}
	if len(body) <= maxFrame-HeaderBytes {
		return []Frame{{Type: Event, Hops: hops, Body: body}}, nil
	}
	if maxFrame <= FragmentHeaderBytes {
		return nil, ErrSize
	}
	size := maxFrame - FragmentHeaderBytes
	total := (len(body) + size - 1) / size
	if total > MaxFragments {
		return nil, ErrSize
	}
	id := fragmentID(body)
	frames := make([]Frame, 0, total)
	for start := 0; start < len(body); start += size {
		end := min(start+size, len(body))
		frames = append(frames, Frame{Type: Fragment, Hops: hops, ID: id, Index: uint16(len(frames)), Total: uint16(total), Body: body[start:end]})
	}
	return frames, nil
}
