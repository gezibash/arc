package selfsync

import (
	"slices"
	"strconv"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
)

// MaxMoved bounds the e tags of one deletion that a device sends to a
// relay. One deletion of MaxMoved tags is about 8 KiB.
const MaxMoved = 100

// RetiredKinds are the kinds that a relay no longer takes: the drafts, the
// checkpoints, the parts, and the private relay list of NIP-37 (kind
// 10013), which ARC no longer makes.
var RetiredKinds = []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind, 10013}

// Moves makes the unsigned deletions of these events, MaxMoved events in
// each (docs/storage/SPEC.md, section 4.2). Each deletion has the tag
// draft.Moved, names the events by ID, and names their kinds in k tags.
func Moves(events []nostr.Event, at nostr.Timestamp) []nostr.Event {
	var out []nostr.Event
	for chunk := range slices.Chunk(events, MaxMoved) {
		tags := nostr.Tags{draft.Moved}
		var kinds []nostr.Kind
		for _, event := range chunk {
			tags = append(tags, nostr.Tag{"e", event.ID.Hex()})
			if !slices.Contains(kinds, event.Kind) {
				kinds = append(kinds, event.Kind)
			}
		}
		slices.Sort(kinds)
		for _, k := range kinds {
			tags = append(tags, nostr.Tag{"k", strconv.Itoa(int(k))})
		}
		out = append(out, nostr.Event{Kind: nostr.KindDeletion, CreatedAt: at, Tags: tags,
			Content: "moved to a storage citizen"})
	}
	return out
}

// Movable says whether a relay can drop a draft, a checkpoint or a part of
// the citizen: the local store holds it, a newer version of it, or a
// deletion of it, and the storage citizens hold that record. kept holds the
// records that the storage citizens hold (Report.Kept). A private relay
// list is always movable: it holds no self data.
func Movable(s Store, kept map[nostr.ID]bool, event nostr.Event) (bool, error) {
	if event.Kind == 10013 {
		return true, nil
	}
	if kept[event.ID] {
		return true, nil
	}
	held := func(filter nostr.Filter, since nostr.Timestamp) (bool, error) {
		filter.Authors = []nostr.PubKey{event.PubKey}
		records, err := s.Query(filter)
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(records, func(r nostr.Event) bool { return r.CreatedAt >= since && kept[r.ID] }), nil
	}
	if ok, err := held(nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}, Tags: nostr.TagMap{"e": {event.ID.Hex()}}}, 0); ok || err != nil {
		return ok, err
	}
	if event.Kind != draft.Kind {
		return false, nil
	}
	d := event.Tags.GetD()
	if ok, err := held(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Tags: nostr.TagMap{"d": {d}}}, event.CreatedAt); ok || err != nil {
		return ok, err
	}
	return held(nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}, Tags: nostr.TagMap{"a": {draft.Coordinate(event.PubKey, d)}}}, event.CreatedAt)
}
