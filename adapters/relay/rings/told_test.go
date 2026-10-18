package rings

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
)

// A small daily_cap whose last used day was yesterday says nothing today.
func TestASmallCapAfterAUsedDaySaysNothing(t *testing.T) {
	s := fresh()
	s.DailyCap, s.Used, s.Day = 3, 2, "2026-10-15"
	if got := s.low(t0); got != "" {
		t.Fatalf("a new day of daily_cap 3 after 2 used yesterday said %q", got)
	}
}

// The day is the UTC day, whatever the zone of the time.
func TestTheDayIsUTC(t *testing.T) {
	tirane := time.FixedZone("UTC+2", 2*60*60)
	if got := day(time.Date(2026, 10, 24, 0, 30, 0, 0, tirane)); got != "2026-10-23" {
		t.Fatalf("00:30 at UTC+2 is day %s, want 2026-10-23", got)
	}
}

// The day is the UTC day, also when the local zone is UTC+2.
func TestTheDayIsUTCNotLocal(t *testing.T) {
	// Run with TZ=Europe/Tirane (UTC+2 in October); it does not set
	// time.Local itself, which would race other goroutines.
	before := time.Date(2026, 10, 23, 21, 59, 58, 0, time.UTC)
	after := time.Date(2026, 10, 23, 22, 0, 1, 0, time.UTC)
	if _, off := after.Local().Zone(); off == 0 {
		t.Skip("the local zone is UTC; run with TZ=Europe/Tirane")
	}
	s := fresh()
	s.DailyCap = 6
	s.count(before)
	s.count(before)
	want := "arc-ring: 4 of 6 rings left today; then rings wait until 00:00 UTC"
	if got := s.low(after); got != want {
		t.Fatalf("after local midnight: %q, want %q", got, want)
	}
	if got := s.low(time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)); got != "" {
		t.Fatalf("after UTC midnight: %q", got)
	}
}

// lowCitizen registers a citizen with daily_cap 2 and uses one ring, so
// that each of its reads gets the NOTICE of rings that run low.
func lowCitizen(t *testing.T, h *harness) keys.Key {
	t.Helper()
	bob := keys.Generate()
	if code, body := h.register(bob, Request{URL: "https://example.com/hook", Key: ringKey, DailyCap: 2}); code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, TestPath, bob, nil, nil); code != http.StatusOK {
		t.Fatalf("ring test: %d %s", code, body)
	}
	return bob
}

// toldEmpty waits until the relay holds no told set, and fails if one stays.
func toldEmpty(t *testing.T, h *harness, after string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.rings.mu.Lock()
		n, m := len(h.rings.told), len(h.rings.subs)
		h.rings.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("told holds %d connections after %s (subs %d)", n, after, m)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The told set of a connection is freed when the connection ends.
func TestToldIsFreedOnDisconnect(t *testing.T) {
	h := start(t, Config{})
	bob := lowCitizen(t, h)
	for range 5 {
		if got := h.notices(bob, bob.Public, 2); len(got) != 1 {
			t.Fatalf("got %q", got)
		}
	}
	toldEmpty(t, h, "5 closed connections")
}

// A read that races the close of its connection must not leave the told set
// of that connection behind: khatru removes the listeners of a closed
// connection after OnDisconnect, and the read can tell between the two.
func TestAReadThatRacesTheCloseLeavesNoToldSet(t *testing.T) {
	h := start(t, Config{})
	bob := lowCitizen(t, h)
	ctx := context.Background()
	filter := nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {bob.Public.Hex()}}}
	// 20 at a time: more dials at once overflow the listen queue.
	slots := make(chan struct{}, 20)
	var wg sync.WaitGroup
	for range 300 {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			conn, err := nostr.RelayConnect(ctx, h.ws, nostr.RelayOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			authenticate(t, conn, bob)
			for range 8 {
				_, _ = conn.Subscribe(ctx, filter, nostr.SubscriptionOptions{})
			}
			_ = conn.Close()
		}()
	}
	wg.Wait()
	toldEmpty(t, h, "300 connections that read and closed")
}
