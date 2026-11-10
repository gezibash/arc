package iface

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/runtime/iface/args"
	"github.com/gezibash/arc/runtime/iface/manifest"
)

func (r *run) publishSealed(p *manifest.Publish) ([]*entry, error) {
	kind := r.kindOf(p.Kind)
	d, err := r.template(p.D)
	if err != nil {
		return nil, err
	}
	if err := checkD(r.env.Me(), d); err != nil {
		return nil, err
	}

	content, err := r.content(p.Content)
	if err != nil {
		return nil, err
	}
	tags, err := r.tagsOf(p)
	if err != nil {
		return nil, err
	}

	// An entry has a d tag of its own, and is never revised: there is no
	// old version to read, and no checkpoint to keep.
	isEntry := p.Revise == "entry"
	var old *draft.Draft
	if !isEntry {
		if old, err = r.current(d); err != nil {
			return nil, err
		}
	}
	now := after(old, nostr.Timestamp(r.env.Now().Unix()))
	if p.At != 0 {
		now = p.At
	}
	var keep []string
	var keptLines []int
	head := content
	var rest []string
	if p.Revise == "append" {
		if old != nil && !old.Deleted {
			tags = mergeTags(old.Event.Tags, tags)
			if parts := old.Parts(); len(parts) > 0 {
				// Only the last part changes: it takes the new text, and
				// splits when it grows past one part.
				last, err := r.partTexts(parts[len(parts)-1:])
				if err != nil {
					return nil, err
				}
				head, keep = old.Event.Content, parts[:len(parts)-1]
				if counts := lineCounts(old.Event.Tags, len(parts)); counts != nil {
					keptLines = counts[1 : len(counts)-1]
				} else {
					keptLines = nil
				}
				rest = draft.Split(last[0] + content)
			} else {
				head = old.Event.Content + content
			}
		}
	}
	// An entry, and a head that moves to its address, keep their text as
	// it is.
	if !isEntry {
		if head, err = r.notebookHeader(p, head, old); err != nil {
			return nil, err
		}
	}
	tags, err = r.enforceFrontmatter(p, head, tags)
	if err != nil {
		return nil, err
	}
	// An updated timestamp can grow when it gains fractional seconds. If the
	// first piece no longer fits, repartition the complete append without losing
	// the previously retained parts. The usual append still changes only its end.
	if rest != nil && len(head) > draft.PartSize {
		retained, err := r.partTexts(keep)
		if err != nil {
			return nil, err
		}
		head += strings.Join(retained, "") + strings.Join(rest, "")
		keep, keptLines, rest = nil, nil, nil
	}
	if rest == nil {
		pieces := draft.Split(head)
		head, rest = pieces[0], pieces[1:]
	}
	if len(keep)+len(rest) > draft.MaxParts {
		return nil, fmt.Errorf("the content needs more than %d parts of %d bytes", draft.MaxParts, draft.PartSize)
	}

	var k draft.Sealer
	if !r.flags.DryRun {
		if k, err = r.env.Sealer(r.ctx, r.in.Manifest.ID); err != nil {
			return nil, err
		}
	}
	var events []nostr.Event
	ids := slices.Clone(keep)
	if r.flags.DryRun {
		// Show the event without the parts, which would need signing.
		rest, ids = nil, nil
	}
	for _, text := range rest {
		part, err := k.Part(r.ctx, d, text, now)
		if err != nil {
			return nil, err
		}
		events = append(events, part)
		ids = append(ids, part.ID.Hex())
	}
	tags = slices.DeleteFunc(tags, func(t nostr.Tag) bool { return t[0] == "parts" || t[0] == "lines" })
	if len(ids) > 0 {
		tags = append(tags, append(nostr.Tag{"parts"}, ids...))
		counts := []string{strconv.Itoa(strings.Count(head, "\n"))}
		if len(keptLines) == len(keep) {
			for _, n := range keptLines {
				counts = append(counts, strconv.Itoa(n))
			}
			for _, text := range rest {
				counts = append(counts, strconv.Itoa(strings.Count(text, "\n")))
			}
			tags = append(tags, append(nostr.Tag{"lines"}, counts...))
		}
	}

	inner := nostr.Event{Kind: nostr.Kind(kind.Kind), CreatedAt: now, Tags: tags, Content: head, PubKey: r.env.Me()}
	var body []byte
	if name := manifest.BodyArg(p); name != "" {
		file, ok := r.values[name].(args.File)
		if !ok {
			return nil, fmt.Errorf("the body needs the file %s", name)
		}
		body = file.Bytes
	}
	if r.flags.DryRun {
		preview := map[string]any{"draft": d, "sealed": true, "parts": len(rest), "event": unsignedOf(inner)}
		if body != nil {
			preview["body"] = len(body)
		}
		return nil, r.dryRun(preview)
	}
	var wrap nostr.Event
	if body != nil {
		// The body goes to the disk first: a wrap never names a body that
		// this machine lacks.
		sealed, ref, err := k.SealBody(body)
		if err != nil {
			return nil, err
		}
		if err := r.env.KeepBody(ref.Sum, sealed); err != nil {
			return nil, err
		}
		wrap, err = k.WrapBody(r.ctx, d, inner, ref, now)
		if err != nil {
			return nil, err
		}
	} else if wrap, err = k.Wrap(r.ctx, d, inner, now); err != nil {
		return nil, err
	}
	var saved []nostr.Event
	if !isEntry {
		checkpoint, err := k.Checkpoint(r.ctx, d, inner, now)
		if err != nil {
			return nil, err
		}
		saved = append(saved, checkpoint)
	}
	// The store saves the parts first. A draft that it then refuses would
	// leave parts that nothing names, so check its size before any save.
	size := len(wrap.Content)
	for _, event := range saved {
		size = max(size, len(event.Content))
	}
	if size > draft.MaxContent {
		total := len(head) + len(keep)*draft.PartSize
		for _, text := range rest {
			total += len(text)
		}
		// NIP-44 pads what it seals in steps, so the sealed size names a
		// step: give the size of the draft too, which grows with the page
		// (#398).
		plain, err := json.Marshal(inner)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("the page is %.1f MB, too large to keep: its draft names %d parts in %d bytes, which are %d bytes once sealed and padded; the store keeps at most %d bytes in one event; split it into pages of at most about 3 MB",
			float64(total)/1e6, len(ids), len(plain), size, draft.MaxContent)
	}
	if err := r.env.Publish(r.ctx, append(append(events, wrap), saved...), nil); err != nil {
		return nil, r.failedWrite(err, p.Kind, d, inner, old, append(events, saved...))
	}

	opened, err := r.opener().Open(r.ctx, wrap)
	if err != nil {
		return nil, err
	}
	return []*entry{entryOf(opened, p.Kind)}, nil
}

// mergeTags keeps the old tags, and replaces each one that a new tag of the
// same name replaces.
func mergeTags(old, new nostr.Tags) nostr.Tags {
	out := slices.Clone(new)
	for _, tag := range old {
		if !slices.ContainsFunc(new, func(n nostr.Tag) bool { return n[0] == tag[0] }) {
			out = append(out, tag)
		}
	}
	return out
}

// content renders the content of a publish: text, or a JSON object whose
// strings are templates.
func (r *run) content(c manifest.Content) (string, error) {
	if c.JSON == nil {
		return r.template(c.Text)
	}
	var render func(v any) (any, error)
	render = func(v any) (any, error) {
		switch v := v.(type) {
		case string:
			return r.template(v)
		case map[string]any:
			out := map[string]any{}
			for key, item := range v {
				rendered, err := render(item)
				if err != nil {
					return nil, err
				}
				out[key] = rendered
			}
			return out, nil
		case []any:
			out := make([]any, len(v))
			for i, item := range v {
				rendered, err := render(item)
				if err != nil {
					return nil, err
				}
				out[i] = rendered
			}
			return out, nil
		}
		return v, nil
	}
	value, err := render(c.JSON)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(value)
	return string(body), err
}
