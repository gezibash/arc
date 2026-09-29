package relay_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/internal/testrelay"
)

// Safe retry depends on what the concrete relay actually accepted, not on a
// simulated Exchanger's error. Only proven non-submission permits failover.
func TestExchangeRetryContract(t *testing.T) {
	for _, state := range []string{"offline", "no_listener", "reply_lost"} {
		t.Run(state, func(t *testing.T) {
			url := ""
			if state == "offline" {
				url, _ = testrelay.StartDown(t)
			} else {
				url = testrelay.Start(t)
			}
			r := relay.Relay{URL: url}
			author, recipient := keys.Generate(), keys.Generate()
			event := nostr.Event{Kind: 21059, CreatedAt: nostr.Now(), Content: "request", Tags: nostr.Tags{{"p", recipient.Public.Hex()}}}
			if err := event.Sign(author.Secret); err != nil {
				t.Fatal(err)
			}
			received := make(chan nostr.Event, 1)
			if state == "reply_lost" {
				watchCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				events, err := r.Watch(watchCtx, nostr.Filter{Kinds: []nostr.Kind{21059}, Tags: nostr.TagMap{"p": {recipient.Public.Hex()}}})
				if err != nil {
					t.Fatal(err)
				}
				go func() {
					for got := range events {
						select {
						case received <- got:
						default:
						}
					}
				}()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := r.Exchange(ctx, event, nostr.Filter{Kinds: []nostr.Kind{21059}, Tags: nostr.TagMap{"p": {author.Public.Hex()}}}, func(nostr.Event) bool { return true })
			var notSubmitted *transport.NotSubmittedError
			safe := errors.As(err, &notSubmitted)
			if state == "reply_lost" {
				if err == nil || safe || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("accepted request became retryable: %v", err)
				}
				select {
				case got := <-received:
					if got.ID != event.ID {
						t.Fatal("the wrong request reached the receiver")
					}
				default:
					t.Fatal("the request never reached the receiver")
				}
			} else if !safe {
				t.Fatalf("unsubmitted request was not identified: %v", err)
			}
		})
	}
}

func TestReconcileKeepsCallerDeadline(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, fallback, err := (relay.Relay{URL: "ws" + strings.TrimPrefix(server.URL, "http")}).Reconcile(ctx, nostr.Filter{}, nil)
	select {
	case <-entered:
	default:
		t.Fatal("the information request did not reach the relay")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline became a normal fallback (ok=%v): %v", fallback, err)
	}
}
