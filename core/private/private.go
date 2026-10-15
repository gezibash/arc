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
	"context"
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
	"fiatjaf.com/nostr/nip13"
	"fiatjaf.com/nostr/nip44"
	"github.com/gezibash/arc/core/keys"
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

// The size limits of a store-and-forward wrap, as docs/delivery/SPEC.md
// section 3.4 defines. Mail is one wrap and is never split into parts.
// Bigger content goes over a direct path.
const (
	// MaxMailContent is the most content that one rumor in a wrap of
	// WrapKind carries, in bytes.
	MaxMailContent = 24 << 10
	// MaxRumor is the most bytes that the JSON of such a rumor holds. The
	// JSON of a rumor with MaxMailContent bytes of plain text and its tags
	// fits. JSON escapes and long tags can make a rumor larger than its
	// content, so MaxWrap follows from this limit, not from MaxMailContent.
	// MaxRumor is a NIP-44 padding boundary: 7 chunks of 4,096 bytes. It is
	// the largest rumor whose wrap holds at most 65,535 bytes of content,
	// which the Bolt event store and common relays take.
	MaxRumor = 28 << 10
	// MaxSeal is the most bytes that the JSON of a seal holds. NIP-44 adds
	// 67 bytes to the padded rumor: a version byte, a 32-byte nonce, a
	// 2-byte length and a 32-byte MAC. Base64 gives 4 bytes for each 3.
	MaxSeal = len(sealEnvelope) + 4*((67+MaxRumor+2)/3)
	// MaxWrap is the most bytes that the JSON of a wrap of WrapKind holds,
	// in the relay form. NIP-44 pads a seal of MaxSeal bytes to 40,960
	// bytes: 5 chunks of 8,192 bytes. The wrap also has room for one tag
	// that admits it to an inbox relay: a NIP-13 nonce, or a stamp of
	// docs/stranger-filter/SPEC.md, the larger. A relay refuses a larger
	// wrap.
	MaxWrap = len(wrapEnvelope) + 4*((67+sealPadded+2)/3) + max(nonceTag, stampTag)

	sealPadded = 5 << 13
	// The tags that admit a wrap, with the comma before them. The largest
	// nonce has 20 digits. A stamp holds 354 bytes, which base64 writes in
	// 472 characters.
	nonceTag = len(`,["nonce","18446744073709551615","20"]`)
	stampTag = len(`,["stamp",""]`) + 4*354/3
	// The JSON of a seal and of a relay-form wrap without their content.
	// A created_at and an expiration have 10 digits until the year 2286.
	sealEnvelope = `{"kind":13,"id":"` + hex64 + `","pubkey":"` + hex64 + `","created_at":1234567890,"tags":[],"content":"","sig":"` + hex64 + hex64 + `"}`
	wrapEnvelope = `{"kind":1059,"id":"` + hex64 + `","pubkey":"` + hex64 + `","created_at":1234567890,"tags":[["p","` + hex64 + `"],["expiration","1234567890"]],"content":"","sig":"` + hex64 + hex64 + `"}`
	hex64        = "0000000000000000000000000000000000000000000000000000000000000000"
)

// Work is the NIP-13 difficulty, in bits, that admits a wrap without a
// stamp: from a stranger, on the recipient's device, and on an inbox relay
// that asks for it. It is a protocol constant, so each sender knows it.
const Work = 20

// ErrTooLarge says that a rumor is too large for mail.
var ErrTooLarge = errors.New("private: the content is too large for mail")

// CheckMail refuses a rumor that is too large for a wrap of WrapKind. The
// error names the direct path, which carries bigger content.
func CheckMail(rumor nostr.Event) error {
	if n := len(rumor.Content); n > MaxMailContent {
		return fmt.Errorf("%w: it holds %d bytes, and mail holds at most %d; send it over a direct path, such as the transfer app", ErrTooLarge, n, MaxMailContent)
	}
	rumor.Sig = [64]byte{}
	if n := len(rumor.String()); n > MaxRumor {
		return fmt.Errorf("%w: its event holds %d bytes as JSON, and mail holds at most %d; send it over a direct path, such as the transfer app", ErrTooLarge, n, MaxRumor)
	}
	return nil
}

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
func Rumor(author keys.Signer, kind nostr.Kind, content string, tags nostr.Tags, now time.Time) nostr.Event {
	rumor := nostr.Event{
		Kind:      kind,
		PubKey:    author.PublicKey(),
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
func Seal(ctx context.Context, author keys.Signer, recipient nostr.PubKey, rumor nostr.Event) (nostr.Event, error) {
	if rumor.PubKey != author.PublicKey() {
		return nostr.Event{}, errors.New("private: the rumor names another author")
	}
	rumor.Sig = [64]byte{}

	content, err := author.Encrypt(ctx, rumor.String(), recipient)
	if err != nil {
		return nostr.Event{}, err
	}

	seal := nostr.Event{Kind: SealKind, CreatedAt: pastTime(), Content: content, Tags: nostr.Tags{}}
	if err := author.SignEvent(ctx, &seal); err != nil {
		return nostr.Event{}, err
	}
	return seal, nil
}

// WrapSeal encrypts a seal to the recipient under a one-time key. The wrap
// expires at the given time, and names the recipient in the given form.
func WrapSeal(seal nostr.Event, recipient nostr.PubKey, form Form, kind nostr.Kind, expires time.Time) (nostr.Event, error) {
	return wrapSeal(context.Background(), seal, recipient, form, kind, expires, 0)
}

// WrapSealWork wraps a seal as WrapSeal does, with NIP-13 proof of work of
// at least the given bits in the ID of the wrap. It searches on each CPU
// until it finds a nonce, or the context ends.
func WrapSealWork(ctx context.Context, seal nostr.Event, recipient nostr.PubKey, form Form, kind nostr.Kind, expires time.Time, bits int) (nostr.Event, error) {
	return wrapSeal(ctx, seal, recipient, form, kind, expires, bits)
}

// WorkOf is the proof of work of a wrap: the leading zero bits of its ID.
// The target that a nonce tag claims does not count.
func WorkOf(wrap nostr.Event) int { return nip13.Difficulty(wrap.ID) }

func wrapSeal(ctx context.Context, seal nostr.Event, recipient nostr.PubKey, form Form, kind nostr.Kind, expires time.Time, bits int) (nostr.Event, error) {
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
	if bits > 0 {
		wrap.PubKey = nostr.GetPublicKey(once)
		nonce, err := nip13.DoWork(ctx, wrap, bits)
		if err != nil {
			return nostr.Event{}, fmt.Errorf("private: proof of work: %w", err)
		}
		wrap.Tags = append(wrap.Tags, nonce)
	}
	if err := wrap.Sign(once); err != nil {
		return nostr.Event{}, err
	}
	return wrap, nil
}

// Wrap seals a rumor to the recipient and wraps it in one step.
func Wrap(ctx context.Context, author keys.Signer, recipient nostr.PubKey, rumor nostr.Event, form Form, kind nostr.Kind, expires time.Time) (nostr.Event, error) {
	seal, err := Seal(ctx, author, recipient, rumor)
	if err != nil {
		return nostr.Event{}, err
	}
	return WrapSeal(seal, recipient, form, kind, expires)
}

// pastTime is a created_at up to two days in the past, as NIP-17 suggests,
// so that a seal or a wrap does not reveal when it was made.
func pastTime() nostr.Timestamp {
	return nostr.Now() - nostr.Timestamp(rand.Int64N(2*24*3600)) //nolint:gosec // Timestamp jitter, not a key. The math/rand/v2 global source is ChaCha8 with a random seed.
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
func Unwrap(ctx context.Context, me keys.Signer, wrap nostr.Event) (Opened, error) {
	if wrap.Kind != WrapKind && wrap.Kind != LiveWrapKind {
		return Opened{}, fmt.Errorf("private: kind %d is not a gift wrap", wrap.Kind)
	}

	sealed, err := decrypt(ctx, me, wrap.PubKey, wrap.Content)
	if err != nil {
		return Opened{}, fmt.Errorf("private: the wrap does not open with this key: %w", err)
	}
	seal, err := OpenSealJSON(sealed)
	if err != nil {
		return Opened{}, err
	}

	rumor, err := OpenSeal(ctx, me, seal)
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

// OpenSeal opens a seal that its author sealed to this key. To open a seal
// that this key sealed to someone else, use OpenOwnSeal.
func OpenSeal(ctx context.Context, me keys.Signer, seal nostr.Event) (nostr.Event, error) {
	return openSealWith(ctx, me, seal, seal.PubKey)
}

// OpenOwnSeal opens a seal that this key wrote to a recipient.
func OpenOwnSeal(ctx context.Context, me keys.Signer, seal nostr.Event, recipient nostr.PubKey) (nostr.Event, error) {
	return openSealWith(ctx, me, seal, recipient)
}

func openSealWith(ctx context.Context, me keys.Signer, seal nostr.Event, other nostr.PubKey) (nostr.Event, error) {
	body, err := decrypt(ctx, me, other, seal.Content)
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

func decrypt(ctx context.Context, me keys.Signer, other nostr.PubKey, ciphertext string) (string, error) {
	return me.Decrypt(ctx, ciphertext, other)
}
