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

// sealing is one publish of a sealed draft, as it is built.
type sealing struct {
	p       *manifest.Publish
	kind    manifest.Kind
	d       string
	content string
	tags    nostr.Tags
	// isEntry is an entry. An entry has a d tag of its own, and is never
	// revised: there is no old version to read, and no checkpoint to keep.
	isEntry bool
	old     *draft.Draft
	now     nostr.Timestamp
	// head is the text of the draft. keep are the IDs of the parts that an
	// append keeps, with their line counts in keptLines, and rest are the
	// texts of the new parts.
	head      string
	keep      []string
	keptLines []int
	rest      []string
	// k seals; events are the new parts, and ids name each part.
	k      draft.Sealer
	events []nostr.Event
	ids    []string
}

func (r *run) publishSealed(p *manifest.Publish) ([]*entry, error) {
	s, err := r.startSealing(p)
	if err != nil {
		return nil, err
	}
	if err := r.partition(s); err != nil {
		return nil, err
	}
	if err := r.sealParts(s); err != nil {
		return nil, err
	}

	inner := nostr.Event{Kind: nostr.Kind(s.kind.Kind), CreatedAt: s.now, Tags: s.tags, Content: s.head, PubKey: r.env.Me()}
	var body []byte
	if name := manifest.BodyArg(p); name != "" {
		file, ok := r.values[name].(args.File)
		if !ok {
			return nil, fmt.Errorf("the body needs the file %s", name)
		}
		body = file.Bytes
	}
	if r.flags.DryRun {
		preview := map[string]any{"draft": s.d, "sealed": true, "parts": len(s.rest), "event": unsignedOf(inner)}
		if body != nil {
			preview["body"] = len(body)
		}
		return nil, r.dryRun(preview)
	}
	wrap, saved, err := r.wrapSealed(s, inner, body)
	if err != nil {
		return nil, err
	}
	if err := s.checkSize(inner, wrap, saved); err != nil {
		return nil, err
	}
	if err := r.env.Publish(r.ctx, append(append(s.events, wrap), saved...), nil); err != nil {
		return nil, r.failedWrite(err, p.Kind, s.d, inner, s.old, append(s.events, saved...))
	}

	opened, err := r.opener().Open(r.ctx, wrap)
	if err != nil {
		return nil, err
	}
	return []*entry{entryOf(opened, p.Kind)}, nil
}

// startSealing renders the d tag, the content and the tags, and reads the
// version that the publish revises.
func (r *run) startSealing(p *manifest.Publish) (*sealing, error) {
	s := &sealing{p: p, kind: r.kindOf(p.Kind)}
	var err error
	s.d, err = r.template(p.D)
	if err != nil {
		return nil, err
	}
	if err := checkD(r.env.Me(), s.d); err != nil {
		return nil, err
	}

	s.content, err = r.content(p.Content)
	if err != nil {
		return nil, err
	}
	s.tags, err = r.tagsOf(p)
	if err != nil {
		return nil, err
	}

	s.isEntry = p.Revise == "entry"
	if !s.isEntry {
		if s.old, err = r.current(s.d); err != nil {
			return nil, err
		}
	}
	s.now = after(s.old, nostr.Timestamp(r.env.Now().Unix()))
	if p.At != 0 {
		s.now = p.At
	}
	return s, nil
}

// partition splits the text into the head and the parts. An append keeps
// the parts of the old version and changes only the last one.
func (r *run) partition(s *sealing) error {
	p, old := s.p, s.old
	s.head = s.content
	var err error
	if p.Revise == "append" {
		if old != nil && !old.Deleted {
			s.tags = mergeTags(old.Event.Tags, s.tags)
			if parts := old.Parts(); len(parts) > 0 {
				// Only the last part changes: it takes the new text, and
				// splits when it grows past one part.
				last, err := r.partTexts(parts[len(parts)-1:])
				if err != nil {
					return err
				}
				s.head, s.keep = old.Event.Content, parts[:len(parts)-1]
				if counts := lineCounts(old.Event.Tags, len(parts)); counts != nil {
					s.keptLines = counts[1 : len(counts)-1]
				} else {
					s.keptLines = nil
				}
				s.rest = draft.Split(last[0] + s.content)
			} else {
				s.head = old.Event.Content + s.content
			}
		}
	}
	// An entry, and a head that moves to its address, keep their text as
	// it is.
	if !s.isEntry {
		if s.head, err = r.notebookHeader(p, s.head, old); err != nil {
			return err
		}
	}
	s.tags, err = r.enforceFrontmatter(p, s.head, s.tags)
	if err != nil {
		return err
	}
	// An updated timestamp can grow when it gains fractional seconds. If the
	// first piece no longer fits, repartition the complete append without losing
	// the previously retained parts. The usual append still changes only its end.
	if s.rest != nil && len(s.head) > draft.PartSize {
		retained, err := r.partTexts(s.keep)
		if err != nil {
			return err
		}
		s.head += strings.Join(retained, "") + strings.Join(s.rest, "")
		s.keep, s.keptLines, s.rest = nil, nil, nil
	}
	if s.rest == nil {
		pieces := draft.Split(s.head)
		s.head, s.rest = pieces[0], pieces[1:]
	}
	if len(s.keep)+len(s.rest) > draft.MaxParts {
		return fmt.Errorf("the content needs more than %d parts of %d bytes", draft.MaxParts, draft.PartSize)
	}
	return nil
}

// sealParts seals each new part, and names the parts and their lines in
// the tags. A dry run seals nothing.
func (r *run) sealParts(s *sealing) error {
	var err error
	if !r.flags.DryRun {
		if s.k, err = r.env.Sealer(r.ctx, r.in.Manifest.ID); err != nil {
			return err
		}
	}
	s.ids = slices.Clone(s.keep)
	if r.flags.DryRun {
		// Show the event without the parts, which would need signing.
		s.rest, s.ids = nil, nil
	}
	for _, text := range s.rest {
		part, err := s.k.Part(r.ctx, s.d, text, s.now)
		if err != nil {
			return err
		}
		s.events = append(s.events, part)
		s.ids = append(s.ids, part.ID.Hex())
	}
	s.tags = slices.DeleteFunc(s.tags, func(t nostr.Tag) bool { return t[0] == "parts" || t[0] == "lines" })
	if len(s.ids) > 0 {
		s.tags = append(s.tags, append(nostr.Tag{"parts"}, s.ids...))
		counts := []string{strconv.Itoa(strings.Count(s.head, "\n"))}
		if len(s.keptLines) == len(s.keep) {
			for _, n := range s.keptLines {
				counts = append(counts, strconv.Itoa(n))
			}
			for _, text := range s.rest {
				counts = append(counts, strconv.Itoa(strings.Count(text, "\n")))
			}
			s.tags = append(s.tags, append(nostr.Tag{"lines"}, counts...))
		}
	}
	return nil
}

// wrapSealed wraps the draft, with its body when it has one, and makes the
// checkpoint of a draft that is not an entry.
func (r *run) wrapSealed(s *sealing, inner nostr.Event, body []byte) (nostr.Event, []nostr.Event, error) {
	var wrap nostr.Event
	var err error
	if body != nil {
		// The body goes to the disk first: a wrap never names a body that
		// this machine lacks.
		sealed, ref, err := s.k.SealBody(body)
		if err != nil {
			return wrap, nil, err
		}
		if err := r.env.KeepBody(ref.Sum, sealed); err != nil {
			return wrap, nil, err
		}
		wrap, err = s.k.WrapBody(r.ctx, s.d, inner, ref, s.now)
		if err != nil {
			return wrap, nil, err
		}
	} else if wrap, err = s.k.Wrap(r.ctx, s.d, inner, s.now); err != nil {
		return wrap, nil, err
	}
	var saved []nostr.Event
	if !s.isEntry {
		checkpoint, err := s.k.Checkpoint(r.ctx, s.d, inner, s.now)
		if err != nil {
			return wrap, nil, err
		}
		saved = append(saved, checkpoint)
	}
	return wrap, saved, nil
}

// checkSize refuses a draft that the store would refuse. The store saves
// the parts first. A draft that it then refuses would leave parts that
// nothing names, so check its size before any save.
func (s *sealing) checkSize(inner, wrap nostr.Event, saved []nostr.Event) error {
	size := len(wrap.Content)
	for _, event := range saved {
		size = max(size, len(event.Content))
	}
	if size <= draft.MaxContent {
		return nil
	}
	total := len(s.head) + len(s.keep)*draft.PartSize
	for _, text := range s.rest {
		total += len(text)
	}
	// NIP-44 pads what it seals in steps, so the sealed size names a
	// step: give the size of the draft too, which grows with the page
	// (#398).
	plain, err := json.Marshal(inner)
	if err != nil {
		return err
	}
	return fmt.Errorf("the page is %.1f MB, too large to keep: its draft names %d parts in %d bytes, which are %d bytes once sealed and padded; the store keeps at most %d bytes in one event; split it into pages of at most about 3 MB",
		float64(total)/1e6, len(s.ids), len(plain), size, draft.MaxContent)
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
