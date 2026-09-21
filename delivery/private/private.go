// Package private wraps an event for one recipient, as NIP-59 defines.
//
// The author writes a rumor, seals it to the recipient, and wraps the seal
// under a one-time key. Only the recipient can open the wrap, and the seal
// proves who wrote the rumor.
//
// A wrap names its recipient in one of two forms. The relay form carries a p
// tag with the recipient's key, which Nostr relays and clients understand.
// The courier form carries a w tag with a route tag, which names the
// recipient for one day without revealing their key to a courier.
package private

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"github.com/gezibash/arc/delivery/keys"
)

// The kinds of a private event.
const (
	SealKind     nostr.Kind = 13
	WrapKind     nostr.Kind = 1059
	LiveWrapKind nostr.Kind = 21059
)

// MaxAge is how long a wrap lives by default, and how far back a recipient
// looks for its route tags. Mail on a USB stick can take days.
const MaxAge = 7 * 24 * time.Hour

// Form says how a wrap names its recipient.
type Form int

// The two forms of a wrap.
const (
	RelayForm Form = iota
	CourierForm
)

// RouteTag names a recipient for one UTC day. Anyone who knows the
// recipient's key can compute it; a courier that does not know the key
// cannot link it to them, or link two days to each other.
func RouteTag(recipient nostr.PubKey, day time.Time) string {
	mac := hmac.New(sha256.New, recipient[:])
	mac.Write([]byte("arc-route-v1\x00" + day.UTC().Format("2006-01-02")))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// RouteTags returns the route tags of a recipient for every day that a wrap
// can still be alive on, and for tomorrow, which covers a clock that runs
// ahead.
func RouteTags(recipient nostr.PubKey, now time.Time) []string {
	days := int(MaxAge / (24 * time.Hour))
	tags := make([]string, 0, days+2)
	for i := -days; i <= 1; i++ {
		tags = append(tags, RouteTag(recipient, now.AddDate(0, 0, i)))
	}
	return tags
}

// Rumor makes an unsigned event by the author.
func Rumor(author keys.Key, kind nostr.Kind, content string, tags nostr.Tags, now time.Time) nostr.Event {
	rumor := nostr.Event{
		Kind:      kind,
		PubKey:    author.Public,
		CreatedAt: nostr.Timestamp(now.Unix()),
		Tags:      tags,
		Content:   content,
	}
	rumor.ID = rumor.GetID()
	return rumor
}

// Seal encrypts a rumor to the recipient, and signs the result as the author.
// The author can open the seal again, because a NIP-44 conversation key is
// the same from both ends. The seal carries no tags, and its created_at moves
// up to two days into the past.
func Seal(author keys.Key, recipient nostr.PubKey, rumor nostr.Event) (nostr.Event, error) {
	if rumor.PubKey != author.Public {
		return nostr.Event{}, errors.New("private: the rumor names another author")
	}
	rumor.Sig = [64]byte{}

	conv, err := nip44.GenerateConversationKey(recipient, author.Secret)
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := nip44.Encrypt(rumor.String(), conv)
	if err != nil {
		return nostr.Event{}, err
	}

	seal := nostr.Event{Kind: SealKind, CreatedAt: pastTime(), Content: content, Tags: nostr.Tags{}}
	if err := seal.Sign(author.Secret); err != nil {
		return nostr.Event{}, err
	}
	return seal, nil
}

// WrapSeal encrypts a seal to the recipient under a one-time key. The wrap
// expires at the given time, and names the recipient in the given form.
func WrapSeal(seal nostr.Event, recipient nostr.PubKey, form Form, kind nostr.Kind, expires time.Time) (nostr.Event, error) {
	if kind != WrapKind && kind != LiveWrapKind {
		return nostr.Event{}, fmt.Errorf("private: kind %d is not a gift wrap", kind)
	}

	once := nostr.Generate()
	conv, err := nip44.GenerateConversationKey(recipient, once)
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := nip44.Encrypt(seal.String(), conv)
	if err != nil {
		return nostr.Event{}, err
	}

	route := nostr.Tag{"p", recipient.Hex()}
	if form == CourierForm {
		route = nostr.Tag{"w", RouteTag(recipient, time.Now())}
	}

	wrap := nostr.Event{
		Kind:      kind,
		CreatedAt: pastTime(),
		Tags:      nostr.Tags{route, {"expiration", strconv.FormatInt(expires.Unix(), 10)}},
		Content:   content,
	}
	if err := wrap.Sign(once); err != nil {
		return nostr.Event{}, err
	}
	return wrap, nil
}

// Wrap seals a rumor to the recipient and wraps it in one step.
func Wrap(author keys.Key, recipient nostr.PubKey, rumor nostr.Event, form Form, kind nostr.Kind, expires time.Time) (nostr.Event, error) {
	seal, err := Seal(author, recipient, rumor)
	if err != nil {
		return nostr.Event{}, err
	}
	return WrapSeal(seal, recipient, form, kind, expires)
}

// pastTime is a created_at up to two days in the past, as NIP-17 suggests,
// so that a seal or a wrap does not reveal when it was made.
func pastTime() nostr.Timestamp {
	return nostr.Now() - nostr.Timestamp(rand.Int64N(2*24*3600))
}

// Opened is a wrap that opened: the seal that proves the author, and the
// rumor inside it.
type Opened struct {
	Seal  nostr.Event
	Rumor nostr.Event
}

// Author is the citizen who wrote the rumor. The seal proves it.
func (o Opened) Author() nostr.PubKey { return o.Seal.PubKey }

// Unwrap opens a wrap for its recipient. It refuses a seal that is not signed,
// a seal with tags, and a rumor whose author differs from the seal's author.
// Without that last check, any author could claim to be another.
func Unwrap(me keys.Key, wrap nostr.Event) (Opened, error) {
	if wrap.Kind != WrapKind && wrap.Kind != LiveWrapKind {
		return Opened{}, fmt.Errorf("private: kind %d is not a gift wrap", wrap.Kind)
	}

	sealed, err := decrypt(me, wrap.PubKey, wrap.Content)
	if err != nil {
		return Opened{}, fmt.Errorf("private: the wrap does not open with this key: %w", err)
	}
	seal, err := OpenSealJSON(sealed)
	if err != nil {
		return Opened{}, err
	}

	rumor, err := OpenSeal(me, seal)
	if err != nil {
		return Opened{}, err
	}
	return Opened{Seal: seal, Rumor: rumor}, nil
}

// OpenSealJSON reads a seal and checks it.
func OpenSealJSON(body string) (nostr.Event, error) {
	var seal nostr.Event
	if err := json.Unmarshal([]byte(body), &seal); err != nil {
		return nostr.Event{}, errors.New("private: the wrap holds no seal")
	}
	if seal.Kind != SealKind {
		return nostr.Event{}, fmt.Errorf("private: the wrap holds kind %d, not a seal", seal.Kind)
	}
	if len(seal.Tags) != 0 {
		return nostr.Event{}, errors.New("private: a seal carries no tags")
	}
	if !seal.CheckID() || !seal.VerifySignature() {
		return nostr.Event{}, errors.New("private: the seal is not signed by its author")
	}
	return seal, nil
}

// OpenSeal opens a seal that this key can read: one sealed to this key, or
// one this key sealed to someone else. A NIP-44 conversation key is the same
// from both ends.
func OpenSeal(me keys.Key, seal nostr.Event) (nostr.Event, error) {
	return openSealWith(me, seal, seal.PubKey)
}

// OpenOwnSeal opens a seal that this key wrote to a recipient.
func OpenOwnSeal(me keys.Key, seal nostr.Event, recipient nostr.PubKey) (nostr.Event, error) {
	return openSealWith(me, seal, recipient)
}

func openSealWith(me keys.Key, seal nostr.Event, other nostr.PubKey) (nostr.Event, error) {
	body, err := decrypt(me, other, seal.Content)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("private: the seal does not open with this key: %w", err)
	}

	var rumor nostr.Event
	if err := json.Unmarshal([]byte(body), &rumor); err != nil {
		return nostr.Event{}, errors.New("private: the seal holds no rumor")
	}
	if rumor.PubKey != seal.PubKey {
		return nostr.Event{}, errors.New("private: the rumor names another author than the seal")
	}
	rumor.ID = rumor.GetID()
	return rumor, nil
}

func decrypt(me keys.Key, other nostr.PubKey, ciphertext string) (string, error) {
	conv, err := nip44.GenerateConversationKey(other, me.Secret)
	if err != nil {
		return "", err
	}
	return nip44.Decrypt(ciphertext, conv)
}
