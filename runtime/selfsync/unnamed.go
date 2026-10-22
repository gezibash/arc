package selfsync

import (
	"context"
	"slices"
	"strconv"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/gezibash/arc/core/draft"
)

// unnamedKinds are the k tags of a deletion of parts and checkpoints that
// no page names. It names each event, and not the coordinate of a draft.
var unnamedKinds = []string{strconv.Itoa(int(draft.CheckpointKind)), strconv.Itoa(int(draft.PartKind))}

// unnamed deletes the parts and checkpoints that no page names, as a
// write that stopped half way leaves them (docs/storage/SPEC.md, section
// 4.7). It deletes an event only when all of these hold:
//
//   - this device signed it. In a sync, a part of another device can
//     arrive before its draft.
//   - the storage citizens hold its blob at the start of the sync, so it
//     is older than one sync, and a write that still runs keeps its parts.
//   - no page names it. A checkpoint is unnamed when its address has no
//     draft, or a blank one. A part is unnamed when no draft and no
//     checkpoint that stays names it in a parts tag, after each one is
//     opened. If one of them does not open, each event of the address
//     stays.
//
// It saves one deletion that names each such event, as journal delete
// does. The write of this sync gives it to the storage citizens, and the
// next repair frees the blobs.
func (r *run) unnamed(ctx context.Context) error {
	if r.s.Opener == nil {
		return nil
	}
	me := r.s.Device.Public()
	mine, err := r.s.Store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.CheckpointKind, draft.PartKind}, Authors: []nostr.PubKey{me}})
	if err != nil {
		return err
	}
	// The candidates of each address.
	byAddress := map[string][]nostr.Event{}
	for _, e := range mine {
		a := e.Tags.Find("a")
		if _, sealed := draft.Gen(e); !sealed || a == nil || draft.Owner(e) != r.s.Citizen {
			continue
		}
		id := r.current.BlobID(e.ID)
		if r.blobs[id] == nil || r.written[id] {
			continue
		}
		byAddress[a[1]] = append(byAddress[a[1]], e)
	}
	var gone []nostr.Event
	for coordinate, candidates := range byAddress {
		named, ok, err := r.named(ctx, coordinate)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		for _, e := range candidates {
			if !named[e.ID.Hex()] {
				gone = append(gone, e)
			}
		}
	}
	if len(gone) == 0 {
		return nil
	}
	tags := nostr.Tags{}
	for _, e := range gone {
		tags = append(tags, nostr.Tag{"e", e.ID.Hex()})
		if e.Kind == draft.PartKind {
			r.report.Repair.UnnamedParts++
		} else {
			r.report.Repair.UnnamedCheckpoints++
		}
	}
	for _, kind := range unnamedKinds {
		tags = append(tags, nostr.Tag{"k", kind})
	}
	s := draft.Sealer{Citizen: r.s.Citizen, Signer: keyer.NewPlainKeySigner(r.s.Device)}
	request, err := s.Deletion(ctx, tags, nostr.Now())
	if err != nil {
		return err
	}
	_, err = r.s.Store.Save(request)
	return err
}

// named returns the IDs of the events of one address that a page names:
// each checkpoint, when the address has a draft that is not blank, and
// each part that the draft or a checkpoint lists. It returns false when a
// draft or a checkpoint of the address does not open: then nothing of the
// address may go.
func (r *run) named(ctx context.Context, coordinate string) (map[string]bool, bool, error) {
	prefix := strconv.Itoa(int(draft.Kind)) + ":" + r.s.Citizen.Hex() + ":"
	if len(coordinate) <= len(prefix) || coordinate[:len(prefix)] != prefix {
		return nil, false, nil
	}
	d := coordinate[len(prefix):]
	wraps, err := r.s.Store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Tags: nostr.TagMap{"d": {d}}})
	if err != nil {
		return nil, false, err
	}
	wraps = slices.DeleteFunc(wraps, func(e nostr.Event) bool { return draft.Owner(e) != r.s.Citizen })
	named := map[string]bool{}
	live := slices.IndexFunc(wraps, func(e nostr.Event) bool { return e.Content != "" })
	if live < 0 {
		// No draft, or a blank one: no page names the checkpoints and parts
		// of the address.
		return named, true, nil
	}
	wrap := wraps[live]
	k := wrap.Tags.Find("k")
	if _, sealed := draft.Gen(wrap); !sealed || k == nil {
		return nil, false, nil
	}
	opener, ok := r.s.Opener(k[1])
	if !ok {
		return nil, false, nil
	}
	checkpoints, err := r.s.Store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.CheckpointKind}, Tags: nostr.TagMap{"a": {coordinate}}})
	if err != nil {
		return nil, false, err
	}
	for _, e := range append(checkpoints, wrap) {
		if draft.Owner(e) != r.s.Citizen {
			continue
		}
		if _, sealed := draft.Gen(e); !sealed {
			return nil, false, nil
		}
		opened, err := opener.Open(ctx, e)
		if err != nil {
			return nil, false, nil
		}
		named[e.ID.Hex()] = true
		for _, id := range opened.Parts() {
			named[id] = true
		}
	}
	return named, true, nil
}

// isUnnamed says if a deletion names parts and checkpoints that no page
// names: it has no a tag, and its k tags name only checkpoints and parts.
// A deletion of a page names the draft by its coordinate.
func isUnnamed(request nostr.Event) bool {
	if request.Kind != nostr.KindDeletion || request.Tags.Find("a") != nil || draft.IsMoved(request) {
		return false
	}
	kinds := 0
	for _, tag := range request.Tags {
		if len(tag) >= 2 && tag[0] == "k" {
			if !slices.Contains(unnamedKinds, tag[1]) {
				return false
			}
			kinds++
		}
	}
	return kinds > 0
}
