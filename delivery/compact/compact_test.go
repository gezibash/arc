package compact_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/compact"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/store"
)

func signed(t *testing.T) nostr.Event {
	t.Helper()
	e := nostr.Event{CreatedAt: nostr.Timestamp(time.Now().Unix()), Kind: 3272,
		Tags: nostr.Tags{{"p", "recipient"}, {}, {"", "a\x00b", "你好"}, {"p", "again"}}, Content: "Hello 👋\n\"quoted\"\\"}
	if err := e.Sign(keys.Generate().Secret); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRoundTripAndStoreVerification(t *testing.T) {
	e := signed(t)
	wire, err := compact.Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := compact.Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != e.ID || got.Sig != e.Sig || !bytes.Equal(got.Serialize(), e.Serialize()) {
		t.Fatal("signed fields changed")
	}
	if len(wire) >= len(e.String()) {
		t.Fatal("sample event was not smaller than JSON")
	}
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	result, err := s.Save(got)
	if err != nil || result.Outcome != store.Stored {
		t.Fatalf("valid event: %v %v", result, err)
	}
	// Corrupt only the signature: parsing succeeds, but the store refuses it.
	wire[33] ^= 1
	forged, err := compact.Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	result, err = other.Save(forged)
	if err != nil || result.Outcome != store.Refused {
		t.Fatalf("forged signature: %v %v", result, err)
	}
}

func TestWireVector(t *testing.T) {
	// Fixed structural vector: zero key/signature, timestamp 128, kind 1,
	// tags [["p", "x"], []], content "hi". Not an authenticated event.
	wire, err := hex.DecodeString("01" + strings.Repeat("00", 96) + "80010102020170017800026869")
	if err != nil {
		t.Fatal(err)
	}
	e, err := compact.Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	if e.CreatedAt != 128 || e.Kind != 1 || len(e.Tags) != 2 || e.Tags[0][1] != "x" || len(e.Tags[1]) != 0 || e.Content != "hi" {
		t.Fatalf("unexpected vector: %+v", e)
	}
	got, err := compact.Encode(e)
	if err != nil || !bytes.Equal(got, wire) {
		t.Fatalf("vector encoding: %x %v", got, err)
	}
}

func TestRejectsMalformedInput(t *testing.T) {
	wire, err := compact.Encode(signed(t))
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(wire); n++ {
		if _, err := compact.Decode(wire[:n]); err == nil {
			t.Fatalf("accepted prefix of %d bytes", n)
		}
	}
	fixed := append([]byte{1}, make([]byte, 96)...)
	fields := func(values ...uint64) []byte {
		out := bytes.Clone(fixed)
		for _, v := range values {
			out = binary.AppendUvarint(out, v)
		}
		return out
	}
	cases := [][]byte{
		append(bytes.Clone(wire), 0),
		append([]byte{2}, wire[1:]...),
		append(bytes.Clone(fixed), 0x80, 0), // nonminimal timestamp
		append(bytes.Clone(fixed), bytes.Repeat([]byte{0xff}, 11)...),
		fields(math.MaxUint64, 1, 0, 0),
		fields(0, 65536, 0, 0),
		fields(0, 1, math.MaxUint64, 0),
		fields(0, 1, 1, math.MaxUint64, 0),
		fields(0, 1, 0, math.MaxUint64),
		append(fields(0, 1, 0, 1), 0xff),       // invalid UTF-8 content
		append(fields(0, 1, 1, 1, 1), 0xff, 0), // invalid UTF-8 tag
		make([]byte, compact.MaxBytes+1),
	}
	for i, input := range cases {
		if _, err := compact.Decode(input); err == nil {
			t.Errorf("accepted malformed case %d", i)
		}
	}
}

func TestLimitsAndEncodeRefusals(t *testing.T) {
	e := nostr.Event{Kind: 1}
	// Fixed fields 97 + timestamp/kind/tag-count 3 + content-length varint 3.
	e.Content = strings.Repeat("a", compact.MaxBytes-103)
	e.SetID()
	wire, err := compact.Encode(e)
	if err != nil || len(wire) != compact.MaxBytes {
		t.Fatalf("boundary: %d %v", len(wire), err)
	}
	if _, err := compact.Decode(wire); err != nil {
		t.Fatal(err)
	}
	e.Content += "a"
	e.SetID()
	if _, err := compact.Encode(e); !errors.Is(err, compact.ErrTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	e = signed(t)
	e.CreatedAt = -1
	if _, err := compact.Encode(e); !errors.Is(err, compact.ErrInvalid) {
		t.Fatalf("negative timestamp: %v", err)
	}
	e = signed(t)
	e.Content = "\xff"
	if _, err := compact.Encode(e); !errors.Is(err, compact.ErrInvalid) {
		t.Fatalf("invalid text: %v", err)
	}
	e = signed(t)
	e.Tags[0][0] = "\xff"
	if _, err := compact.Encode(e); !errors.Is(err, compact.ErrInvalid) {
		t.Fatalf("invalid tag: %v", err)
	}
	e = signed(t)
	e.ID[0] ^= 1
	if _, err := compact.Encode(e); !errors.Is(err, compact.ErrInvalid) {
		t.Fatalf("wrong ID: %v", err)
	}
}

func FuzzDecode(f *testing.F) {
	e := nostr.Event{Kind: 1, Content: "hello"}
	e.SetID()
	wire, _ := compact.Encode(e)
	f.Add(wire)
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, input []byte) {
		e, err := compact.Decode(input)
		if err != nil {
			return
		}
		got, err := compact.Encode(e)
		if err != nil || !bytes.Equal(got, input) {
			t.Fatalf("noncanonical round trip: %v", err)
		}
	})
}
