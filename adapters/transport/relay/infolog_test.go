package relay_test

import (
	"context"
	"iter"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
)

// A relay can send an event that does not match the filter of a read. The
// Nostr library drops it, and must not write its info line about it to the
// standard error of arc. The read runs in a child process, so that the test
// sees the real standard error.
func TestADroppedEventPrintsNothing(t *testing.T) {
	if url := os.Getenv("ARC_TEST_DROPPED_EVENT_RELAY"); url != "" {
		filter := nostr.Filter{Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": {keys.Generate().Public.Hex()}}}
		if _, err := (relay.Relay{URL: url}).Fetch(context.Background(), filter); err != nil {
			t.Fatal(err)
		}
		return
	}

	other := nostr.Event{Kind: 1059, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"p", keys.Generate().Public.Hex()}}, Content: "for another key"}
	if err := other.Sign(keys.Generate().Secret); err != nil {
		t.Fatal(err)
	}
	rl := khatru.NewRelay()
	served := make(chan struct{}, 1)
	rl.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		return func(yield func(nostr.Event) bool) {
			select {
			case served <- struct{}{}:
			default:
			}
			yield(other)
		}
	}
	server := httptest.NewServer(rl)
	t.Cleanup(server.Close)

	child := exec.Command(os.Args[0], "-test.run=^TestADroppedEventPrintsNothing$", "-test.count=1")
	child.Env = append(os.Environ(), "ARC_TEST_DROPPED_EVENT_RELAY=ws"+strings.TrimPrefix(server.URL, "http"))
	var stderr strings.Builder
	child.Stderr = &stderr
	if err := child.Run(); err != nil {
		t.Fatalf("the read failed: %v\n%s", err, stderr.String())
	}
	select {
	case <-served:
	default:
		t.Fatal("the relay served no event, so the test proves nothing")
	}
	if strings.Contains(stderr.String(), "[nl]") {
		t.Errorf("the Nostr library wrote to standard error: %s", stderr.String())
	}
}
