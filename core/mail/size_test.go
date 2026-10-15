package mail_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/internal/testutil"
)

// A sender refuses mail over the limit before it seals anything, so the
// outbox stays empty. Mail at the limit arrives.
func TestMailOverTheLimitIsRefusedBeforeItIsQueued(t *testing.T) {
	alice, bob := newCitizen(t), newCitizen(t)
	ctx := context.Background()
	big := strings.Repeat("x", private.MaxMailContent+1)

	if _, err := alice.mail.Send(ctx, []nostr.PubKey{bob.key.Public}, big); !errors.Is(err, private.ErrTooLarge) {
		t.Errorf("a message of 24 KiB + 1: %v", err)
	}
	if _, err := alice.mail.Request(ctx, bob.key.Public, call.Request{Capability: "echo", Body: big}); !errors.Is(err, private.ErrTooLarge) {
		t.Errorf("a deferred call of 24 KiB + 1: %v", err)
	}
	if out := testutil.Must(alice.mail.Outbox(ctx)); len(out) != 0 {
		t.Errorf("the outbox holds %d items", len(out))
	}

	full := strings.Repeat("x", private.MaxMailContent)
	alice.send(t, bob, full)
	courier := stick(t)
	alice.sync(t, courier)
	bob.sync(t, courier)
	if got := inboxText(bob); len(got) != 1 || got[0] != full {
		t.Errorf("bob did not get the message of 24 KiB")
	}
}

// A provider whose reply is larger than mail sends an error to the caller in
// place of the reply.
func TestADeferredReplyOverTheLimitArrivesAsAnError(t *testing.T) {
	alice, bob := newCitizen(t), newCitizen(t)
	ctx := context.Background()
	if _, err := alice.mail.Request(ctx, bob.key.Public, call.Request{Capability: "echo", Body: "big"}); err != nil {
		t.Fatal(err)
	}
	bob.mail.OnRequest = func(context.Context, nostr.Event) (call.Reply, error) {
		return call.Reply{Body: strings.Repeat("x", private.MaxMailContent+1)}, nil
	}
	courier := stick(t)
	alice.sync(t, courier)
	if report := bob.sync(t, courier); report.Answered != 1 {
		t.Fatalf("answered %d, want 1", report.Answered)
	}
	alice.sync(t, courier)
	out := testutil.Must(alice.mail.Outbox(ctx))
	if len(out) != 1 || out[0].Reply.Err != mail.TooLargeReply || out[0].Reply.Body != "" {
		t.Fatalf("outbox %+v", out)
	}
}
