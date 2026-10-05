package relay_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/internal/testrelay"
)

// giftWrap is an event that the relay takes only after authentication.
func giftWrap(t *testing.T) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: 1059, CreatedAt: nostr.Now(), Content: "sealed", Tags: nostr.Tags{{"p", nostr.Generate().Public().Hex()}}}
	if err := event.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	return event
}

func note(t *testing.T, content string) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: content, Tags: nostr.Tags{}}
	if err := event.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	return event
}

// holds says whether the relay stored an event. It asks on its own connection.
func holds(t *testing.T, url string, event nostr.Event) bool {
	t.Helper()
	batch, err := relay.Relay{URL: url}.Fetch(context.Background(), nostr.Filter{IDs: []nostr.ID{event.ID}})
	if err != nil {
		t.Fatal(err)
	}
	return len(batch.Events) == 1
}

// A session sends one event for each chunk. Each event must not pay for a
// connection and an authentication of its own.
func TestSendKeepsOneConnection(t *testing.T) {
	url, conns := testrelay.StartTracked(t)
	r := relay.New(url, keyer.NewPlainKeySigner(nostr.Generate()))
	defer func() { _ = r.Close() }()

	for range 3 {
		if err := r.Send(context.Background(), giftWrap(t)); err != nil {
			t.Fatalf("the relay did not take a gift wrap: %v", err)
		}
	}
	if got := conns.Accepted(); got != 1 {
		t.Fatalf("3 sends opened %d connections, want 1", got)
	}
}

func TestSendsAtTheSameTimeShareTheConnection(t *testing.T) {
	url, conns := testrelay.StartTracked(t)
	r := relay.New(url, keyer.NewPlainKeySigner(nostr.Generate()))
	defer func() { _ = r.Close() }()

	const senders = 8
	errs := make(chan error, senders)
	var wg sync.WaitGroup
	for range senders {
		event := giftWrap(t)
		wg.Go(func() { errs <- r.Send(context.Background(), event) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a send failed: %v", err)
		}
	}
	if got := conns.Accepted(); got != 1 {
		t.Fatalf("%d sends at the same time opened %d connections, want 1", senders, got)
	}
}

func TestSendConnectsAgainAfterTheRelayClosedTheConnection(t *testing.T) {
	url, conns := testrelay.StartTracked(t)
	r := relay.New(url, nil)
	defer func() { _ = r.Close() }()

	if err := r.Send(context.Background(), note(t, "before")); err != nil {
		t.Fatal(err)
	}
	conns.Drop()
	after := note(t, "after")
	if err := r.Send(context.Background(), after); err != nil {
		t.Fatalf("the send after the close failed: %v", err)
	}
	if !holds(t, url, after) {
		t.Fatal("the relay does not hold the event that came after the close")
	}
}

// A machine that slept leaves a connection that looks open and answers
// nothing. The send must not wait on it until its caller gives up.
func TestSendLeavesAConnectionThatDoesNotAnswer(t *testing.T) {
	defer relay.SetReuseWait(200 * time.Millisecond)()
	url, conns := testrelay.StartTracked(t)
	r := relay.New(url, nil)
	defer func() { _ = r.Close() }()

	if err := r.Send(context.Background(), note(t, "before")); err != nil {
		t.Fatal(err)
	}
	conns.Silence()
	after := note(t, "after")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Send(ctx, after); err != nil {
		t.Fatalf("the send on a silent connection failed: %v", err)
	}
	if !holds(t, url, after) {
		t.Fatal("the relay does not hold the event that came after the silence")
	}
}

// The transport contract: a caller that stopped sends nothing. A connection
// that is open already must not change that.
func TestAStoppedCallerSendsNothingOnAUsedConnection(t *testing.T) {
	url, _ := testrelay.StartTracked(t)
	r := relay.New(url, nil)
	defer func() { _ = r.Close() }()

	if err := r.Send(context.Background(), note(t, "before")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	late := note(t, "late")
	if err := r.Send(ctx, late); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
	if holds(t, url, late) {
		t.Fatal("the relay holds an event of a caller that stopped")
	}
}
