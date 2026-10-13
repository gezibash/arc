package draft

import (
	"bytes"
	"context"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
)

// TestABodyOpensOnlyWithTheKeyOfItsApp checks section 3.9.5 of the
// interface spec: the body key comes from the content key of one app, and
// the body tag of the wrap binds the encrypted bytes.
func TestABodyOpensOnlyWithTheKeyOfItsApp(t *testing.T) {
	ctx := context.Background()
	citizen := nostr.Generate()
	s := Sealer{Citizen: citizen.Public(), Signer: keyer.NewPlainKeySigner(citizen), Gen: 3, Key: [32]byte{1}}
	raw := bytes.Repeat([]byte("file bytes "), 1000)
	sealed, ref, err := s.SealBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) != len(raw)+BodyOverhead || bytes.Contains(sealed, raw[:20]) {
		t.Fatalf("the body has %d bytes, or holds the raw bytes", len(sealed))
	}
	wrap, err := s.WrapBody(ctx, "file/1", nostr.Event{Kind: 1063}, ref, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := Body(wrap); !ok || got != ref {
		t.Fatalf("the wrap names %+v, want %+v", got, ref)
	}
	keyOf := func(key byte) Opener {
		return Opener{Citizen: citizen.Public(), Key: func(_ context.Context, gen uint32) ([32]byte, error) {
			if gen != 3 {
				t.Fatalf("the opener asked for generation %d", gen)
			}
			return [32]byte{key}, nil
		}}
	}
	if got, err := keyOf(1).OpenBody(ctx, wrap, sealed); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("the body opened as %d bytes: %v", len(got), err)
	}
	if _, err := keyOf(2).OpenBody(ctx, wrap, sealed); err == nil {
		t.Error("the body opened with the key of another app")
	}
	// The body of another file opens with the same key, so only the body
	// tag tells that it is not the body of this wrap.
	other, _, err := s.SealBody([]byte("another file"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyOf(1).OpenBody(ctx, wrap, other); err == nil {
		t.Error("the body of another wrap opened")
	}
	if _, _, err := s.SealBody(make([]byte, MaxBody+1)); err == nil {
		t.Errorf("a body of %d bytes was sealed", MaxBody+1)
	}
}
