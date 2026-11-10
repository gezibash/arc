package iface

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/iface/manifest"
)

// Store is what the data actions need from the citizen's machine.
type Store interface {
	// Keyer signs as the citizen. On a device it asks the signer.
	Keyer() nostr.Keyer
	// Sealer writes self data of an app: the device key signs it, under
	// the newest data root that this machine holds. It needs no signer.
	Sealer(ctx context.Context, app string) (draft.Sealer, error)
	// Opener reads self data of an app. A draft of the format before the
	// data roots opens through the signer.
	Opener(app string) draft.Opener
	// Publish keeps events and sends them, in order. With no relays, it sends
	// them to the citizen's relays, and a relay that fails is reported. With
	// relays, such as the relay of a group, it sends them there, and fails
	// when none of them takes an event.
	Publish(ctx context.Context, events []nostr.Event, relays []string) error
	// Fetch returns the events that match, newest first. With no relays, it
	// asks the citizen's relays, keeps what they send, and reads the store.
	// With relays, it returns what those relays hold now, so an event that a
	// group removed does not show.
	Fetch(ctx context.Context, filter nostr.Filter, relays []string) ([]nostr.Event, error)
	// Watch passes on each new event that matches, as it arrives, from the
	// citizen's relays or from the relays named.
	Watch(ctx context.Context, filter nostr.Filter, relays []string) (<-chan transport.Received, error)
	// SendPrivate seals one private event to each recipient, and sends it
	// through the mail layer: relays, couriers and the outbox.
	SendPrivate(ctx context.Context, to []nostr.PubKey, kind nostr.Kind, content string, tags nostr.Tags) error
	// Private syncs the mail, and returns the private events of these kinds
	// that the citizen received and sent, oldest first.
	Private(ctx context.Context, kinds []nostr.Kind) ([]nostr.Event, error)
	// KeepBody keeps the encrypted body of a draft on this machine, under
	// the SHA-256 of its bytes, before the wrap that names it.
	KeepBody(sum string, data []byte) error
	// Body reads the encrypted body with the SHA-256 sum.
	Body(sum string) ([]byte, error)
}

// entry is one record, and what the pipeline needs to know about its event.
type entry struct {
	rec Record
	// parts are the IDs of the parts of a sealed event, in order.
	parts []string
	// d is the d tag of the draft that holds a sealed event.
	d string
	// lines are the line ends in the content and in each part, when the
	// event says. A range of lines then needs only some parts.
	lines []int
	// wrap is the wrap of a draft that names a body. save writes the body.
	wrap *nostr.Event
}

func (r *run) kindOf(name string) manifest.Kind { return r.in.Manifest.Kinds[name] }

// kindName finds the name of an event kind among the kinds of a query.
func (r *run) kindName(names []string, kind nostr.Kind) string {
	for _, name := range names {
		if r.kindOf(name).Kind == int(kind) {
			return name
		}
	}
	return ""
}

func (r *run) sealedOnly(name, action string) error {
	if v := r.kindOf(name).Visibility; v != "sealed" {
		return fmt.Errorf("this arc does not %s %s kinds", action, v)
	}
	return nil
}

// visibility is the one visibility of the kinds of a query.
func (r *run) visibility(names []string) (string, error) {
	v := r.kindOf(names[0]).Visibility
	for _, name := range names[1:] {
		if r.kindOf(name).Visibility != v {
			return "", errors.New("a query reads kinds of one visibility")
		}
	}
	return v, nil
}

// entryOf makes the record of an opened draft.
func entryOf(d draft.Draft, name string) *entry {
	tags := map[string]any{}
	for _, tag := range d.Event.Tags {
		if len(tag) >= 2 && tag[0] != "parts" && tag[0] != "lines" {
			if _, seen := tags[tag[0]]; !seen {
				tags[tag[0]] = tag[1]
			}
		}
	}
	e := &entry{
		rec: Record{
			"id": d.Event.ID.Hex(), "kind": name, "author": d.Event.PubKey.Hex(),
			"created": int64(d.Event.CreatedAt), "tags": tags, "content": d.Event.Content,
		},
		parts: d.Parts(),
		d:     d.D,
		lines: lineCounts(d.Event.Tags, len(d.Parts())),
	}
	if _, ok := draft.Body(d.Wrap); ok {
		wrap := d.Wrap
		e.wrap = &wrap
	}
	return e
}

// lineCounts reads the lines tag: the line ends in the content, then in each
// part. It returns nil when the tag is missing or does not fit the parts.
func lineCounts(tags nostr.Tags, parts int) []int {
	tag := tags.Find("lines")
	if tag == nil || len(tag) != parts+2 {
		return nil
	}
	out := make([]int, 0, parts+1)
	for _, v := range tag[1:] {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// current reads the newest wrap with the d tag d. It returns nil when there
// is none. The draft is deleted when the wrap is blank.
func (r *run) current(d string) (*draft.Draft, error) {
	wraps, err := r.fetchSelf(r.selfFilter([]nostr.Kind{draft.Kind}, nostr.TagMap{"d": {d}}))
	if err != nil {
		return nil, err
	}
	if len(wraps) == 0 {
		return nil, nil
	}
	opened, err := r.opener().Open(r.ctx, wraps[0])
	if err != nil {
		return nil, err
	}
	return &opened, nil
}

// selfFilter matches self data of the citizen in the format of the data
// roots: a p tag names the citizen, whichever device signed it.
func (r *run) selfFilter(kinds []nostr.Kind, tags nostr.TagMap) nostr.Filter {
	if tags == nil {
		tags = nostr.TagMap{}
	}
	tags["p"] = []string{r.env.Me().Hex()}
	return nostr.Filter{Kinds: kinds, Tags: tags}
}

// fetchSelf reads self data of the citizen in both formats: the filter of
// selfFilter, and the drafts that the citizen key signed before the data
// roots. It returns them newest first.
func (r *run) fetchSelf(filter nostr.Filter) ([]nostr.Event, error) {
	events, err := r.env.Fetch(r.ctx, filter, nil)
	if err != nil {
		return nil, err
	}
	old := filter
	old.Tags = nostr.TagMap{}
	for name, values := range filter.Tags {
		if name != "p" {
			old.Tags[name] = values
		}
	}
	old.Authors = []nostr.PubKey{r.env.Me()}
	older, err := r.env.Fetch(r.ctx, old, nil)
	if err != nil {
		return nil, err
	}
	seen := map[nostr.ID]bool{}
	for _, event := range events {
		seen[event.ID] = true
	}
	for _, event := range older {
		if !seen[event.ID] {
			events = append(events, event)
		}
	}
	slices.SortStableFunc(events, func(a, b nostr.Event) int {
		if a.CreatedAt != b.CreatedAt {
			return int(b.CreatedAt) - int(a.CreatedAt)
		}
		return strings.Compare(a.ID.Hex(), b.ID.Hex())
	})
	return events, nil
}

func (r *run) opener() draft.Opener { return r.env.Opener(r.in.Manifest.ID) }

// MaxTag is the longest tag value that a relay indexes. Checkpoints and
// parts name their draft by its coordinate, so the coordinate must fit.
const MaxTag = 100

func checkD(me nostr.PubKey, d string) error {
	if d == "" {
		return errors.New("the d tag of the draft is empty")
	}
	if strings.HasPrefix(d, "arc-") {
		return fmt.Errorf("the d tag %q starts with arc-, which core keeps for itself", d)
	}
	if n := len(draft.Coordinate(me, d)); n > MaxTag {
		return fmt.Errorf("the d tag %q is too long: relays index a coordinate of at most %d characters, and this one has %d", d, MaxTag, n)
	}
	return nil
}

// after is a time later than the wrap, when there is one. A new version of
// an addressable event must be newer than the old one to replace it.
func after(old *draft.Draft, now nostr.Timestamp) nostr.Timestamp {
	if old != nil && now <= old.Wrap.CreatedAt {
		return old.Wrap.CreatedAt + 1
	}
	return now
}
