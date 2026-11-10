package iface

import (
	"fmt"
	"slices"
	"strconv"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/runtime/iface/manifest"
	"github.com/gezibash/arc/sdk/terminal"
)

// deletePage deletes a page. The head goes as a delete action deletes a
// draft, with the entries of the page and their parts in one more request.
// The head of a lost page is a checkpoint, so a request names it and its
// parts, and the live page at the address stays.
func (r *run) deletePage(n *manifest.Notebook, dTemplate string) error {
	// A delete names its page: it does not resolve today.
	if _, _, err := r.resolve(n.Kind, n.Address); err != nil {
		return err
	}
	pages, err := r.notebookPages(n.Kind)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(pages, func(p notebookPage) bool { return p.address == n.Address })
	if i < 0 {
		// A lost page that moved up still answers to <address>-<head id>.
		if i = slices.IndexFunc(pages, func(p notebookPage) bool {
			return !p.lost && p.hid != "" && p.address+"-"+p.hid == n.Address
		}); i >= 0 {
			n.Address = pages[i].address
		}
	}
	var page *notebookPage
	if i >= 0 {
		page = &pages[i]
	}
	// The head of a lost page is a checkpoint at the same address, so the
	// delete of the address keeps it, and the newest one moves up to it.
	var keep []string
	var up *notebookPage
	for j := range pages {
		p := &pages[j]
		if p.lost && p.base == n.Address && p.checkpoint != (nostr.ID{}) {
			keep = append(append(keep, p.checkpoint.Hex()), p.entry.parts...)
			if up == nil || number(p.entry.rec["created"]) > number(up.entry.rec["created"]) {
				up = p
			}
		}
	}
	if page != nil && (len(page.entries) > 0 || page.lost) {
		if r.flags.DryRun {
			return r.dryRun(map[string]any{"delete": n.Address, "entries": len(page.entries)})
		}
		me := r.env.Me()
		tags := nostr.Tags{}
		at := nostr.Now()
		for _, p := range page.entries {
			tags = append(tags, nostr.Tag{"a", draft.Coordinate(me, p.e.d)}, nostr.Tag{"e", p.wrap.Hex()})
			for _, id := range p.e.parts {
				tags = append(tags, nostr.Tag{"e", id})
			}
			if p.created >= at {
				at = p.created + 1
			}
		}
		if page.lost && page.checkpoint != (nostr.ID{}) {
			tags = append(tags, nostr.Tag{"e", page.checkpoint.Hex()})
			for _, id := range page.entry.parts {
				tags = append(tags, nostr.Tag{"e", id})
			}
		}
		for _, kind := range []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind} {
			tags = append(tags, nostr.Tag{"k", strconv.Itoa(int(kind))})
		}
		k, err := r.env.Sealer(r.ctx, r.in.Manifest.ID)
		if err != nil {
			return err
		}
		request, err := k.Deletion(r.ctx, tags, at)
		if err != nil {
			return err
		}
		if err := r.env.Publish(r.ctx, []nostr.Event{request}, nil); err != nil {
			return err
		}
		if page.lost {
			return nil
		}
	}
	scoped := r.scopedWith(Values{"address": n.Address})
	d, err := scoped.template(dTemplate)
	if err != nil {
		return err
	}
	old, err := r.current(d)
	if err != nil {
		return err
	}
	if _, err := scoped.removeKeeping(&manifest.Delete{Kind: n.Kind, D: dTemplate}, keep); err != nil && (page == nil || len(page.entries) == 0 || err.Error() != "there is nothing to delete") {
		return err
	}
	if up == nil || r.flags.DryRun {
		return nil
	}
	gone := ""
	if page != nil && !page.lost {
		gone = page.title
	}
	return r.moveUp(n.Kind, d, old, up, gone)
}

// moveUp makes the head of a lost page the draft at its address again, as
// it was written, when the page there is deleted. Nothing then takes its
// checkpoint for one that a failed write left, and its entries, which name
// its head id, read at the address.
func (r *run) moveUp(kind, d string, old *draft.Draft, up *notebookPage, gone string) error {
	text, err := r.joined(up.entry)
	if err != nil {
		return err
	}
	// The blank draft of the delete is one second newer than the old draft
	// or the clock; this draft is newer again.
	at := nostr.Now()
	if old != nil && at <= old.Wrap.CreatedAt {
		at = old.Wrap.CreatedAt + 1
	}
	at++
	tags := [][]string{{"d", "{{up_address}}"}, {"title", "{{up_title}}"}, {"head", "{{up_head}}"}}
	scoped := r.scopedWith(Values{"up_text": text, "up_address": up.base, "up_title": up.title, "up_head": up.hid})
	if _, err := scoped.publishSealed(&manifest.Publish{Kind: kind, D: d, Revise: "entry", At: at, Content: manifest.Content{Text: "{{up_text}}"}, Tags: tags}); err != nil {
		return err
	}
	fmt.Fprintf(r.stdio.Err, "%s is deleted, so %s, made at %s offline too, moved up from %s to %s\n",
		titled(gone), titled(up.title), terminal.EscapeLine(up.base), terminal.EscapeLine(up.address), terminal.EscapeLine(up.base))
	return nil
}

// notebookWrite runs the operations of a kind that keeps each entry.
func (r *run) notebookWrite(n *manifest.Notebook, dTemplate string) ([]*entry, error) {
	if n.Op != "fix" {
		address, _, err := r.notebookAddress(n.Address)
		if err != nil {
			return nil, err
		}
		n.Address = address
	}
	var address string
	var err error
	switch n.Op {
	case "write":
		address, err = r.writePage(n, dTemplate)
	case "append":
		address, err = r.appendEntry(n, dTemplate)
	case "fix":
		address, err = r.fixEntry(n)
	case "history":
		return r.history(n, dTemplate)
	case "delete":
		return nil, r.deletePage(n, dTemplate)
	}
	if err != nil || r.flags.DryRun || r.command.Output.Format == "" && !r.flags.JSON {
		return nil, err
	}
	return []*entry{{rec: Record{"kind": n.Kind, "tags": map[string]any{"d": address}, "content": address}}}, nil
}

// names are the addresses of a page that a fix of its head can name: its
// address, and <address>-<head id>, which a fix made while the page was lost
// names.
func (p *notebookPage) names() []string {
	if p.lost || p.hid == "" {
		return []string{p.address}
	}
	return []string{p.address, p.address + "-" + p.hid}
}
