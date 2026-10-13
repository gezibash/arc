package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// TestLazySignerRemembersSilence checks that a signer that did not answer
// fails the next command at once, through the file silent, for signerRetry.
// After that, a command asks the signer again, and an answer removes the
// file.
func TestLazySignerRemembersSilence(t *testing.T) {
	citizen := keys.Generate()
	silent := filepath.Join(t.TempDir(), "signer-silent")
	clock := time.Unix(1760000000, 0)
	calls := 0
	up := false
	command := func() *lazySigner {
		return &lazySigner{public: citizen.Public, silent: silent, now: func() time.Time { return clock }, connect: func() (nostr.Keyer, error) {
			calls++
			if !up {
				return nil, fmt.Errorf("the remote signer did not answer in 15 seconds: %w", context.DeadlineExceeded)
			}
			return citizen, nil
		}}
	}
	ctx := context.Background()
	event := nostr.Event{Kind: 1, CreatedAt: nostr.Now()}

	first := command().SignEvent(ctx, &event)
	if first == nil || !strings.Contains(first.Error(), "this needs the signer of the citizen "+citizen.Public.Hex()) || strings.Contains(first.Error(), "deadline") {
		t.Fatalf("the first command gave %v", first)
	}
	clock = clock.Add(59 * time.Second)
	second := command().SignEvent(ctx, &event)
	if second == nil || calls != 1 {
		t.Fatalf("a command 59 seconds later gave %v after %d connections, want a refusal after 1", second, calls)
	}
	if !strings.Contains(second.Error(), "arc asks it again after "+time.Unix(1760000060, 0).Format(time.TimeOnly)) {
		t.Fatalf("the refusal does not say when arc asks again: %v", second)
	}

	up = true
	clock = clock.Add(time.Second)
	if err := command().SignEvent(ctx, &event); err != nil || calls != 2 {
		t.Fatalf("a command 60 seconds later gave %v after %d connections, want a signature after 2", err, calls)
	}
	if _, err := os.Stat(silent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an answer of the signer left the file: %v", err)
	}
}
