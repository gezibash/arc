package iface

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/runtime/iface/manifest"
	"github.com/gezibash/arc/sdk/terminal"
)

// removeUnnamed deletes parts and checkpoints that no draft names, such as
// the parts of a write whose draft the store refused. The request names
// each event, and not the coordinate, so a blank draft of the address
// stays, and NIP-37 still reads the page as deleted.
func (r *run) removeUnnamed(coordinate string, events []nostr.Event) error {
	if r.flags.DryRun {
		return r.dryRun(map[string]any{"delete": coordinate, "events": len(events)})
	}
	k, err := r.env.Sealer(r.ctx, r.in.Manifest.ID)
	if err != nil {
		return err
	}
	var tags nostr.Tags
	for _, event := range events {
		tags = append(tags, nostr.Tag{"e", event.ID.Hex()})
	}
	for _, kind := range []nostr.Kind{draft.CheckpointKind, draft.PartKind} {
		tags = append(tags, nostr.Tag{"k", strconv.Itoa(int(kind))})
	}
	request, err := k.Deletion(r.ctx, tags, nostr.Now())
	if err != nil {
		return err
	}
	if err := r.env.Publish(r.ctx, []nostr.Event{request}, nil); err != nil {
		return err
	}
	r.removed(events)
	return nil
}

// removed says how many parts and checkpoints that no page names a delete
// removed, and what frees them on the storage citizens.
func (r *run) removed(events []nostr.Event) {
	fmt.Fprintf(r.stdio.Err, "removed %s that no page names, as a write that failed leaves them; the next arc sync --repair frees any that went to your storage citizens\n", counted(events))
}

// counted says how many parts and checkpoints the events are.
func counted(events []nostr.Event) string {
	parts := 0
	for _, event := range events {
		if event.Kind == draft.PartKind {
			parts++
		}
	}
	var out []string
	for _, n := range []struct {
		n   int
		one string
	}{{parts, "part"}, {len(events) - parts, "checkpoint"}} {
		switch {
		case n.n == 1:
			out = append(out, "1 "+n.one)
		case n.n > 1:
			out = append(out, strconv.Itoa(n.n)+" "+n.one+"s")
		}
	}
	return strings.Join(out, " and ")
}

// deleteCommand is the command that deletes a draft of a kind at an address,
// or "" when the manifest has no such command.
func (r *run) deleteCommand(kind, address string) string {
	for _, c := range r.in.Manifest.Commands {
		if c.Action.Delete != nil && c.Action.Delete.Kind == kind || c.Action.Notebook != nil && c.Action.Notebook.Op == "delete" && c.Action.Notebook.Kind == kind {
			return strings.Join(append(append([]string{"arc", r.in.Name}, c.Path...), address), " ")
		}
	}
	return ""
}

// failedWrite names the address of a write that failed after the store kept
// some of its parts, so the citizen knows which address to delete.
func (r *run) failedWrite(err error, kind, d string, inner nostr.Event, old *draft.Draft, tried []nostr.Event) error {
	stored, fetchErr := r.fetchSelf(r.selfFilter([]nostr.Kind{draft.CheckpointKind, draft.PartKind}, nostr.TagMap{"a": {draft.Coordinate(r.env.Me(), d)}}))
	if fetchErr != nil {
		return err
	}
	var kept []nostr.Event
	for _, event := range stored {
		if slices.ContainsFunc(tried, func(t nostr.Event) bool { return t.ID == event.ID }) {
			kept = append(kept, event)
		}
	}
	if len(kept) == 0 {
		return err
	}
	address := d
	if tag := inner.Tags.Find("d"); tag != nil {
		address = tag[1]
	}
	hint := ""
	if command := r.deleteCommand(kind, address); command != "" {
		hint = "; " + command + " removes them"
		if old != nil && !old.Deleted {
			hint += " with the page"
		}
	}
	return fmt.Errorf("%w; the write kept %s at %s that no page names%s", err, counted(kept), terminal.EscapeLine(address), hint)
}

// reportUnnamed lists, on the standard error, the addresses of a kind that
// hold parts or checkpoints but no draft, as a write that failed leaves
// them. A checkpoint gives the address. Parts alone do not, as only the
// keyed coordinate names them.
func (r *run) reportUnnamed(kind string) error {
	wraps, err := r.fetchSelf(r.selfFilter([]nostr.Kind{draft.Kind}, nil))
	if err != nil {
		return err
	}
	me := r.env.Me()
	seen := map[string]bool{}
	named := map[string]bool{}
	for _, wrap := range wraps {
		// The wraps come newest first: the first one of a d tag is current.
		d := wrap.Tags.GetD()
		if !seen[d] {
			seen[d] = true
			named[draft.Coordinate(me, d)] = wrap.Content != ""
		}
	}
	events, err := r.fetchSelf(r.selfFilter([]nostr.Kind{draft.CheckpointKind, draft.PartKind}, nil))
	if err != nil {
		return err
	}
	groups := map[string][]nostr.Event{}
	var order []string
	for _, event := range events {
		tag := event.Tags.Find("a")
		if tag == nil || named[tag[1]] {
			continue
		}
		// An event of the format before the data roots opens only through
		// the signer, and ls does not ask the signer for this.
		if _, ok := draft.Gen(event); !ok {
			continue
		}
		if groups[tag[1]] == nil {
			order = append(order, tag[1])
		}
		groups[tag[1]] = append(groups[tag[1]], event)
	}
	var lost []nostr.Event
	for _, a := range order {
		group := groups[a]
		// The events come newest first. Another app and another kind can
		// keep parts too, so a group counts only when it opens with the key
		// of this app.
		checkpoint := slices.IndexFunc(group, func(e nostr.Event) bool { return e.Kind == draft.CheckpointKind })
		if checkpoint < 0 {
			if _, err := r.opener().OpenPart(r.ctx, group[0]); err == nil {
				lost = append(lost, group...)
			}
			continue
		}
		opened, err := r.opener().Open(r.ctx, group[checkpoint])
		if err != nil || opened.Event.Kind != nostr.Kind(r.kindOf(kind).Kind) {
			continue
		}
		address := opened.Event.Tags.GetD()
		if address == "" {
			lost = append(lost, group...)
			continue
		}
		line := fmt.Sprintf("%s holds %s that no page names, as a write that failed leaves them", terminal.EscapeLine(address), counted(group))
		if command := r.deleteCommand(kind, address); command != "" {
			line += "; " + command + " removes them"
		}
		fmt.Fprintln(r.stdio.Err, line)
	}
	if len(lost) > 0 {
		fmt.Fprintf(r.stdio.Err, "%s that no page names are left by a write that was interrupted: arc cannot name their address; a repair frees them after an hour\n", counted(lost))
	}
	return nil
}

func (r *run) remove(dl *manifest.Delete) ([]*entry, error) { return r.removeKeeping(dl, nil) }

// removeKeeping deletes a draft and its history, except the events of the
// IDs in keep, such as the head of a lost page at the same address.
func (r *run) removeKeeping(dl *manifest.Delete, keep []string) ([]*entry, error) {
	if err := r.sealedOnly(dl.Kind, "delete"); err != nil {
		return nil, err
	}
	d, err := r.template(dl.D)
	if err != nil {
		return nil, err
	}
	if d == "" {
		return nil, errors.New("the d tag of the draft is empty")
	}
	me := r.env.Me()
	coordinate := draft.Coordinate(me, d)

	wraps, err := r.fetchSelf(r.selfFilter([]nostr.Kind{draft.Kind}, nostr.TagMap{"d": {d}}))
	if err != nil {
		return nil, err
	}
	history, err := r.fetchSelf(r.selfFilter([]nostr.Kind{draft.CheckpointKind, draft.PartKind}, nostr.TagMap{"a": {coordinate}}))
	if err != nil {
		return nil, err
	}
	history = slices.DeleteFunc(history, func(e nostr.Event) bool { return slices.Contains(keep, e.ID.Hex()) })
	if len(wraps) == 0 || wraps[0].Content == "" {
		if len(history) == 0 {
			return nil, fmt.Errorf("there is nothing to delete")
		}
		return nil, r.removeUnnamed(coordinate, history)
	}

	if r.flags.DryRun {
		return nil, r.dryRun(map[string]any{"delete": coordinate, "events": 1 + len(history)})
	}
	now := nostr.Now()
	if now <= wraps[0].CreatedAt {
		now = wraps[0].CreatedAt + 1
	}
	k, err := r.env.Sealer(r.ctx, r.in.Manifest.ID)
	if err != nil {
		return nil, err
	}
	blank, err := k.Blank(r.ctx, d, nostr.Kind(r.kindOf(dl.Kind).Kind), now)
	if err != nil {
		return nil, err
	}

	// The request deletes every older version of the wrap, but not the
	// blank one, which NIP-37 reads as deleted.
	tags := nostr.Tags{{"a", coordinate}, {"e", wraps[0].ID.Hex()}}
	for _, event := range history {
		tags = append(tags, nostr.Tag{"e", event.ID.Hex()})
	}
	for _, kind := range []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind} {
		tags = append(tags, nostr.Tag{"k", strconv.Itoa(int(kind))})
	}
	request, err := k.Deletion(r.ctx, tags, now-1)
	if err != nil {
		return nil, err
	}
	if err := r.env.Publish(r.ctx, []nostr.Event{blank, request}, nil); err != nil {
		return nil, err
	}
	if unnamed := r.unnamedParts(wraps[0], history); len(unnamed) > 0 {
		r.removed(unnamed)
	}
	return nil, nil
}

// unnamedParts finds the parts of an address that neither its draft nor a
// checkpoint names, such as the parts of a write whose draft the store
// refused.
func (r *run) unnamedParts(wrap nostr.Event, history []nostr.Event) []nostr.Event {
	named := map[string]bool{}
	for _, event := range append([]nostr.Event{wrap}, history...) {
		if event.Kind == draft.PartKind {
			continue
		}
		opened, err := r.opener().Open(r.ctx, event)
		if err != nil {
			// A draft that does not open can name any part: count none.
			return nil
		}
		for _, id := range opened.Parts() {
			named[id] = true
		}
	}
	var out []nostr.Event
	for _, event := range history {
		if event.Kind == draft.PartKind && !named[event.ID.Hex()] {
			out = append(out, event)
		}
	}
	return out
}
