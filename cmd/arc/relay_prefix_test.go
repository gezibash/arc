package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/coder/websocket"
	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
)

// The store of the relay indexes a p tag by its first 8 bytes. A read of
// the mail of bob must still get only the gift wraps whose p tag is bob's
// full key, and a key that shares those 8 bytes must not get bob's mail.
// The test reads the raw frames of the relay, because the client library
// drops an event that does not match the filter before arc sees it.
func TestRelayServesMailOnlyToTheFullKey(t *testing.T) {
	rl, _, _, closeDB, err := newRelay(filepath.Join(t.TempDir(), "relay.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	server := httptest.NewServer(rl)
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx := context.Background()

	alice, bob := keys.Generate(), keys.Generate()
	bobHex := bob.Public.Hex()
	// other shares the first 8 bytes of bob's key, then differs.
	other := bobHex[:16] + strings.Repeat("0", 48)
	if other == bobHex {
		other = bobHex[:16] + strings.Repeat("1", 48)
	}

	rumor := private.Rumor(alice, 14, "for bob", nostr.Tags{{"p", bobHex}}, time.Now())
	toBob, err := private.Wrap(ctx, alice, bob.Public, rumor, private.RelayForm, private.WrapKind, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// A key that shares a prefix with bob's is not a point that a wrap can
	// encrypt to, so this wrap only has the form of mail.
	sender := keys.Generate()
	toOther := nostr.Event{Kind: private.WrapKind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"p", other}}, Content: "for the other key"}
	if err := toOther.Sign(sender.Secret); err != nil {
		t.Fatal(err)
	}
	for _, event := range []nostr.Event{toBob, toOther} {
		if err := (relay.Relay{URL: url, Signer: sender}).Send(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	for _, tc := range []struct {
		name string
		p    string
		want nostr.ID
	}{{"bob", bobHex, toBob.ID}, {"the other key", other, toOther.ID}} {
		filter := nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {tc.p}}}
		if got := rawRead(t, conn, filter); len(got) != 1 || got[0] != tc.want {
			t.Errorf("a read of the mail of %s got %v, want only %s", tc.name, got, tc.want)
		}
		if got := rawCount(t, conn, filter); got != 1 {
			t.Errorf("a count of the mail of %s got %d, want 1", tc.name, got)
		}
	}
}

// rawRead sends a REQ on conn and returns the IDs of the events that the
// relay sends before EOSE.
func rawRead(t *testing.T, conn *websocket.Conn, filter nostr.Filter) []nostr.ID {
	t.Helper()
	var ids []nostr.ID
	for _, frame := range rawAsk(t, conn, "REQ", filter, "EOSE") {
		if frame.label != "EVENT" {
			t.Fatalf("the relay answered the read with %s: %s", frame.label, frame.body)
		}
		var event nostr.Event
		if err := json.Unmarshal(frame.body, &event); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, event.ID)
	}
	return ids
}

// rawCount sends a COUNT on conn and returns the count of the relay.
func rawCount(t *testing.T, conn *websocket.Conn, filter nostr.Filter) int {
	t.Helper()
	frames := rawAsk(t, conn, "COUNT", filter, "COUNT")
	var result struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(frames[len(frames)-1].body, &result); err != nil {
		t.Fatal(err)
	}
	return result.Count
}

type rawFrame struct {
	label string
	body  json.RawMessage
}

// rawAsk sends ["<verb>", id, filter] on conn and returns the frames for id
// up to and with the first frame labelled last. The last frame is left out
// when it is EOSE.
func rawAsk(t *testing.T, conn *websocket.Conn, verb string, filter nostr.Filter, last string) []rawFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := verb + "-" + filter.Tags["p"][0][:20]
	request, err := json.Marshal([]any{verb, id, filter})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	var frames []rawFrame
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("no %s from the relay: %v", last, err)
		}
		var parts []json.RawMessage
		if err := json.Unmarshal(data, &parts); err != nil || len(parts) < 2 {
			t.Fatalf("the relay sent %s", data)
		}
		var label, sub string
		if json.Unmarshal(parts[0], &label) != nil || json.Unmarshal(parts[1], &sub) != nil || sub != id {
			continue
		}
		frame := rawFrame{label: label}
		if len(parts) > 2 {
			frame.body = parts[2]
		}
		if label == last {
			if last != "EOSE" {
				frames = append(frames, frame)
			}
			return frames
		}
		if label == "CLOSED" {
			t.Fatalf("the relay closed the %s: %s", verb, frame.body)
		}
		frames = append(frames, frame)
	}
}
