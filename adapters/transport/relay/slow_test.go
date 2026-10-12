package relay_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/coder/websocket"
	"github.com/gezibash/arc/adapters/transport/relay"
)

// slowAnswer is longer than the 7 seconds after which the Nostr library
// makes up an end of stored events by default.
const slowAnswer = 8 * time.Second

// slowRelay is a relay that holds one event. It answers its first query
// after slowAnswer, as a relay on a loaded machine can, and each later query
// at once.
func slowRelay(t *testing.T, held nostr.Event) string {
	t.Helper()
	var queries atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var message []json.RawMessage
			if json.Unmarshal(data, &message) != nil || len(message) < 2 || string(message[0]) != `"REQ"` {
				continue
			}
			if queries.Add(1) == 1 {
				select {
				case <-time.After(slowAnswer):
				case <-ctx.Done():
					return
				}
			}
			event, _ := json.Marshal(held)
			for _, answer := range []string{
				`["EVENT",` + string(message[1]) + `,` + string(event) + `]`,
				`["EOSE",` + string(message[1]) + `]`,
			} {
				if conn.Write(ctx, websocket.MessageText, []byte(answer)) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// A relay that answers late still holds its events. Fetch must give them,
// or an error. An empty result says that the relay holds nothing, and a
// caller then takes a provider with a current announcement for offline.
func TestFetchWaitsForASlowRelay(t *testing.T) {
	t.Parallel()
	held := note(t, "an announcement")
	url := slowRelay(t, held)

	batch, err := relay.Relay{URL: url}.Fetch(context.Background(), nostr.Filter{Kinds: []nostr.Kind{1}})
	if err != nil {
		t.Fatalf("the slow relay gave an error: %v", err)
	}
	if len(batch.Events) != 1 || batch.Events[0].ID != held.ID {
		t.Fatalf("Fetch gave %d events from a relay that answered after %s, want the 1 that it holds", len(batch.Events), slowAnswer)
	}
}
