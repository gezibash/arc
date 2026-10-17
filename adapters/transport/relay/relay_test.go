package relay_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/coder/websocket"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/internal/testrelay"
)

func protected(t *testing.T, k keys.Key) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: "mine", Tags: nostr.Tags{{"-"}}}
	if err := event.Sign(k.Secret); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestAProtectedEventNeedsItsAuthor(t *testing.T) {
	url := testrelay.Start(t)
	k := keys.Generate()
	event := protected(t, k)
	ctx := context.Background()

	err := relay.Relay{URL: url}.Send(ctx, event)
	if err == nil || !strings.Contains(err.Error(), "auth-required") {
		t.Fatalf("a relay took a protected event without authentication: %v", err)
	}

	stranger := relay.Relay{URL: url, Signer: keyer.NewPlainKeySigner(keys.Generate().Secret)}
	if err := stranger.Send(ctx, event); err == nil {
		t.Fatal("a relay took a protected event from another citizen")
	}

	author := relay.Relay{URL: url, Signer: keyer.NewPlainKeySigner(k.Secret)}
	if err := author.Send(ctx, event); err != nil {
		t.Fatalf("the author could not publish: %v", err)
	}
	batch, err := author.Fetch(ctx, nostr.Filter{IDs: []nostr.ID{event.ID}})
	if err != nil || len(batch.Events) != 1 {
		t.Errorf("the relay does not hold the event: %v %d", err, len(batch.Events))
	}
}

// refusingRelay is a relay that lists arc-rings, sends a challenge on each
// connection, refuses each AUTH unless accept is set, and answers each query
// with no events. It counts the AUTH messages.
func refusingRelay(t *testing.T) (url string, auths *atomic.Int32, accept *atomic.Bool) {
	t.Helper()
	auths, accept = &atomic.Int32{}, &atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/nostr+json" {
			_, _ = w.Write([]byte(`{"tags":["arc-rings"]}`))
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		send := func(message ...any) error {
			data, _ := json.Marshal(message)
			return conn.Write(ctx, websocket.MessageText, data)
		}
		if send("AUTH", "a challenge") != nil {
			return
		}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var message []json.RawMessage
			var label, id string
			if json.Unmarshal(data, &message) != nil || len(message) < 2 || json.Unmarshal(message[0], &label) != nil {
				continue
			}
			switch label {
			case "AUTH":
				auths.Add(1)
				var event nostr.Event
				_ = json.Unmarshal(message[1], &event)
				err = send("OK", event.ID.Hex(), accept.Load(), "restricted: not you")
			case "REQ":
				_ = json.Unmarshal(message[1], &id)
				err = send("EOSE", id)
			}
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), auths, accept
}

// A relay that rings counts a read only after authentication
// (docs/rings/SPEC.md, section 3.3). When it refuses, each read still
// reads, and the Relay gives the error to OnAuthFailure once, also when
// later reads and a new connection fail again.
func TestARefusedAuthenticationBeforeAReadIsToldOnce(t *testing.T) {
	url, auths, _ := refusingRelay(t)
	r := relay.New(url, keyer.NewPlainKeySigner(nostr.Generate()))
	defer func() { _ = r.Close() }()
	var told []error
	r.OnAuthFailure(func(err error) { told = append(told, err) })

	mail := nostr.Filter{Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {nostr.Generate().Public().Hex()}}}
	for i := range 3 {
		if i == 2 {
			_ = r.Close()
		}
		if _, err := r.Fetch(context.Background(), mail); err != nil {
			t.Fatalf("read %d failed: %v", i+1, err)
		}
	}
	if n := auths.Load(); n != 3 {
		t.Fatalf("the relay got %d AUTH messages, want 3: one for each read", n)
	}
	if len(told) != 1 || !strings.Contains(told[0].Error(), "restricted: not you") {
		t.Fatalf("OnAuthFailure got %v, want the one refusal of the relay", told)
	}
}

// In a long command, such as arc serve, a relay can refuse, take a later
// authentication, and refuse again. Each streak of refusals is told once.
func TestARefusalAfterAnAuthenticationThatWorkedIsToldAgain(t *testing.T) {
	url, _, accept := refusingRelay(t)
	r := relay.New(url, keyer.NewPlainKeySigner(nostr.Generate()))
	defer func() { _ = r.Close() }()
	var told []error
	r.OnAuthFailure(func(err error) { told = append(told, err) })

	mail := nostr.Filter{Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {nostr.Generate().Public().Hex()}}}
	// Refused twice, taken, then refused twice: each read on a new
	// connection, so each authenticates.
	for _, takes := range []bool{false, false, true, false, false} {
		accept.Store(takes)
		if _, err := r.Fetch(context.Background(), mail); err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
	}
	if len(told) != 2 {
		t.Fatalf("OnAuthFailure got %d errors, want 2: one for each streak of refusals", len(told))
	}
}
