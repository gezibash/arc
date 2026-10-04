package mail_test

import (
	"context"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/internal/testrelay"
	"github.com/gezibash/arc/internal/testutil"
)

// watch runs Watch until the test ends, and waits until it is ready.
func (c citizen) watch(t *testing.T, r relay.Relay) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- c.mail.Watch(ctx, r, func() { close(ready) }) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the watch ended before it was ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the watch was not ready")
	}
}

// eventually checks a condition each 50 ms, for at most 5 seconds.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if check() {
			return
		}
	}
	t.Fatalf("%s did not happen", what)
}

func state(c citizen) string {
	out := testutil.Must(c.mail.Outbox(context.Background()))
	if len(out) != 1 {
		return ""
	}
	return out[0].State(time.Now())
}

// A message reaches a watching recipient, and its acknowledgement reaches a
// watching sender, with no sync.
func TestAWatchDeliversMailWithoutASync(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	alice.watch(t, r)
	bob.watch(t, r)

	alice.send(t, bob, "pushed, not fetched")
	eventually(t, "bob receives the message", func() bool {
		got := inboxText(bob)
		return len(got) == 1 && got[0] == "pushed, not fetched"
	})
	eventually(t, "alice sees the acknowledgement", func() bool { return state(alice) == "delivered" })
}

// A message that could not leave at once leaves when its sender watches a
// relay: the watch flushes the outbox first.
func TestAWatchSendsWhatTheOutboxHolds(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t), newCitizen(t, r)
	bob.watch(t, r)

	alice.send(t, bob, "waited in the outbox")
	time.Sleep(200 * time.Millisecond)
	if got := inboxText(bob); len(got) != 0 {
		t.Fatalf("bob got %q before alice had a relay", got)
	}
	alice.watch(t, r)
	eventually(t, "bob receives the queued message", func() bool { return len(inboxText(bob)) == 1 })
}

// A call that arrived before its provider was attached is answered when the
// provider watches, and the reply reaches the watching caller.
func TestAWatchAnswersAPendingCall(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	if _, err := alice.mail.Request(context.Background(), bob.key.Public, call.Request{Capability: "counter", Body: "increment"}); err != nil {
		t.Fatal(err)
	}
	bob.sync(t, r) // receipt only: bob has no provider yet
	executions := 0
	bob.mail.OnRequest = func(context.Context, nostr.Event) (call.Reply, error) {
		executions++
		return call.Reply{Body: "1"}, nil
	}
	alice.watch(t, r)
	bob.watch(t, r)
	eventually(t, "alice gets the reply", func() bool {
		out := testutil.Must(alice.mail.Outbox(context.Background()))
		return len(out) == 1 && out[0].Reply.Body == "1"
	})
	if executions != 1 {
		t.Fatalf("the provider ran %d times, want 1", executions)
	}
}

// Two relays deliver the same wrap. The recipient keeps one message, and
// sends one acknowledgement.
func TestTwoWatchesTakeOneMessageOnce(t *testing.T) {
	first, second := relay.Relay{URL: testrelay.Start(t)}, relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, first, second), newCitizen(t, first, second)
	bob.watch(t, first)
	bob.watch(t, second)
	alice.send(t, bob, "twice on the wire")
	eventually(t, "bob receives the message", func() bool { return len(inboxText(bob)) == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := inboxText(bob); len(got) != 1 {
		t.Fatalf("bob's inbox holds %d messages, want 1", len(got))
	}
	if report := alice.sync(t, first); report.Delivered != 1 {
		t.Fatalf("alice saw %d acknowledgements, want 1", report.Delivered)
	}
}

// The route tags change each day, so a watch ends at midnight UTC, and its
// owner starts a new one.
func TestAWatchEndsAtMidnight(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	bob := newCitizen(t, r)
	almost := time.Date(2026, 10, 4, 23, 59, 59, 500_000_000, time.UTC)
	mail.SetNow(bob.mail, func() time.Time { return almost })
	done := make(chan error, 1)
	go func() { done <- bob.mail.Watch(context.Background(), r, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the watch ended with %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not end at midnight")
	}
}

// A courier can post the courier form of a message to a relay. That form
// names no recipient, only a route tag, and the watch of the recipient finds
// it by the tag.
func TestAWatchFindsTheCourierFormByItsRouteTag(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	courier := stick(t)
	alice, bob := newCitizen(t), newCitizen(t, r)
	bob.watch(t, r)

	alice.send(t, bob, "carried, then posted")
	alice.sync(t, courier)
	batch, err := courier.Fetch(context.Background(), nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}})
	if err != nil {
		t.Fatal(err)
	}
	posted := 0
	for _, wrap := range batch.Events {
		if wrap.Tags.Find("w") != nil && wrap.Tags.Find("p") == nil {
			if err := r.Send(context.Background(), wrap); err != nil {
				t.Fatal(err)
			}
			posted++
		}
	}
	if posted != 1 {
		t.Fatalf("the stick held %d courier forms, want 1", posted)
	}
	eventually(t, "bob receives the courier form", func() bool { return len(inboxText(bob)) == 1 })
}
