package turn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	return AuthHeaderBody(ctx, signer, method, rawURL, nil)
}

// AuthHeaderBody is AuthHeader for a request with a body. The event names
// the SHA-256 hash of the body in a payload tag, so that the signature
// covers the body too.
func AuthHeaderBody(ctx context.Context, signer Signer, method, rawURL string, body []byte) (string, error) {
	event := nostr.Event{
		Kind:      KindHTTPAuth,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"u", rawURL}, {"method", method}},
	}
	if body != nil {
		sum := sha256.Sum256(body)
		event.Tags = append(event.Tags, nostr.Tag{"payload", hex.EncodeToString(sum[:])})
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
	event, err := VerifyAuthBody(header, method, host, path, nil, now)
	return event.PubKey, err
}

// VerifyAuthBody is VerifyAuth for a request with a body, and returns the
// event. If body is not nil, the event must name its SHA-256 hash in a
// payload tag.
func VerifyAuthBody(header, method, host, path string, body []byte, now time.Time) (nostr.Event, error) {
	encoded, ok := strings.CutPrefix(header, "Nostr ")
	if !ok {
		return nostr.Event{}, errors.New("no NIP-98 authorization")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nostr.Event{}, errors.New("the authorization is not base64")
	}
	var event nostr.Event
	if err := json.Unmarshal(data, &event); err != nil {
		return nostr.Event{}, errors.New("the authorization is not an event")
	}
	if event.Kind != KindHTTPAuth {
		return nostr.Event{}, fmt.Errorf("the authorization has kind %d, not %d", event.Kind, KindHTTPAuth)
	}
	if !event.CheckID() || !event.VerifySignature() {
		return nostr.Event{}, errors.New("the authorization has a bad signature")
	}
	at := time.Unix(int64(event.CreatedAt), 0)
	if at.Before(now.Add(-AuthWindow)) || at.After(now.Add(AuthWindow)) {
		return nostr.Event{}, errors.New("the authorization is too old or too new; check the clock")
	}
	if tag := event.Tags.Find("method"); len(tag) < 2 || !strings.EqualFold(tag[1], method) {
		return nostr.Event{}, errors.New("the authorization is for another method")
	}
	tag := event.Tags.Find("u")
	if len(tag) < 2 {
		return nostr.Event{}, errors.New("the authorization names no URL")
	}
	u, err := url.Parse(tag[1])
	if err != nil || !strings.EqualFold(u.Host, host) || u.Path != path {
		return nostr.Event{}, errors.New("the authorization is for another URL")
	}
	if body != nil {
		sum := sha256.Sum256(body)
		if tag := event.Tags.Find("payload"); len(tag) < 2 || !strings.EqualFold(tag[1], hex.EncodeToString(sum[:])) {
			return nostr.Event{}, errors.New("the authorization is for another body")
		}
	}
	return event, nil
}
