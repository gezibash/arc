package relay

import (
	"context"
	"errors"
	"testing"

	"fiatjaf.com/nostr"
)

// ended is a subscription as the library leaves it after a message from the
// relay: the message waits on its channel, and Events is closed.
func ended(eose bool, reason string) *nostr.Subscription {
	sub := &nostr.Subscription{
		Events:            make(chan nostr.Event),
		EndOfStoredEvents: make(chan nostr.EndOfStoredEvent, 1),
		ClosedReason:      make(chan string, 1),
	}
	if eose {
		sub.EndOfStoredEvents <- nostr.EndOfStoredEvent{}
	}
	if reason != "" {
		sub.ClosedReason <- reason
	}
	close(sub.Events)
	return sub
}

// A relay that serves parts only to their author answers CLOSED
// auth-required. When drain comes to the subscription late, the reason and
// the closed Events are both ready. Each run picks one at random, so many
// runs make a lost reason certain to show.
func TestARequestForAuthenticationIsNotLost(t *testing.T) {
	for range 200 {
		_, err := drain(context.Background(), ended(false, "auth-required: sealed data is served only to its author"))
		if !errors.Is(err, errAuthRequired) {
			t.Fatalf("drain returned %v, want the request for authentication", err)
		}
	}
}

// A relay can close a query just after its end of stored events.
func TestAnEndOfStoredEventsIsNotLost(t *testing.T) {
	for range 200 {
		if _, err := drain(context.Background(), ended(true, "")); err != nil {
			t.Fatalf("drain returned %v, want the end of stored events", err)
		}
	}
}

// A subscription that ends with no message is an error.
func TestASubscriptionThatEndsWithNoMessageFails(t *testing.T) {
	if _, err := drain(context.Background(), ended(false, "")); err == nil {
		t.Fatal("drain returned no error")
	}
}
