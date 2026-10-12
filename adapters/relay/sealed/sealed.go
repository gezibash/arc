// Package sealed makes a khatru relay serve sealed data only to its author,
// as NIP-37 recommends for private storage.
//
// Sealed data is a NIP-37 draft, a draft checkpoint, a part of a draft, a
// private relay list, a device list, the data roots, or a storage list. The
// content is already encrypted to its author. The relay also keeps the
// events themselves from everyone else, so nobody else learns how many
// drafts a citizen has, or when they wrote them. A NIP-09 deletion whose k
// tag names a sealed kind is sealed too, because its e tags count the
// drafts.
//
// A query that names a sealed kind needs NIP-42 authentication, and must name
// the authenticated citizen as its only author. Every other query, a query by
// ID among them, leaves out the sealed events of other citizens.
//
// The relay refuses new drafts, checkpoints, parts and private relay lists
// (docs/storage/SPEC.md, section 7). Self data stays on the device and goes
// to storage citizens. The relay still serves the old ones to their author,
// until the author deletes them or the operator removes them. The one draft
// that the relay still takes holds the keyed root of a citizen.
//
// The relay applies a deletion with the tag draft.Moved, and then serves it
// to nobody, also not to its author. An older client imports each deletion
// of its drafts into its store, and its store would delete them there. The
// NIP-11 document of the relay has the tag draft.RetiredTag, so that a
// device sends such a deletion only to this relay.
package sealed

import (
	"context"
	"iter"
	"slices"
	"strconv"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/sdk/storage"
)

// privateRelayList is the kind of the private relay list of NIP-37. ARC no
// longer makes it, but a relay still holds the lists of older clients.
const privateRelayList nostr.Kind = 10013

// keyedRootD is the d tag of the draft that holds the keyed root of a
// citizen, as runtime/iface.KeyedRootD names it. A device that signs
// through a bunker reads its keyed root from this draft on the relays.
const keyedRootD = "arc-keyed-root"

// Kinds are the kinds that a relay serves only to their author.
var Kinds = []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind, privateRelayList, devices.Kind, devices.RootsKind, storage.ListKind}

// Retired are the kinds that a relay no longer takes. A draft wrap with the
// d tag of the keyed root is the one exception.
var Retired = []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind, privateRelayList}

// Refusal is the reason that a relay gives for a retired kind.
const Refusal = "blocked: this relay keeps no drafts; arc keeps them on the device and syncs them with a storage citizen; update arc"

// isSealed says whether a relay serves the event only to its author.
func isSealed(event nostr.Event) bool {
	if slices.Contains(Kinds, event.Kind) {
		return true
	}
	if event.Kind != nostr.KindDeletion {
		return false
	}
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "k" {
			continue
		}
		if k, err := strconv.Atoi(tag[1]); err == nil && slices.Contains(Kinds, nostr.Kind(k)) {
			return true
		}
	}
	return false
}

// refused says whether a relay refuses a new event of a retired kind.
func refused(event nostr.Event) bool {
	if !slices.Contains(Retired, event.Kind) {
		return false
	}
	return event.Kind != draft.Kind || event.Tags.GetD() != keyedRootD
}

// Protect makes a relay serve sealed events only to their author, and
// refuse the retired kinds. Call it after the relay has its event store.
func Protect(rl *khatru.Relay) {
	rl.Info.Tags = append(rl.Info.Tags, draft.RetiredTag)
	query := rl.QueryStored
	rl.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		if khatru.IsInternalCall(ctx) {
			return query(ctx, filter)
		}
		authed := khatru.GetAllAuthed(ctx)
		return func(yield func(nostr.Event) bool) {
			for event := range query(ctx, filter) {
				if draft.IsMoved(event) || isSealed(event) && !slices.Contains(authed, event.PubKey) {
					continue
				}
				if !yield(event) {
					return
				}
			}
		}
	}

	onEvent := rl.OnEvent
	rl.OnEvent = func(ctx context.Context, event nostr.Event) (bool, string) {
		if refused(event) {
			return true, Refusal
		}
		if onEvent != nil {
			return onEvent(ctx, event)
		}
		return false, ""
	}

	onRequest := rl.OnRequest
	rl.OnRequest = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		if reject, msg := check(ctx, filter); reject {
			return reject, msg
		}
		if onRequest != nil {
			return onRequest(ctx, filter)
		}
		return false, ""
	}
	onCount := rl.OnCount
	rl.OnCount = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		if reject, msg := check(ctx, filter); reject {
			return reject, msg
		}
		if onCount != nil {
			return onCount(ctx, filter)
		}
		return false, ""
	}

	prevent := rl.PreventBroadcast
	rl.PreventBroadcast = func(ws *khatru.WebSocket, filter nostr.Filter, event nostr.Event) bool {
		if draft.IsMoved(event) || isSealed(event) && !slices.Contains(ws.AuthedPublicKeys, event.PubKey) {
			return true
		}
		return prevent != nil && prevent(ws, filter, event)
	}
}

// check refuses a query that names a sealed kind, unless the citizen who
// asks has authenticated and names only themselves as the author.
func check(ctx context.Context, filter nostr.Filter) (bool, string) {
	if !slices.ContainsFunc(filter.Kinds, func(k nostr.Kind) bool { return slices.Contains(Kinds, k) }) {
		return false, ""
	}
	authed := khatru.GetAllAuthed(ctx)
	if len(authed) == 0 {
		khatru.RequestAuth(ctx)
		return true, "auth-required: sealed data is served only to its author"
	}
	if len(filter.Authors) == 0 || slices.ContainsFunc(filter.Authors, func(pk nostr.PubKey) bool { return !slices.Contains(authed, pk) }) {
		return true, "restricted: a citizen reads only their own sealed data"
	}
	return false, ""
}
