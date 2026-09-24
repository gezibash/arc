package frame

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/compact"
	"github.com/gezibash/arc/delivery/keys"
)

func event(t *testing.T) nostr.Event {
	t.Helper()
	e := nostr.Event{Kind: 1, CreatedAt: 1, Content: strings.Repeat("hello 🌍", 80)}
	if err := e.Sign(keys.Generate().Secret); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRoundTrip(t *testing.T) {
	e := event(t)
	for _, limit := range []int{20, 100, 512, 2000} {
		frames, err := Split(e, limit, 7)
		if err != nil {
			t.Fatal(err)
		}
		var a Assembler
		var got *Completed
		now := time.Now()
		// Reverse arrival order, with every incomplete piece duplicated.
		for i := len(frames) - 1; i >= 0; i-- {
			wire, err := Encode(frames[i])
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) > limit {
				t.Fatal("link limit exceeded")
			}
			f, err := Decode(wire)
			if err != nil {
				t.Fatal(err)
			}
			f.Hops = 3
			got, err = a.Add(f, now)
			if err != nil {
				t.Fatal(err)
			}
			if i > 0 {
				if got != nil {
					t.Fatal("completed with missing pieces")
				}
				f.Hops = 2
				if duplicate, err := a.Add(f, now); duplicate != nil || err != nil {
					t.Fatal("duplicate changed assembly")
				}
			}
		}
		if got == nil || got.Event.ID != e.ID || got.Event.Sig != e.Sig || !bytes.Equal(got.Event.Serialize(), e.Serialize()) {
			t.Fatal("event changed")
		}
		wantHops := byte(2)
		if len(frames) == 1 {
			wantHops = 3
		}
		if got.Hops != wantHops || a.buffered != 0 || len(a.pending) != 0 {
			t.Fatal("completion state")
		}
	}
}

func TestWireVectorAndValidation(t *testing.T) {
	wire, _ := hex.DecodeString("02070102030405060708000100026162")
	f, err := Decode(wire)
	if err != nil || f.Index != 1 || f.Total != 2 || f.Hops != 7 || string(f.Body) != "ab" {
		t.Fatalf("vector: %+v %v", f, err)
	}
	encoded, err := Encode(f)
	if err != nil || !bytes.Equal(encoded, wire) {
		t.Fatal("wire mismatch")
	}
	wire[len(wire)-1] = 0
	if string(f.Body) != "ab" {
		t.Fatal("decode aliases input")
	}
	for n := 0; n <= FragmentHeaderBytes; n++ {
		if _, err := Decode(encoded[:n]); err == nil {
			t.Fatalf("accepted empty/truncated frame %d", n)
		}
	}
	for _, f := range []Frame{
		{Type: 0, Body: []byte{1}}, {Type: 3, Body: []byte{1}},
		{Type: Event}, {Type: Event, Index: 1, Body: []byte{1}},
		{Type: Fragment, Total: 1, Body: []byte{1}},
		{Type: Fragment, Total: 2, Index: 2, Body: []byte{1}},
		{Type: Fragment, Total: MaxFragments + 1, Body: []byte{1}},
		{Type: Event, Body: make([]byte, compact.MaxBytes+1)},
	} {
		if _, err := Encode(f); err == nil {
			t.Fatal("accepted invalid frame")
		}
		var a Assembler
		if _, err := a.Add(f, time.Now()); err == nil {
			t.Fatal("assembly accepted invalid frame")
		}
	}
	e := event(t)
	for _, limit := range []int{-1, 0, 2, 14} {
		if _, err := Split(e, limit, 1); err == nil {
			t.Fatal("accepted tiny limit")
		}
	}
	e.Content = strings.Repeat("a", MaxFragments+1)
	e.SetID()
	if _, err := Split(e, 15, 1); !errors.Is(err, ErrSize) {
		t.Fatal("fragment count limit")
	}
}

func piece(id byte, body []byte) Frame {
	return Frame{Type: Fragment, ID: [8]byte{id}, Total: 2, Body: body, Hops: 7}
}

func TestTimeoutAndCapacity(t *testing.T) {
	now := time.Now()
	var a Assembler
	for i := 0; i < MaxAssemblies; i++ {
		if _, err := a.Add(piece(byte(i), []byte{1}), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Add(piece(200, []byte{1}), now); !errors.Is(err, ErrCapacity) {
		t.Fatal("assembly count unbounded")
	}
	if _, err := a.Add(piece(0, []byte{1}), now.Add(29*time.Second)); err != nil {
		t.Fatal(err)
	}
	if n := a.Expire(now.Add(AssemblyLifetime)); n != MaxAssemblies || a.buffered != 0 {
		t.Fatal("duplicates extended deadline or leaked memory")
	}
	for i := 0; i < 8; i++ {
		if _, err := a.Add(piece(byte(i), make([]byte, compact.MaxBytes)), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Add(piece(9, []byte{1}), now); !errors.Is(err, ErrCapacity) {
		t.Fatal("global byte limit")
	}
	// Oversize continuation drops only its own assembly and releases its bytes.
	f := piece(0, []byte{1})
	f.Index = 1
	if _, err := a.Add(f, now); !errors.Is(err, ErrSize) || a.buffered != 7*compact.MaxBytes {
		t.Fatal("per-event byte limit")
	}
	if _, err := a.Add(piece(9, []byte{1}), now); err != nil {
		t.Fatal("capacity not recovered")
	}
	a.Expire(now.Add(AssemblyLifetime))
	if a.buffered != 0 {
		t.Fatal("bytes leaked")
	}
}

func TestConflictsAndCorruption(t *testing.T) {
	now := time.Now()
	for _, change := range []func(*Frame){
		func(f *Frame) { f.Total++ },
		func(f *Frame) { f.Body = []byte{2} },
	} {
		var a Assembler
		f := piece(1, []byte{1})
		a.Add(f, now)
		change(&f)
		if _, err := a.Add(f, now); !errors.Is(err, ErrConflict) || len(a.pending) != 0 || a.buffered != 0 {
			t.Fatal("conflict retained")
		}
	}
	frames, _ := Split(event(t), 100, 7)
	frames[0].Body = bytes.Clone(frames[0].Body)
	frames[0].Body[0] ^= 1
	var a Assembler
	for i, f := range frames {
		got, err := a.Add(f, now)
		if got != nil {
			t.Fatal("corrupt event completed")
		}
		if i == len(frames)-1 && !errors.Is(err, ErrConflict) {
			t.Fatal("digest not checked")
		}
	}
	if len(a.pending) != 0 || a.buffered != 0 {
		t.Fatal("corruption leaked memory")
	}
	// Matching fragment IDs are not enough: the compact body must parse too.
	body := []byte{99, 0}
	id := fragmentID(body)
	for i := range body {
		_, err := a.Add(Frame{Type: Fragment, ID: id, Total: 2, Index: uint16(i), Body: body[i : i+1]}, now)
		if i == 1 && err == nil {
			t.Fatal("invalid compact event accepted")
		}
	}
}

func TestExactLimitAndOwnership(t *testing.T) {
	e := nostr.Event{Kind: 1, Content: strings.Repeat("a", compact.MaxBytes-103)}
	e.SetID()
	frames, err := Split(e, 512, 0)
	if err != nil {
		t.Fatal(err)
	}
	var a Assembler
	now := time.Now()
	for i, f := range frames {
		got, err := a.Add(f, now)
		if err != nil {
			t.Fatal(err)
		}
		// The assembler must retain its own copy of an incomplete piece.
		f.Body[0] ^= 1
		if i == len(frames)-1 && (got == nil || got.Event.ID != e.ID) {
			t.Fatal("boundary round trip")
		}
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte{1, 0, 1})
	f.Add([]byte{2, 7, 1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 2, 1})
	f.Fuzz(func(t *testing.T, wire []byte) {
		frame, err := Decode(wire)
		if err != nil {
			return
		}
		encoded, err := Encode(frame)
		if err != nil || !bytes.Equal(encoded, wire) {
			t.Fatal("wire changed")
		}
		var a Assembler
		a.Add(frame, time.Time{})
		a.Expire(time.Time{}.Add(AssemblyLifetime))
		if a.buffered != 0 || len(a.pending) != 0 {
			t.Fatal("expiry leaked assembly")
		}
	})
}
