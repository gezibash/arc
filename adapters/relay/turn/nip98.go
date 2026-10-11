package turn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"fiatjaf.com/nostr"
)

// KindHTTPAuth is the kind of a NIP-98 HTTP authentication event.
const KindHTTPAuth nostr.Kind = 27235

// AuthWindow is the largest difference between the time of a NIP-98 event
// and the clock of the relay.
const AuthWindow = 60 * time.Second

// Signer signs an event with the key of a citizen.
type Signer interface {
	SignEvent(ctx context.Context, event *nostr.Event) error
}

// AuthHeader makes the value of the Authorization header of a NIP-98
// request: "Nostr " and the signed event as base64 JSON.
func AuthHeader(ctx context.Context, signer Signer, method, rawURL string) (string, error) {
	event := nostr.Event{
		Kind:      KindHTTPAuth,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"u", rawURL}, {"method", method}},
	}
	if err := signer.SignEvent(ctx, &event); err != nil {
		return "", err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(data), nil
}

// VerifyAuth checks the Authorization header of a NIP-98 request and
// returns the key that signed it. host and path are those of the request.
// The scheme of the u tag is not compared, because a proxy in front of the
// relay can end TLS.
func VerifyAuth(header, method, host, path string, now time.Time) (nostr.PubKey, error) {
	encoded, ok := strings.CutPrefix(header, "Nostr ")
	if !ok {
		return nostr.PubKey{}, errors.New("no NIP-98 authorization")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nostr.PubKey{}, errors.New("the authorization is not base64")
	}
	var event nostr.Event
	if err := json.Unmarshal(data, &event); err != nil {
		return nostr.PubKey{}, errors.New("the authorization is not an event")
	}
	if event.Kind != KindHTTPAuth {
		return nostr.PubKey{}, fmt.Errorf("the authorization has kind %d, not %d", event.Kind, KindHTTPAuth)
	}
	if !event.CheckID() || !event.VerifySignature() {
		return nostr.PubKey{}, errors.New("the authorization has a bad signature")
	}
	at := time.Unix(int64(event.CreatedAt), 0)
	if at.Before(now.Add(-AuthWindow)) || at.After(now.Add(AuthWindow)) {
		return nostr.PubKey{}, errors.New("the authorization is too old or too new; check the clock")
	}
	if tag := event.Tags.Find("method"); len(tag) < 2 || !strings.EqualFold(tag[1], method) {
		return nostr.PubKey{}, errors.New("the authorization is for another method")
	}
	tag := event.Tags.Find("u")
	if len(tag) < 2 {
		return nostr.PubKey{}, errors.New("the authorization names no URL")
	}
	u, err := url.Parse(tag[1])
	if err != nil || !strings.EqualFold(u.Host, host) || u.Path != path {
		return nostr.PubKey{}, errors.New("the authorization is for another URL")
	}
	return event.PubKey, nil
}
