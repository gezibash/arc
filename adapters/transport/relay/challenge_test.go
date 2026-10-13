package relay_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/coder/websocket"
	"github.com/gezibash/arc/adapters/transport/relay"
)

// A relay asks for NIP-42 authentication before it takes a gift wrap
// (docs/delivery/SPEC.md section 6.2), and khatru sends a new challenge with
// each refusal. A refusal of a second call on the kept connection can bring a
// new challenge while the first call waits for the OK of its authentication.
// The send must still finish, with one authentication.
func TestAWrapIsSentWhenANewChallengeArrivesDuringAuthentication(t *testing.T) {
	type step struct {
		label string
		event nostr.Event
	}
	got := make(chan step, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		send := func(message ...any) {
			data, _ := json.Marshal(message)
			_ = conn.Write(ctx, websocket.MessageText, data)
		}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var message []json.RawMessage
			var label string
			var event nostr.Event
			if json.Unmarshal(data, &message) != nil || len(message) < 2 ||
				json.Unmarshal(message[0], &label) != nil || json.Unmarshal(message[1], &event) != nil {
				continue
			}
			got <- step{label, event}
			switch {
			case label == "EVENT" && len(got) == 1:
				send("OK", event.ID.Hex(), false, "auth-required: a gift wrap needs authentication")
				send("AUTH", "first")
			case label == "AUTH":
				send("AUTH", "second")
				send("OK", event.ID.Hex(), true, "")
			default:
				send("OK", event.ID.Hex(), true, "")
			}
		}
	}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")

	r := relay.New(url, keyer.NewPlainKeySigner(nostr.Generate()))
	defer func() { _ = r.Close() }()
	one := nostr.Generate()
	wrap := nostr.Event{Kind: 1059, CreatedAt: nostr.Now(), Content: "sealed",
		Tags: nostr.Tags{{"p", nostr.Generate().Public().Hex()}}}
	if err := wrap.Sign(one); err != nil {
		t.Fatal(err)
	}

	if err := r.Send(context.Background(), wrap); err != nil {
		t.Fatalf("send: %v", err)
	}
	close(got)
	var steps []string
	for s := range got {
		switch {
		case s.label == "EVENT" && s.event.ID == wrap.ID:
			steps = append(steps, "wrap")
		case s.label == "AUTH" && s.event.Tags.Find("challenge")[1] == "first":
			steps = append(steps, "auth first")
		default:
			steps = append(steps, s.label+" "+s.event.ID.Hex())
		}
	}
	if want := "wrap, auth first, wrap"; strings.Join(steps, ", ") != want {
		t.Errorf("the relay got %q, want %q", strings.Join(steps, ", "), want)
	}
}
