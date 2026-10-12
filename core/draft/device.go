package draft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
)

// A device writes self data in the format of a data root, so that it needs
// no signer. The device key signs each wrap, checkpoint, part and deletion.
// A p tag names the citizen, and a gen tag names the generation of the data
// root. The content is the NIP-44 payload of the event inside, with the
// content key of the app under that generation as the conversation key.
// The event inside keeps the citizen as its author. docs/interface/SPEC.md,
// section 3.9, defines the format.

// Sealer writes self data of one app under one generation.
type Sealer struct {
	// Citizen names the owner of the data in the p tag.
	Citizen nostr.PubKey
	// Signer is the device key, or the citizen key on a machine that holds
	// it.
	Signer nostr.Keyer
	// Gen is the generation of the data root.
	Gen uint32
	// Key is the content key of the app under the data root of Gen.
	Key [32]byte
}

// Opener reads self data of one app, in either format.
type Opener struct {
	Citizen nostr.PubKey
	// Keyer opens the drafts that the citizen key sealed to itself. On a
	// device it asks the signer.
	Keyer nostr.Keyer
	// Key returns the content key of the app under the data root of a
	// generation.
	Key func(ctx context.Context, gen uint32) ([32]byte, error)
}

// Owner is the citizen that an event of self data belongs to: the p tag of
// a draft, checkpoint, part or deletion of them, or else its author.
func Owner(event nostr.Event) nostr.PubKey {
	if !Self(event) {
		return event.PubKey
	}
	if tag := event.Tags.Find("p"); tag != nil {
		if pk, err := nostr.PubKeyFromHex(tag[1]); err == nil {
			return pk
		}
	}
	return event.PubKey
}

// Self says whether an event is self data: a draft, a checkpoint, a part,
// or a deletion of them.
func Self(event nostr.Event) bool {
	switch event.Kind {
	case Kind, CheckpointKind, PartKind:
		return true
	case nostr.KindDeletion:
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "k" && slices.Contains(selfKinds, tag[1]) {
				return true
			}
		}
	}
	return false
}

var selfKinds = []string{strconv.Itoa(int(Kind)), strconv.Itoa(int(CheckpointKind)), strconv.Itoa(int(PartKind))}

// Gen returns the generation that an event of self data names. It returns
// false for a draft that the citizen key sealed to itself.
func Gen(event nostr.Event) (uint32, bool) {
	tag := event.Tags.Find("gen")
	if tag == nil {
		return 0, false
	}
	n, err := strconv.ParseUint(tag[1], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

func (s Sealer) tags(first ...nostr.Tag) nostr.Tags {
	return append(first, nostr.Tag{"p", s.Citizen.Hex()}, nostr.Tag{"gen", strconv.FormatUint(uint64(s.Gen), 10)}, protected)
}

func (s Sealer) seal(inner *nostr.Event) (string, error) {
	inner.PubKey = s.Citizen
	if inner.Tags == nil {
		inner.Tags = nostr.Tags{}
	}
	inner.ID = inner.GetID()
	body, err := json.Marshal(unsigned{
		ID: inner.ID.Hex(), PubKey: s.Citizen.Hex(), CreatedAt: inner.CreatedAt,
		Kind: inner.Kind, Tags: inner.Tags, Content: inner.Content,
	})
	if err != nil {
		return "", err
	}
	return nip44.Encrypt(string(body), s.Key)
}

// Wrap seals an event into a draft wrap with the d tag d.
func (s Sealer) Wrap(ctx context.Context, d string, inner nostr.Event, at nostr.Timestamp) (nostr.Event, error) {
	if d == "" {
		return nostr.Event{}, errors.New("draft: a wrap needs a d tag")
	}
	content, err := s.seal(&inner)
	if err != nil {
		return nostr.Event{}, err
	}
	return sign(ctx, s.Signer, nostr.Event{
		Kind: Kind, CreatedAt: at, Content: content,
		Tags: s.tags(nostr.Tag{"d", d}, nostr.Tag{"k", strconv.Itoa(int(inner.Kind))}),
	})
}

// Blank makes the wrap that says a draft is deleted.
func (s Sealer) Blank(ctx context.Context, d string, kind nostr.Kind, at nostr.Timestamp) (nostr.Event, error) {
	return sign(ctx, s.Signer, nostr.Event{
		Kind: Kind, CreatedAt: at,
		Tags: s.tags(nostr.Tag{"d", d}, nostr.Tag{"k", strconv.Itoa(int(kind))}),
	})
}

// Checkpoint keeps one version of the draft with the d tag d.
func (s Sealer) Checkpoint(ctx context.Context, d string, inner nostr.Event, at nostr.Timestamp) (nostr.Event, error) {
	content, err := s.seal(&inner)
	if err != nil {
		return nostr.Event{}, err
	}
	return sign(ctx, s.Signer, nostr.Event{
		Kind: CheckpointKind, CreatedAt: at, Content: content,
		Tags: s.tags(nostr.Tag{"a", Coordinate(s.Citizen, d)}),
	})
}

// Part seals one part of the draft with the d tag d.
func (s Sealer) Part(ctx context.Context, d, text string, at nostr.Timestamp) (nostr.Event, error) {
	content, err := nip44.Encrypt(text, s.Key)
	if err != nil {
		return nostr.Event{}, err
	}
	return sign(ctx, s.Signer, nostr.Event{
		Kind: PartKind, CreatedAt: at, Content: content,
		Tags: s.tags(nostr.Tag{"a", Coordinate(s.Citizen, d)}),
	})
}

// Deletion signs a NIP-09 request with these tags, and names the citizen.
func (s Sealer) Deletion(ctx context.Context, tags nostr.Tags, at nostr.Timestamp) (nostr.Event, error) {
	return sign(ctx, s.Signer, nostr.Event{
		Kind: nostr.KindDeletion, CreatedAt: at,
		Tags: append(tags, nostr.Tag{"p", s.Citizen.Hex()}),
	})
}

// Open reads a wrap or a checkpoint of the citizen, in either format. It
// does not check that the signer may write for the citizen: the store does
// that before it keeps the event.
func (o Opener) Open(ctx context.Context, wrap nostr.Event) (Draft, error) {
	gen, ok := Gen(wrap)
	if !ok {
		return Open(ctx, o.Keyer, wrap)
	}
	if Owner(wrap) != o.Citizen {
		return Draft{}, errors.New("draft: the draft names another citizen")
	}
	out := Draft{Wrap: wrap}
	switch wrap.Kind {
	case Kind:
		out.D = wrap.Tags.GetD()
	case CheckpointKind:
		a := wrap.Tags.Find("a")
		prefix := fmt.Sprintf("%d:%s:", Kind, o.Citizen.Hex())
		if a == nil || !strings.HasPrefix(a[1], prefix) {
			return Draft{}, errors.New("draft: the checkpoint names no draft of the citizen")
		}
		out.D = strings.TrimPrefix(a[1], prefix)
	default:
		return Draft{}, fmt.Errorf("draft: kind %d is not a draft", wrap.Kind)
	}
	if wrap.Content == "" {
		out.Deleted = true
		return out, nil
	}
	key, err := o.Key(ctx, gen)
	if err != nil {
		return Draft{}, fmt.Errorf("draft: %w", err)
	}
	plain, err := nip44.Decrypt(wrap.Content, key)
	if err != nil {
		return Draft{}, fmt.Errorf("draft: the draft does not open with generation %d: %w", gen, err)
	}
	var inner unsigned
	if err := json.Unmarshal([]byte(plain), &inner); err != nil {
		return Draft{}, fmt.Errorf("draft: the draft is not an event: %w", err)
	}
	if inner.PubKey != o.Citizen.Hex() {
		return Draft{}, errors.New("draft: the event inside names another author")
	}
	if kind := wrap.Tags.Find("k"); wrap.Kind == Kind && (kind == nil || kind[1] != strconv.Itoa(int(inner.Kind))) {
		return Draft{}, errors.New("draft: the k tag does not name the kind inside")
	}
	out.Event = nostr.Event{PubKey: o.Citizen, CreatedAt: inner.CreatedAt, Kind: inner.Kind, Tags: inner.Tags, Content: inner.Content}
	out.Event.ID = out.Event.GetID()
	return out, nil
}

// OpenPart reads one part of the citizen, in either format.
func (o Opener) OpenPart(ctx context.Context, part nostr.Event) (string, error) {
	gen, ok := Gen(part)
	if !ok {
		return OpenPart(ctx, o.Keyer, part)
	}
	if part.Kind != PartKind || Owner(part) != o.Citizen {
		return "", errors.New("draft: that is not a part of this citizen")
	}
	key, err := o.Key(ctx, gen)
	if err != nil {
		return "", fmt.Errorf("draft: %w", err)
	}
	return nip44.Decrypt(part.Content, key)
}
