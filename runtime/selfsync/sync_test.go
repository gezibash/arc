package selfsync

import (
	"testing"
	"time"
)

// TestSilence checks the record of decision 10 of the storage network
// proposal: a device warns after 7 days with no answer, counted from the
// first failed open after the last answer.
func TestSilence(t *testing.T) {
	s := &Syncer{State: t.TempDir()}
	day := 24 * time.Hour
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	silence := func(at time.Time) time.Duration {
		t.Helper()
		d, err := s.Unanswered("charlie", at)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	// A storage citizen that never answered: the first failure starts the
	// count.
	if d := silence(t0); d != 0 {
		t.Fatalf("the first failure gives a silence of %s; want 0", d)
	}
	if d := silence(t0.Add(6 * day)); d >= SilentWarning {
		t.Fatalf("6 days give %s, which warns", d)
	}
	if d := silence(t0.Add(7 * day)); d < SilentWarning {
		t.Fatalf("7 days give %s, which does not warn", d)
	}

	// An answer ends the silence. A device that was off for 30 days does
	// not count them: the count starts at its next failed open.
	if err := s.Answered("charlie", t0.Add(8*day)); err != nil {
		t.Fatal(err)
	}
	if d := silence(t0.Add(38 * day)); d != 0 {
		t.Fatalf("the first failure after an answer gives %s; want 0", d)
	}
	if d := silence(t0.Add(45 * day)); d != 7*day {
		t.Fatalf("7 days of failures after an answer give %s; want 7 days", d)
	}
}
