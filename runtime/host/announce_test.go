package host

import (
	"errors"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/runtime/catalog"
)

// The announcer of arc serve signs nothing before a relay answers, says so
// once, and then signs at the time after the newest, never at the clock.
func TestTheAnnouncerSignsNothingBlind(t *testing.T) {
	answers := []bool{false, false, true}
	var signed []nostr.Timestamp
	said := 0
	a := &announcer{
		pull: func() error {
			ok := answers[0]
			answers = answers[1:]
			if !ok {
				return errors.New("no relay answered")
			}
			return nil
		},
		at:   func() (nostr.Timestamp, error) { return 1_900_000_000, nil },
		sign: func(at nostr.Timestamp) error { signed = append(signed, at); return nil },
		say:  func(string) { said++ },
	}
	for range 3 {
		if err := a.announce(); err != nil {
			t.Fatal(err)
		}
	}
	if len(signed) != 1 || signed[0] != 1_900_000_000 || said != 1 {
		t.Errorf("the announcer signed at %v and said %d times, want one signature at the time after the newest, after one word", signed, said)
	}
}

// An announcement far ahead of this clock holds the new one back, but the
// announcer only waits: it says so once, with the gap in words, and signs
// when the clock reaches the cap (#445).
func TestTheAnnouncerWaitsForAnAnnouncementFarAhead(t *testing.T) {
	ahead := true
	var signed int
	var said []string
	a := &announcer{
		pull: func() error { return nil },
		at: func() (nostr.Timestamp, error) {
			if ahead {
				return 0, &catalog.AheadError{ID: "exec", Ahead: 15 * time.Minute}
			}
			return 1_900_000_000, nil
		},
		sign: func(nostr.Timestamp) error { signed++; return nil },
		say:  func(text string) { said = append(said, text) },
	}
	for range 3 {
		if err := a.announce(); err != nil {
			t.Fatalf("an announcement far ahead stopped the announcer: %v", err)
		}
	}
	if signed != 0 || !a.waiting() || len(said) != 1 || !strings.Contains(said[0], "an announcement is 15m0s ahead of this clock; arc announces again in 5m0s") {
		t.Errorf("with one far ahead, the announcer signed %d, waits %v, and said %q", signed, a.waiting(), said)
	}
	ahead = false
	if err := a.announce(); err != nil || signed != 1 || a.waiting() {
		t.Errorf("once the clock reached it, the announcer gave %v, signed %d, waits %v", err, signed, a.waiting())
	}
}
