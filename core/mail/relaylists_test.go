package mail_test

import (
	"context"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/internal/testrelay"
	"github.com/gezibash/arc/internal/testutil"
)

// publishList puts a NIP-17 relay list of c, naming urls, on the relay r.
func publishList(t *testing.T, c citizen, r relay.Relay, at nostr.Timestamp, urls ...string) {
	t.Helper()
	list, err := mail.RelayList(context.Background(), c.key, urls, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Send(context.Background(), list); err != nil {
		t.Fatal(err)
	}
}

func sendOne(t *testing.T, from, to citizen, text string) mail.Outgoing {
	t.Helper()
	out, err := from.mail.Send(context.Background(), []nostr.PubKey{to.key.Public}, text)
	if err != nil {
		t.Fatal(err)
	}
	return out[0]
}

// Bob moves from one relay to another and publishes a new list. Alice
// holds his old list from her first message, and her next message still
// reaches him: each send looks the list up again (Cato's test of v0.36.1,
// 2026-10-19: after a swap of relays, no message arrived).
func TestASendFollowsANewerRelayList(t *testing.T) {
	shared := relay.Relay{URL: testrelay.Start(t)}
	before := relay.Relay{URL: testrelay.Start(t)}
	after := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, shared), newCitizen(t, after)

	now := nostr.Now()
	publishList(t, bob, shared, now-60, before.URL)
	sendOne(t, alice, bob, "to the old relay")
	if got := bob.sync(t, before); got.Received != 1 {
		t.Fatalf("the first message did not follow the first list: bob received %d", got.Received)
	}

	publishList(t, bob, shared, now, after.URL)
	out := sendOne(t, alice, bob, "to the new relay")
	if out.Inbox != mail.InboxFound {
		t.Errorf("the lookup of the new list gave %q, want %q", out.Inbox, mail.InboxFound)
	}
	if got := bob.sync(t, after); got.Received != 1 {
		t.Fatalf("the second message went to the old list: bob received %d on his new relay", got.Received)
	}
}

// Alice sends before Bob's list can be found. When it appears, the next
// Forward sends the pending message to his relay. A message that is
// delivered, or a relay that has the wrap, gets nothing again.
func TestPendingMailGoesToARelayListThatAppearsLater(t *testing.T) {
	shared := relay.Relay{URL: testrelay.Start(t)}
	bobs := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, shared), newCitizen(t, bobs)
	ctx := context.Background()

	out := sendOne(t, alice, bob, "waiting for your list")
	if out.Inbox != mail.InboxNone {
		t.Errorf("a send with no list for bob gave %q, want %q", out.Inbox, mail.InboxNone)
	}
	if got := bob.sync(t, bobs); got.Received != 0 {
		t.Fatalf("bob received %d messages before his list was published", got.Received)
	}

	publishList(t, bob, shared, nostr.Now(), bobs.URL)
	sent := testutil.Must(alice.mail.Forward(ctx))
	if sent != 1 {
		t.Errorf("Forward sent %d messages, want 1", sent)
	}
	if got := bob.sync(t, bobs); got.Received != 1 {
		t.Fatalf("bob received %d messages after Forward, want 1", got.Received)
	}
	if again := testutil.Must(alice.mail.Forward(ctx)); again != 0 {
		t.Errorf("a second Forward sent %d messages to relays that have them", again)
	}

	// Alice has no relay list, so Bob's acknowledgement waits on his relay.
	// When alice reads it there, the message is delivered, and Forward
	// leaves it alone even after Bob moves to a new relay.
	if got := alice.sync(t, bobs); got.Delivered != 1 {
		t.Fatalf("alice saw %d deliveries, want 1", got.Delivered)
	}
	moved := relay.Relay{URL: testrelay.Start(t)}
	publishList(t, bob, shared, nostr.Now()+1, moved.URL)
	if after := testutil.Must(alice.mail.Forward(ctx)); after != 0 {
		t.Errorf("Forward sent %d messages of a delivered message", after)
	}
}

// When no relay or indexer answers the lookup, the send uses the list in
// the store, and says so.
func TestASendWithNoAnswerUsesTheStoredList(t *testing.T) {
	bobs := relay.Relay{URL: testrelay.Start(t)}
	bob := newCitizen(t, bobs)
	dead := relay.Relay{URL: "ws://127.0.0.1:1"}
	alice := newCitizen(t, dead)

	// Alice holds Bob's list in her store, as after an earlier lookup.
	list, err := mail.RelayList(context.Background(), bob.key, []string{bobs.URL}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.node.Store.Save(list); err != nil {
		t.Fatal(err)
	}
	out := sendOne(t, alice, bob, "through the stored list")
	if out.Inbox != mail.InboxStored {
		t.Errorf("a send whose lookup got no answer gave %q, want %q", out.Inbox, mail.InboxStored)
	}
	if got := bob.sync(t, bobs); got.Received != 1 {
		t.Fatalf("bob received %d messages through the stored list, want 1", got.Received)
	}
}

// Bob's acknowledgement waits on his relay while he cannot find Alice's
// list. When her list appears, Bob's next Forward sends the
// acknowledgement to her relay, so her outbox shows the message
// delivered (Cato's retest of v0.37.0, 2026-10-19: both outboxes stayed
// "pending" after the mail arrived).
func TestAWaitingAcknowledgementGoesToARelayListThatAppearsLater(t *testing.T) {
	shared := relay.Relay{URL: testrelay.Start(t)}
	bobs := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, shared), newCitizen(t, bobs)
	ctx := context.Background()

	publishList(t, bob, shared, nostr.Now(), bobs.URL)
	sendOne(t, alice, bob, "please acknowledge")
	if got := bob.sync(t, bobs); got.Received != 1 {
		t.Fatalf("bob received %d messages, want 1", got.Received)
	}
	if got := alice.sync(t, shared); got.Delivered != 0 {
		t.Fatalf("alice saw %d deliveries before bob could find her list", got.Delivered)
	}

	// Alice's list appears where Bob looks, as after her relay add --index.
	publishList(t, alice, bobs, nostr.Now(), shared.URL)
	if sent := testutil.Must(bob.mail.Forward(ctx)); sent != 1 {
		t.Errorf("bob's Forward sent %d messages, want his acknowledgement", sent)
	}
	if got := alice.sync(t, shared); got.Delivered != 1 {
		t.Fatalf("alice saw %d deliveries after bob's Forward, want 1", got.Delivered)
	}
	if again := testutil.Must(bob.mail.Forward(ctx)); again != 0 {
		t.Errorf("a second Forward sent %d messages to relays that have them", again)
	}
}
