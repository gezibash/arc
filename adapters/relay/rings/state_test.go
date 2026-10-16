package rings

import (
	"testing"
	"time"
)

// The expected values come from docs/rings/SPEC.md, sections 3.4 and 3.6.

var t0 = time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)

func fresh() *state {
	return &state{MinInterval: 300, DailyCap: 48, RingOn: RingOnAny, LastRead: t0.Unix()}
}

// ring tries the ring of s at now, as the relay does, with the answer
// status.
func ring(t *testing.T, s *state, now time.Time, status int) {
	t.Helper()
	if !s.due(now) {
		t.Fatalf("no ring is due at %s", now.Format(time.TimeOnly))
	}
	s.try(now)
	s.answer(status, now)
}

func TestOneRingForEachUnreadPeriod(t *testing.T) {
	s := fresh()
	s.wrap(t0.Add(time.Minute), false)
	ring(t, s, t0.Add(time.Minute+settle), 200)

	// A second wrap in the same unread period rings no more, even after
	// the interval.
	s.wrap(t0.Add(2*time.Minute), false)
	if s.due(t0.Add(time.Hour)) {
		t.Fatal("a second wrap of one unread period rang")
	}

	// A read ends the period. The next wrap rings again.
	s.read(t0.Add(2 * time.Hour))
	if s.due(t0.Add(2 * time.Hour)) {
		t.Fatal("a read with no new wrap rang")
	}
	s.wrap(t0.Add(3*time.Hour), false)
	ring(t, s, t0.Add(3*time.Hour+settle), 200)
	if s.Used != 2 {
		t.Fatalf("used %d rings, want 2", s.Used)
	}
}

func TestTheSinceIsTheFirstUnreadWrap(t *testing.T) {
	s := fresh()
	first := t0.Add(time.Minute)
	s.wrap(first, false)
	s.wrap(first.Add(time.Minute), false)
	if s.Since != first.Unix() {
		t.Fatalf("since is %d, want the first wrap %d", s.Since, first.Unix())
	}
}

func TestTheIntervalHoldsARingUntilItEnds(t *testing.T) {
	s := fresh()
	s.wrap(t0, false)
	ring(t, s, t0.Add(settle), 200)
	s.read(t0.Add(time.Minute))
	s.wrap(t0.Add(2*time.Minute), false)

	// The last ring was at t0+1s, so the interval of 300 s ends at t0+301s.
	end := t0.Add(settle + 300*time.Second)
	if s.due(end.Add(-time.Second)) {
		t.Fatal("a ring came before min_interval passed")
	}
	ring(t, s, end, 200)
}

func TestAWrapThatArrivesWhileTheCitizenWatchesIsRead(t *testing.T) {
	s := fresh()
	s.wrap(t0, true)
	if s.due(t0.Add(time.Hour)) {
		t.Fatal("a wrap that a watching citizen got rang")
	}
}

func TestAWrapWaitsToSettle(t *testing.T) {
	s := fresh()
	s.wrap(t0, false)
	if s.due(t0) {
		t.Fatal("a wrap rang before an open subscription could read it")
	}
}

func TestTheDailyCapHoldsAndTheNextDayRings(t *testing.T) {
	s := fresh()
	s.DailyCap = 2
	s.MinInterval = 60
	at := t0
	for range 2 {
		s.wrap(at, false)
		ring(t, s, at.Add(settle), 200)
		at = at.Add(time.Hour)
		s.read(at)
	}
	s.wrap(at, false)
	if s.due(at.Add(time.Hour)) {
		t.Fatal("a third ring came on a day with daily_cap 2")
	}
	if left := s.left(at); left != 0 {
		t.Fatalf("left %d rings today, want 0", left)
	}

	// At 00:00 UTC the day has rings again, and the period is still unread.
	midnight := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	if s.left(midnight) != 2 {
		t.Fatalf("left %d rings on the next day, want 2", s.left(midnight))
	}
	ring(t, s, midnight, 200)
}

func TestAFailedRingTriesAgainAfterOneAndFiveMinutes(t *testing.T) {
	s := fresh()
	s.wrap(t0, false)
	first := t0.Add(settle)
	ring(t, s, first, 500)

	if s.due(first.Add(time.Minute - time.Second)) {
		t.Fatal("a retry came before 1 minute")
	}
	second := first.Add(time.Minute)
	ring(t, s, second, 0)

	if s.due(second.Add(5*time.Minute - time.Second)) {
		t.Fatal("the last retry came before 5 minutes")
	}
	third := second.Add(5 * time.Minute)
	ring(t, s, third, 503)

	if s.due(third.Add(time.Hour)) {
		t.Fatal("a fourth try came after two retries")
	}
	if s.Used != 3 {
		t.Fatalf("the tries used %d rings, want 3: each try counts", s.Used)
	}
}

func TestAReadCancelsARetry(t *testing.T) {
	s := fresh()
	s.wrap(t0, false)
	ring(t, s, t0.Add(settle), 500)
	s.read(t0.Add(30 * time.Second))
	if s.due(t0.Add(2 * time.Minute)) {
		t.Fatal("a retry came after the citizen read")
	}
}

func TestAnAnswerOf2xxEndsTheRetries(t *testing.T) {
	s := fresh()
	s.wrap(t0, false)
	ring(t, s, t0.Add(settle), 500)
	ring(t, s, t0.Add(settle+time.Minute), 202)
	if s.due(t0.Add(time.Hour)) {
		t.Fatal("a retry came after a 2xx answer")
	}
}

func TestSevenDaysWithNo2xxRemoveTheRegistration(t *testing.T) {
	s := fresh()
	if s.abandoned(t0.Add(30 * 24 * time.Hour)) {
		t.Fatal("a registration that never rang was removed")
	}
	s.wrap(t0, false)
	ring(t, s, t0.Add(settle), 401)
	failed := t0.Add(settle)
	if s.abandoned(failed.Add(Abandon - time.Second)) {
		t.Fatal("a registration was removed before 7 days")
	}
	if !s.abandoned(failed.Add(Abandon)) {
		t.Fatal("a registration with 7 days of failures stayed")
	}

	// A 2xx answer before then keeps it.
	s = fresh()
	s.wrap(t0, false)
	ring(t, s, t0.Add(settle), 401)
	ring(t, s, t0.Add(settle+time.Minute), 200)
	if s.abandoned(t0.Add(8 * 24 * time.Hour)) {
		t.Fatal("a registration that got a 2xx answer was removed")
	}
}
