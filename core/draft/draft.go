// Package draft seals events to their own author, as NIP-37 defines.
//
// A draft wrap, kind 31234, holds one unsigned event, encrypted with NIP-44
// to the author's own key. Its d tag names it, and its k tag names the kind
// of the event inside. A new wrap with the same d tag replaces the old one,
// and a wrap with blank content says that the draft is deleted. A
// checkpoint, kind 1234, keeps one version of a draft, so the checkpoints
// are its history. A private relay list, kind 10013, names the relays that
// hold the drafts.
//
// A relay caps the size of one event. Content past the first 32 KiB of a
// draft travels in parts, kind 3275, which ARC defines. Each part is sealed
// to the author, and the event inside the draft lists its parts in order in
// a parts tag.
//
// Every wrap, checkpoint and part carries the NIP-70 tag, so a relay takes
// it only from its author.
package draft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"fiatjaf.com/nostr"
)

// The kinds and sizes of drafts.
const (
	Kind           nostr.Kind = 31234
	CheckpointKind nostr.Kind = 1234
	RelayListKind  nostr.Kind = 10013
	PartKind       nostr.Kind = 3275

	// PartSize is the most content that one event carries.
	PartSize = 32 * 1024
	// MaxParts bounds the parts of one draft.
	MaxParts = 256
)

// protected is the NIP-70 tag.
var protected = nostr.Tag{"-"}

// Draft is an opened wrap or checkpoint.
type Draft struct {
	// Event is the event inside. Its ID is computed; it has no signature.
	Event nostr.Event
	// D is the d tag of the wrap. A checkpoint gives the d tag of its wrap.
	D string
	// Deleted says that the wrap is blank.
	Deleted bool
	// Wrap is the wrap or the checkpoint itself.
	Wrap nostr.Event
}

// Parts lists the IDs of the parts of the event inside, in order.
func (d Draft) Parts() []string {
	if tag := d.Event.Tags.Find("parts"); tag != nil {
		return tag[1:]
	}
	return nil
}

// Coordinate names the wrap with the d tag d of an author.
func Coordinate(author nostr.PubKey, d string) string {
	return fmt.Sprintf("%d:%s:%s", Kind, author.Hex(), d)
}

// unsigned is an event without its signature, as NIP-37 stores it.
type unsigned struct {
	ID        string          `json:"id"`
	PubKey    string          `json:"pubkey"`
	CreatedAt nostr.Timestamp `json:"created_at"`
	Kind      nostr.Kind      `json:"kind"`
	Tags      nostr.Tags      `json:"tags"`
	Content   string          `json:"content"`
}

func seal(ctx context.Context, k nostr.Keyer, inner *nostr.Event) (string, nostr.PubKey, error) {
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return "", me, err
	}
	inner.PubKey = me
	if inner.Tags == nil {
		inner.Tags = nostr.Tags{}
	}
	inner.ID = inner.GetID()
	body, err := json.Marshal(unsigned{
		ID: inner.ID.Hex(), PubKey: me.Hex(), CreatedAt: inner.CreatedAt,
		Kind: inner.Kind, Tags: inner.Tags, Content: inner.Content,
	})
	if err != nil {
		return "", me, err
	}
	sealed, err := k.Encrypt(ctx, string(body), me)
	return sealed, me, err
}

func sign(ctx context.Context, k nostr.Keyer, event nostr.Event) (nostr.Event, error) {
	if err := k.SignEvent(ctx, &event); err != nil {
		return nostr.Event{}, err
	}
	return event, nil
}

// Wrap seals an event into a draft wrap with the d tag d. It sets the
// author of the event to the signer.
func Wrap(ctx context.Context, k nostr.Keyer, d string, inner nostr.Event, at nostr.Timestamp) (nostr.Event, error) {
	if d == "" {
		return nostr.Event{}, errors.New("draft: a wrap needs a d tag")
	}
	content, _, err := seal(ctx, k, &inner)
	if err != nil {
		return nostr.Event{}, err
	}
	return sign(ctx, k, nostr.Event{
		Kind: Kind, CreatedAt: at, Content: content,
		Tags: nostr.Tags{{"d", d}, {"k", strconv.Itoa(int(inner.Kind))}, protected},
	})
}

// Blank makes the wrap that says a draft is deleted.
func Blank(ctx context.Context, k nostr.Keyer, d string, kind nostr.Kind, at nostr.Timestamp) (nostr.Event, error) {
	return sign(ctx, k, nostr.Event{
		Kind: Kind, CreatedAt: at,
		Tags: nostr.Tags{{"d", d}, {"k", strconv.Itoa(int(kind))}, protected},
	})
}

// Checkpoint keeps one version of the draft with the d tag d.
func Checkpoint(ctx context.Context, k nostr.Keyer, d string, inner nostr.Event, at nostr.Timestamp) (nostr.Event, error) {
	content, me, err := seal(ctx, k, &inner)
	if err != nil {
		return nostr.Event{}, err
	}
	return sign(ctx, k, nostr.Event{
		Kind: CheckpointKind, CreatedAt: at, Content: content,
		Tags: nostr.Tags{{"a", Coordinate(me, d)}, protected},
	})
}

// Open reads a wrap or a checkpoint of the signer.
func Open(ctx context.Context, k nostr.Keyer, wrap nostr.Event) (Draft, error) {
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return Draft{}, err
	}
	if wrap.PubKey != me {
		return Draft{}, errors.New("draft: only its author can open a draft")
	}

	out := Draft{Wrap: wrap}
	switch wrap.Kind {
	case Kind:
		out.D = wrap.Tags.GetD()
	case CheckpointKind:
		a := wrap.Tags.Find("a")
		prefix := fmt.Sprintf("%d:%s:", Kind, me.Hex())
		if a == nil || !strings.HasPrefix(a[1], prefix) {
			return Draft{}, errors.New("draft: the checkpoint names no draft of its author")
		}
		out.D = strings.TrimPrefix(a[1], prefix)
	default:
		return Draft{}, fmt.Errorf("draft: kind %d is not a draft", wrap.Kind)
	}
	if wrap.Content == "" {
		out.Deleted = true
		return out, nil
	}

	plain, err := k.Decrypt(ctx, wrap.Content, me)
	if err != nil {
		return Draft{}, fmt.Errorf("draft: %w", err)
	}
	var inner unsigned
	if err := json.Unmarshal([]byte(plain), &inner); err != nil {
		return Draft{}, fmt.Errorf("draft: the draft is not an event: %w", err)
	}
	if inner.PubKey != me.Hex() {
		return Draft{}, errors.New("draft: the event inside names another author")
	}
	if kind := wrap.Tags.Find("k"); wrap.Kind == Kind && (kind == nil || kind[1] != strconv.Itoa(int(inner.Kind))) {
		return Draft{}, errors.New("draft: the k tag does not name the kind inside")
	}
	out.Event = nostr.Event{PubKey: me, CreatedAt: inner.CreatedAt, Kind: inner.Kind, Tags: inner.Tags, Content: inner.Content}
	out.Event.ID = out.Event.GetID()
	return out, nil
}

// Part seals one part of the draft with the d tag d.
func Part(ctx context.Context, k nostr.Keyer, d, text string, at nostr.Timestamp) (nostr.Event, error) {
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := k.Encrypt(ctx, text, me)
	if err != nil {
		return nostr.Event{}, err
	}
	return sign(ctx, k, nostr.Event{
		Kind: PartKind, CreatedAt: at, Content: content,
		Tags: nostr.Tags{{"a", Coordinate(me, d)}, protected},
	})
}

// OpenPart reads one part of the signer.
func OpenPart(ctx context.Context, k nostr.Keyer, part nostr.Event) (string, error) {
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return "", err
	}
	if part.Kind != PartKind || part.PubKey != me {
		return "", errors.New("draft: that is not a part of this citizen")
	}
	return k.Decrypt(ctx, part.Content, me)
}

// Split cuts text into pieces of at most PartSize bytes. It cuts after the
// last line end that fits, and inside a line only when the line is longer
// than a piece. It never cuts a UTF-8 character.
func Split(text string) []string {
	var out []string
	for len(text) > PartSize {
		cut := strings.LastIndexByte(text[:PartSize], '\n') + 1
		if cut == 0 {
			cut = PartSize
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if text != "" || len(out) == 0 {
		out = append(out, text)
	}
	return out
}

// RelayList makes the private relay list of the signer.
func RelayList(ctx context.Context, k nostr.Keyer, urls []string, at nostr.Timestamp) (nostr.Event, error) {
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return nostr.Event{}, err
	}
	tags := make(nostr.Tags, 0, len(urls))
	for _, url := range urls {
		tags = append(tags, nostr.Tag{"relay", url})
	}
	body, err := json.Marshal(tags)
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := k.Encrypt(ctx, string(body), me)
	if err != nil {
		return nostr.Event{}, err
	}
	return sign(ctx, k, nostr.Event{Kind: RelayListKind, CreatedAt: at, Content: content, Tags: nostr.Tags{}})
}

// ReadRelayList reads the private relay list of the signer.
func ReadRelayList(ctx context.Context, k nostr.Keyer, list nostr.Event) ([]string, error) {
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return nil, err
	}
	if list.Kind != RelayListKind || list.PubKey != me {
		return nil, errors.New("draft: that is not the relay list of this citizen")
	}
	plain, err := k.Decrypt(ctx, list.Content, me)
	if err != nil {
		return nil, err
	}
	var tags nostr.Tags
	if err := json.Unmarshal([]byte(plain), &tags); err != nil {
		return nil, err
	}
	var urls []string
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == "relay" {
			urls = append(urls, tag[1])
		}
	}
	return urls, nil
}
