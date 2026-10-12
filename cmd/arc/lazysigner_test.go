package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
)

// TestLazySignerRetries checks that a failed connection to the signer fails
// each use at once for signerRetry, and that a later use connects again. A
// long arc serve thus finds a signer that comes back, and one command waits
// for a missing signer only once.
func TestLazySignerRetries(t *testing.T) {
	citizen := keys.Generate()
	clock := time.Unix(1760000000, 0)
	calls := 0
	up := false
	lazy := &lazySigner{public: citizen.Public, now: func() time.Time { return clock }, connect: func() (nostr.Keyer, error) {
		calls++
		if !up {
			return nil, errors.New("the remote signer did not answer")
		}
		return citizen, nil
	}}
	ctx := context.Background()
	event := nostr.Event{Kind: 1, CreatedAt: nostr.Now()}

	for range 3 {
		err := lazy.SignEvent(ctx, &event)
		if err == nil || !strings.Contains(err.Error(), "this needs the signer of the citizen") {
			t.Fatalf("a sign with the signer down gave %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("three uses in the retry time connected %d times, want 1", calls)
	}

	up = true
	clock = clock.Add(signerRetry)
	if err := lazy.SignEvent(ctx, &event); err != nil {
		t.Fatalf("a sign after the retry time, with the signer back: %v", err)
	}
	if !event.CheckID() || event.PubKey != citizen.Public {
		t.Fatal("the event has no signature of the citizen")
	}
	if _, err := lazy.Encrypt(ctx, "text", citizen.Public); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("the uses connected %d times, want 2", calls)
	}
	if pk, _ := lazy.GetPublicKey(ctx); pk != citizen.Public {
		t.Fatal("the public key is not the citizen key")
	}
}
