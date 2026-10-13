package devices

import (
	"errors"
	"fmt"
	"strconv"

	"fiatjaf.com/nostr"
)

// TokenKind is the ephemeral event with which the citizen key lets one
// device key read the sealed data of the citizen on a relay. A relay never
// keeps it.
const TokenKind nostr.Kind = 20274

// TokenLife is the longest time from the time of a token to its end. A
// revoked device reads sealed data on a relay until its token ends.
const TokenLife = 7 * 24 * 60 * 60

// TokenSkew is how far the time of a token can be ahead of the clock that
// checks it.
const TokenSkew = 10 * 60

// TokenTag is the tag in the NIP-11 document of a relay that takes tokens.
const TokenTag = "arc-delegation"

// NewToken returns the unsigned token for a device key. It ends TokenLife
// after now.
func NewToken(device nostr.PubKey, now nostr.Timestamp) nostr.Event {
	return nostr.Event{
		Kind:      TokenKind,
		CreatedAt: now,
		Tags: nostr.Tags{
			{"p", device.Hex()},
			{"expiration", strconv.FormatInt(int64(now)+TokenLife, 10)},
		},
	}
}

// TokenEnd checks the content of a token for a device key at the time now,
// and returns its end. It does not check the signature: a signer checks a
// token before it signs it.
func TokenEnd(event nostr.Event, device nostr.PubKey, now nostr.Timestamp) (nostr.Timestamp, error) {
	if event.Kind != TokenKind {
		return 0, fmt.Errorf("a token has kind %d, not %d", TokenKind, event.Kind)
	}
	var named []string
	var end nostr.Timestamp
	for _, tag := range event.Tags {
		if len(tag) < 2 {
			continue
		}
		switch tag[0] {
		case "p":
			named = append(named, tag[1])
		case "expiration":
			n, err := strconv.ParseInt(tag[1], 10, 64)
			if err != nil || end != 0 {
				return 0, errors.New("a token has one expiration")
			}
			end = nostr.Timestamp(n)
		}
	}
	if len(named) != 1 || named[0] != device.Hex() {
		return 0, errors.New("the token names another device key")
	}
	switch {
	case end == 0:
		return 0, errors.New("a token has one expiration")
	case event.CreatedAt > now+TokenSkew:
		return 0, errors.New("the token is from the future")
	case end <= now:
		return 0, errors.New("the token has ended")
	case end-event.CreatedAt > TokenLife:
		return 0, errors.New("the token lasts longer than 7 days")
	}
	return end, nil
}

// CheckToken checks a signed token for a device key at the time now. It
// returns the citizen who signed it, and the end of the token.
func CheckToken(event nostr.Event, device nostr.PubKey, now nostr.Timestamp) (nostr.PubKey, nostr.Timestamp, error) {
	if !event.CheckID() || !event.VerifySignature() {
		return nostr.PubKey{}, 0, errors.New("the token has no valid signature")
	}
	end, err := TokenEnd(event, device, now)
	if err != nil {
		return nostr.PubKey{}, 0, err
	}
	return event.PubKey, end, nil
}
