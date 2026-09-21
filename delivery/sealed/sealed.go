// Package sealed makes a khatru relay serve sealed data only to its author,
// as NIP-37 recommends for private storage.
//
// Sealed data is a NIP-37 draft, a draft checkpoint, a part of a draft, or a
// private relay list. The content is already encrypted to its author. The
// relay also keeps the events themselves from everyone else, so nobody else
// learns how many drafts a citizen has, or when they wrote them.
//
// A query that names a sealed kind needs NIP-42 authentication, and must name
// the authenticated citizen as its only author. Every other query, a query by
// ID among them, leaves out the sealed events of other citizens.
package sealed

import (
	"context"
	"iter"
	"slices"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/delivery/draft"
)

// Kinds are the kinds that a relay serves only to their author.
var Kinds = []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind, draft.RelayListKind}

// Protect makes a relay serve sealed events only to their author. Call it
// after the relay has its event store.
func Protect(rl *khatru.Relay) {
	query := rl.QueryStored
	rl.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		if khatru.IsInternalCall(ctx) {
			return query(ctx, filter)
		}
		authed := khatru.GetAllAuthed(ctx)
		return func(yield func(nostr.Event) bool) {
			for event := range query(ctx, filter) {
				if slices.Contains(Kinds, event.Kind) && !slices.Contains(authed, event.PubKey) {
					continue
				}
				if !yield(event) {
					return
				}
			}
		}
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
		if slices.Contains(Kinds, event.Kind) && !slices.Contains(ws.AuthedPublicKeys, event.PubKey) {
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
