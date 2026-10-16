package rings

import (
	"testing"
	"time"
)

// The expected values come from docs/rings/SPEC.md, sections 3.5 and 3.7:
// a bucket of burst 3 that gets one ring back each 300 s, a daily cap,
// four tries with waits of 30 s, 2 min and 8 min before the jitter.

var t0 = time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)

func fresh() *state {
	return &state{MinInterval: 300, Burst: 3, DailyCap: 48, RingOn: RingOnAny, LastRead: t0.Unix()}
}

// ring sends the ring of s at now, as the relay does, with the answer
// status.
func ring(t *testing.T, s *state, now time.Time, status int) job {
	t.Helper()
	if what, _ := s.next(now); what != send {
		t.Fatalf("no ring goes at %s", now.Format(time.TimeOnly))
	}
	try := s.begin(now, "ring-id")
	s.finish(status, now, 0.5)
	return try
}

func TestAWrapRingsAtOnce(t *testing.T) {
	s := fresh()
	s.wrap(t0, false)
	if what, _ := s.next(t0); what != send {
		t.Fatal("the first wrap after a read does not ring at once")
	}
}

func TestOneRingForEachUnreadPeriod(t *testing.T) {
	s := fresh()
	s.wrap(t0, false)
	ring(t, s, t0, 200)

	s.wrap(t0.Add(time.Minute), false)
	if what, _ := s.next(t0.Add(time.Hour)); what != idle {
		t.Fatal("a second wrap of one unread period rang")
	}

	s.read(t0.Add(2 * time.Hour))
	if what, _ := s.next(t0.Add(2 * time.Hour)); what != idle {
		t.Fatal("a read with no new wrap rang")
	}
	s.wrap(t0.Add(3*time.Hour), false)
	ring(t, s, t0.Add(3*time.Hour), 200)
}

func TestTheSinceIsTheFirstUnreadWrap(t *testing.T) {
	s := fresh()
	s.wrap(t0, false)
	s.wrap(t0.Add(time.Minute), false)
	if try := ring(t, s, t0.Add(time.Minute), 200); try.Since != t0.Unix() {
		t.Fatalf("since is %d, want the first wrap %d", try.Since, t0.Unix())
	}
}

func TestTheBucketLetsABurstGoAndThenPaces(t *testing.T) {
	s := fresh()
	at := t0
	for range 3 {
		s.wrap(at, false)
		ring(t, s, at, 200)
		at = at.Add(time.Second)
		s.read(at)
	}
	// Three rings in three seconds used the bucket. The fourth waits until
	// one ring comes back: 300 s after the bucket was full, less the 2 s
	// that passed.
	s.wrap(at, false)
	what, when := s.next(at)
	if what != wait {
		t.Fatalf("a fourth ring in a burst of 3 does not wait: %v", what)
	}
	if want := t0.Add(300 * time.Second); when.Sub(want).Abs() > time.Second {
		t.Fatalf("the fourth ring waits until %s, want about %s", when.Format(time.TimeOnly), want.Format(time.TimeOnly))
	}
	ring(t, s, when, 200)
}

func TestAWrapInAWatchIsRead(t *testing.T) {
	s := fresh()
	s.wrap(t0, true)
	if what, _ := s.next(t0); what != idle {
		t.Fatal("a wrap that a watching citizen got rang")
	}
}

func TestTheDailyCapHoldsUntilMidnight(t *testing.T) {
	s := fresh()
	s.DailyCap = 2
	at := t0
	for range 2 {
		s.wrap(at, false)
		ring(t, s, at, 200)
		at = at.Add(time.Hour)
		s.read(at)
	}
	s.wrap(at, false)
	what, when := s.next(at)
	midnight := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	if what != wait || !when.Equal(midnight) {
		t.Fatalf("a third ring on a day with daily_cap 2: %v until %s, want a wait until 00:00 UTC", what, when)
	}
	ring(t, s, midnight, 200)
}

func TestAFailedRingTriesFourTimesWithBackoff(t *testing.T) {
	s := fresh()
	s.wrap(t0, false)
	at := t0
	first := ring(t, s, at, 500)
	// With jitter 0.5, each wait is three quarters of 30 s, 2 min, 8 min.
	for i, wait := range []time.Duration{22500 * time.Millisecond, 90 * time.Second, 360 * time.Second} {
		next := at.Add(wait)
		if what, when := s.next(at.Add(time.Millisecond)); !when.Equal(next) {
			t.Fatalf("retry %d: %v at %s, want %s", i+1, what, when.Format(time.TimeOnly), next.Format(time.TimeOnly))
		}
		at = next
		if try := ring(t, s, at, 503); try.ID != first.ID || try.Attempt != i+2 {
			t.Fatalf("retry %d is %+v, want ID %s and attempt %d", i+1, try, first.ID, i+2)
		}
	}
	if what, _ := s.next(at.Add(time.Hour)); what != idle {
		t.Fatal("a fifth try came")
	}
	if s.Used != 4 {
		t.Fatalf("the tries used %d rings of the day, want 4: each try counts", s.Used)
	}
	if s.Tokens != 2 {
		t.Fatalf("the bucket holds %v, want 2: the retries of one ring take no token", s.Tokens)
	}
}

func TestTheJitterStaysInItsBounds(t *testing.T) {
	for attempt, base := range map[int]time.Duration{1: 30 * time.Second, 2: 2 * time.Minute, 3: 8 * time.Minute} {
		if low, high := backoff(attempt, 0), backoff(attempt, 0.999999); low != base/2 || high >= base || high < base*99/100 {
			t.Errorf("attempt %d: waits %s to %s, want %s to just under %s", attempt, low, high, base/2, base)
		}
	}
}

func TestAReadCancelsARetry(t *testing.T) {
	s := fresh()
	s.wrap(t0, false)
	ring(t, s, t0, 500)
	s.read(t0.Add(time.Second))
	if what, _ := s.next(t0.Add(time.Hour)); what != idle {
		t.Fatal("a retry came after the citizen read")
	}
}

func TestAnExpiredRegistrationRingsNoMore(t *testing.T) {
	s := fresh()
	s.Expires = t0.Add(time.Hour).Unix()
	s.wrap(t0.Add(time.Hour), false)
	if what, _ := s.next(t0.Add(time.Hour)); what != idle {
		t.Fatal("an expired registration rang")
	}
}

func TestSevenDaysWithNo2xxRemoveTheRegistration(t *testing.T) {
	s := fresh()
	if s.abandoned(t0.Add(30 * 24 * time.Hour)) {
		t.Fatal("a registration that never rang was removed")
	}
	s.wrap(t0, false)
	ring(t, s, t0, 401)
	if s.abandoned(t0.Add(Abandon - time.Second)) {
		t.Fatal("a registration was removed before 7 days")
	}
	if !s.abandoned(t0.Add(Abandon)) {
		t.Fatal("a registration with 7 days of failures stayed")
	}
	s.finish(200, t0.Add(time.Hour), 0)
	if s.abandoned(t0.Add(8 * 24 * time.Hour)) {
		t.Fatal("a registration that got a 2xx answer was removed")
	}
}
