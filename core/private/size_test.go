package private_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
)

// fullRumor is a message with MaxMailContent bytes of content, whose JSON
// holds MaxRumor+extra bytes. A quote takes 2 bytes in JSON, so the quotes
// in the content set the size of the JSON.
func fullRumor(t *testing.T, from keys.Key, to nostr.PubKey, extra int) nostr.Event {
	t.Helper()
	tags := nostr.Tags{{"p", to.Hex()}}
	now := time.Unix(1_800_000_000, 0)
	plain := private.Rumor(from, 14, strings.Repeat("x", private.MaxMailContent), tags, now)
	quotes := private.MaxRumor + extra - len(plain.String())
	if quotes < 0 || quotes > private.MaxMailContent {
		t.Fatalf("cannot make a rumor of %d bytes with %d quotes", private.MaxRumor+extra, quotes)
	}
	content := strings.Repeat(`"`, quotes) + strings.Repeat("x", private.MaxMailContent-quotes)
	rumor := private.Rumor(from, 14, content, tags, now)
	if got := len(rumor.String()); got != private.MaxRumor+extra {
		t.Fatalf("the rumor holds %d bytes, not %d", got, private.MaxRumor+extra)
	}
	return rumor
}

// The 2026-10-11 measurement: 28,394 bytes of plain text was the largest
// message whose relay-form wrap fit 65,536 bytes, and that wrap held 55,149
// bytes. A stamp tag adds 485 bytes: 354 bytes are 472 characters of
// base64, in `,["stamp","…"]`. The limits must agree with both.
func TestTheWrapLimitsAgreeWithTheMeasurement(t *testing.T) {
	if private.MaxWrap != 55_149+485 {
		t.Errorf("MaxWrap %d; measured 55,149, and a stamp tag adds 485", private.MaxWrap)
	}
}

// The tags that admit a wrap to an inbox relay, at their largest.
var admitTags = map[string]nostr.Tag{
	"nonce": {"nonce", "18446744073709551615", "20"},
	"stamp": {"stamp", strings.Repeat("A", 472)},
}

// The largest rumor that mail takes, sealed and wrapped with the real code,
// gives a wrap of 55,149 bytes, and of at most MaxWrap bytes with the
// largest tag that admits it to an inbox relay. The seal stays under the NIP-44
// plaintext limit of 65,535 bytes, and the content of the wrap under the
// 65,535 bytes that the Bolt event store holds.
func TestTheLargestMailFitsTheWrapLimit(t *testing.T) {
	alice, bob := keys.Generate(), keys.Generate()
	ctx := context.Background()
	for _, rumor := range []nostr.Event{
		private.Rumor(alice, 14, strings.Repeat("x", private.MaxMailContent), nostr.Tags{{"p", bob.Public.Hex()}}, time.Now()),
		fullRumor(t, alice, bob.Public, 0),
	} {
		if err := private.CheckMail(rumor); err != nil {
			t.Fatalf("a rumor of %d bytes: %v", len(rumor.String()), err)
		}
		seal, err := private.Seal(ctx, alice, bob.Public, rumor)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(seal.String()); n > private.MaxSeal || n > 65_535 {
			t.Errorf("the seal holds %d bytes; the limit is %d, and NIP-44 takes 65,535", n, private.MaxSeal)
		}
		for _, form := range []private.Form{private.RelayForm, private.CourierForm} {
			w, err := private.WrapSeal(seal, bob.Public, form, private.WrapKind, time.Now().Add(private.MaxAge))
			if err != nil {
				t.Fatal(err)
			}
			n := len(w.String())
			if len(w.Content) > 65_535 {
				t.Errorf("form %d: the wrap holds %d bytes of content, more than the store takes", form, len(w.Content))
			}
			if n > private.MaxWrap {
				t.Errorf("form %d: the wrap holds %d bytes, more than %d", form, n, private.MaxWrap)
			}
			if form == private.RelayForm && rumor.Content != strings.Repeat("x", private.MaxMailContent) && n != 55_149 {
				t.Errorf("the relay-form wrap holds %d bytes; measured 55,149", n)
			}
			for name, tag := range admitTags {
				tagged := w
				tagged.Tags = append(slices.Clone(w.Tags), tag)
				if n := len(tagged.String()); n > private.MaxWrap {
					t.Errorf("form %d with a %s tag: the wrap holds %d bytes, more than %d", form, name, n, private.MaxWrap)
				} else if form == private.RelayForm && name == "stamp" && rumor.Content != strings.Repeat("x", private.MaxMailContent) && n != private.MaxWrap {
					t.Errorf("the relay-form wrap with a stamp holds %d bytes; the computed limit is %d", n, private.MaxWrap)
				}
			}
		}
	}
}

func TestMailRefusesOneByteMore(t *testing.T) {
	alice, bob := keys.Generate(), keys.Generate()
	tags := nostr.Tags{{"p", bob.Public.Hex()}}
	for name, rumor := range map[string]nostr.Event{
		"content of 24 KiB + 1": private.Rumor(alice, 14, strings.Repeat("x", private.MaxMailContent+1), tags, time.Now()),
		"JSON of 28 KiB + 1":    fullRumor(t, alice, bob.Public, 1),
	} {
		err := private.CheckMail(rumor)
		if !errors.Is(err, private.ErrTooLarge) || !strings.Contains(err.Error(), "transfer") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
