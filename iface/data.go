package iface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/draft"
)

// Store is what the data actions need from the citizen's machine.
type Store interface {
	// Keyer signs, and seals to the citizen's own key.
	Keyer() nostr.Keyer
	// Publish keeps events and sends them to the citizen's relays, in order.
	Publish(ctx context.Context, events []nostr.Event) error
	// Fetch asks the transports for the events that match, keeps them, and
	// returns every stored event that matches, newest first.
	Fetch(ctx context.Context, filter nostr.Filter) ([]nostr.Event, error)
	// Watch passes on each new event that matches, as it arrives.
	Watch(ctx context.Context, filter nostr.Filter) (<-chan nostr.Event, error)
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
}

func (r *run) kindOf(name string) Kind { return r.in.Manifest.Kinds[name] }

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
	switch v := r.kindOf(name).Visibility; v {
	case "sealed":
		return nil
	case "private", "group":
		return fmt.Errorf("this arc does not %s %s kinds yet: they come in phase C of the interface", action, v)
	default:
		return fmt.Errorf("this arc does not %s %s kinds yet", action, v)
	}
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
	return &entry{
		rec: Record{
			"id": d.Event.ID.Hex(), "kind": name, "author": d.Event.PubKey.Hex(),
			"created": int64(d.Event.CreatedAt), "tags": tags, "content": d.Event.Content,
		},
		parts: d.Parts(),
		d:     d.D,
		lines: lineCounts(d.Event.Tags, len(d.Parts())),
	}
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
	me := r.env.Me()
	wraps, err := r.env.Fetch(r.ctx, nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{me}, Tags: nostr.TagMap{"d": {d}}})
	if err != nil {
		return nil, err
	}
	if len(wraps) == 0 {
		return nil, nil
	}
	opened, err := draft.Open(r.ctx, r.env.Keyer(), wraps[0])
	if err != nil {
		return nil, err
	}
	return &opened, nil
}

// MaxTag is the longest tag value that a relay indexes. Checkpoints and
// parts name their draft by its coordinate, so the coordinate must fit.
const MaxTag = 100

func checkD(me nostr.PubKey, d string) error {
	if d == "" {
		return errors.New("the d tag of the draft is empty")
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

func (r *run) publish(p *Publish) ([]*entry, error) {
	if err := r.sealedOnly(p.Kind, "publish"); err != nil {
		return nil, err
	}
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
	var tags nostr.Tags
	for _, tag := range p.Tags {
		rendered := nostr.Tag{tag[0]}
		for _, value := range tag[1:] {
			text, err := r.template(value)
			if err != nil {
				return nil, err
			}
			rendered = append(rendered, text)
		}
		if len(rendered) > 1 && rendered[1] != "" {
			tags = append(tags, rendered)
		}
	}

	old, err := r.current(d)
	if err != nil {
		return nil, err
	}
	now := after(old, nostr.Now())
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
	if rest == nil {
		pieces := draft.Split(head)
		head, rest = pieces[0], pieces[1:]
	}
	if len(keep)+len(rest) > draft.MaxParts {
		return nil, fmt.Errorf("the content needs more than %d parts of %d bytes", draft.MaxParts, draft.PartSize)
	}

	k := r.env.Keyer()
	var events []nostr.Event
	ids := slices.Clone(keep)
	for _, text := range rest {
		part, err := draft.Part(r.ctx, k, d, text, now)
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

	inner := nostr.Event{Kind: nostr.Kind(kind.Kind), CreatedAt: now, Tags: tags, Content: head}
	wrap, err := draft.Wrap(r.ctx, k, d, inner, now)
	if err != nil {
		return nil, err
	}
	checkpoint, err := draft.Checkpoint(r.ctx, k, d, inner, now)
	if err != nil {
		return nil, err
	}
	if err := r.env.Publish(r.ctx, append(events, wrap, checkpoint)); err != nil {
		return nil, err
	}

	opened, err := draft.Open(r.ctx, k, wrap)
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
func (r *run) content(c Content) (string, error) {
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

func (r *run) remove(dl *Delete) ([]*entry, error) {
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

	wraps, err := r.env.Fetch(r.ctx, nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{me}, Tags: nostr.TagMap{"d": {d}}})
	if err != nil {
		return nil, err
	}
	if len(wraps) == 0 || wraps[0].Content == "" {
		return nil, fmt.Errorf("there is nothing to delete")
	}
	history, err := r.env.Fetch(r.ctx, nostr.Filter{
		Kinds: []nostr.Kind{draft.CheckpointKind, draft.PartKind}, Authors: []nostr.PubKey{me},
		Tags: nostr.TagMap{"a": {coordinate}},
	})
	if err != nil {
		return nil, err
	}

	now := nostr.Now()
	if now <= wraps[0].CreatedAt {
		now = wraps[0].CreatedAt + 1
	}
	k := r.env.Keyer()
	blank, err := draft.Blank(r.ctx, k, d, nostr.Kind(r.kindOf(dl.Kind).Kind), now)
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
	request := nostr.Event{Kind: nostr.KindDeletion, CreatedAt: now - 1, Tags: tags}
	if err := k.SignEvent(r.ctx, &request); err != nil {
		return nil, err
	}
	if err := r.env.Publish(r.ctx, []nostr.Event{blank, request}); err != nil {
		return nil, err
	}
	return nil, nil
}

// filter builds the filter of a query of sealed kinds.
func (r *run) filter(q *Query) (nostr.Filter, error) {
	me := r.env.Me()
	switch q.Authors {
	case "", "me":
	default:
		return nostr.Filter{}, errors.New("sealed data belongs to its author, so a query of it reads authors me")
	}
	var kinds []string
	for _, name := range q.Kinds {
		if err := r.sealedOnly(name, "read"); err != nil {
			return nostr.Filter{}, err
		}
		kinds = append(kinds, strconv.Itoa(r.kindOf(name).Kind))
	}

	d, err := r.template(q.D)
	if err != nil {
		return nostr.Filter{}, err
	}
	filter := nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{me}, Tags: nostr.TagMap{"k": kinds}}
	if q.History {
		if d == "" {
			return nostr.Filter{}, errors.New("history needs the d tag of one draft")
		}
		return nostr.Filter{Kinds: []nostr.Kind{draft.CheckpointKind}, Authors: []nostr.PubKey{me},
			Tags: nostr.TagMap{"a": {draft.Coordinate(me, d)}}}, nil
	}
	if d != "" {
		filter.Tags["d"] = []string{d}
	}
	for _, bound := range []struct {
		template Number
		set      func(nostr.Timestamp)
	}{
		{q.Since, func(t nostr.Timestamp) { filter.Since = t }},
		{q.Until, func(t nostr.Timestamp) { filter.Until = t }},
	} {
		text, err := r.template(string(bound.template))
		if err != nil {
			return nostr.Filter{}, err
		}
		if text != "" {
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nostr.Filter{}, fmt.Errorf("%q is not a time", text)
			}
			bound.set(nostr.Timestamp(n))
		}
	}
	return filter, nil
}

func (r *run) limitOf(q *Query) (int, error) {
	text, err := r.template(string(q.Limit))
	if err != nil || text == "" {
		return 0, err
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a limit", text)
	}
	return n, nil
}

// open reads each draft or checkpoint that a query found. It leaves out a
// deleted draft, and a draft that it cannot open.
func (r *run) openAll(q *Query, events []nostr.Event) []*entry {
	out := []*entry{}
	for _, event := range events {
		opened, err := draft.Open(r.ctx, r.env.Keyer(), event)
		if err != nil {
			fmt.Fprintf(r.stdio.Err, "left out %s: %v\n", event.ID.Hex()[:8], err)
			continue
		}
		if opened.Deleted {
			continue
		}
		name := r.kindName(q.Kinds, opened.Event.Kind)
		if name == "" {
			continue
		}
		out = append(out, entryOf(opened, name))
	}
	if q.History {
		slices.Reverse(out)
	}
	return out
}

func (r *run) query(q *Query) ([]*entry, error) {
	filter, err := r.filter(q)
	if err != nil {
		return nil, err
	}
	limit, err := r.limitOf(q)
	if err != nil {
		return nil, err
	}
	if q.History {
		// A deletion removes checkpoints that this machine may hold. Learn
		// of it first, so the store drops them.
		deletions := nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}, Authors: filter.Authors, Tags: nostr.TagMap{"a": filter.Tags["a"]}}
		if _, err := r.env.Fetch(r.ctx, deletions); err != nil {
			return nil, err
		}
	}
	events, err := r.env.Fetch(r.ctx, filter)
	if err != nil {
		return nil, err
	}
	out := r.openAll(q, events)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// watch shows what a query holds now, then each new version as it arrives.
func (r *run) watch(q *Query) error {
	filter, err := r.filter(q)
	if err != nil {
		return err
	}
	events, err := r.env.Fetch(r.ctx, filter)
	if err != nil {
		return err
	}
	if len(events) > 0 && q.D != "" {
		events = events[:1]
	}
	stored := r.openAll(q, events)
	slices.Reverse(stored)
	if err := r.show(stored); err != nil {
		return err
	}

	filter.Since = nostr.Now()
	live, err := r.env.Watch(r.ctx, filter)
	if err != nil {
		return err
	}
	for event := range live {
		if err := r.show(r.openAll(q, []nostr.Event{event})); err != nil {
			return err
		}
	}
	if r.ctx.Err() != nil {
		return nil
	}
	return errors.New("every relay ended the watch")
}

// show runs the pipeline on entries and writes them.
func (r *run) show(entries []*entry) error {
	entries, err := r.pipeline(entries)
	if err != nil {
		return err
	}
	return r.write(entries, r.stdio.Out)
}

// partTexts opens parts by their IDs, in order. It stops at the first part
// that it cannot find, and returns the texts before it with an error.
func (r *run) partTexts(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	want := make([]nostr.ID, 0, len(ids))
	for _, id := range ids {
		parsed, err := nostr.IDFromHex(id)
		if err != nil {
			return nil, fmt.Errorf("the parts tag names %q, which is not an event", id)
		}
		want = append(want, parsed)
	}
	events, err := r.env.Fetch(r.ctx, nostr.Filter{IDs: want})
	if err != nil {
		return nil, err
	}
	byID := map[nostr.ID]nostr.Event{}
	for _, event := range events {
		byID[event.ID] = event
	}
	var out []string
	for i, id := range want {
		event, ok := byID[id]
		if !ok {
			return out, fmt.Errorf("part %d of %d is missing: sync with a relay or a directory that holds it", i+1, len(want))
		}
		text, err := draft.OpenPart(r.ctx, r.env.Keyer(), event)
		if err != nil {
			return out, fmt.Errorf("part %d of %d: %w", i+1, len(want), err)
		}
		out = append(out, text)
	}
	return out, nil
}

// joined is the whole text of an entry: its content, then its parts.
func (r *run) joined(e *entry) (string, error) {
	texts, err := r.partTexts(e.parts)
	return str(e.rec["content"]) + strings.Join(texts, ""), err
}

// joinedLines is a range of lines of an entry. When the entry counts its
// lines, it fetches only the parts that hold the range.
func (r *run) joinedLines(e *entry, lines string) (string, error) {
	if e.lines == nil {
		text, err := r.joined(e)
		return selectLines(text, lines), err
	}
	first, last := bounds(lines)
	// Piece i holds lines start[i] to start[i]+lines[i]; the last of them can
	// go on in the next piece.
	start := 1
	var picked []int
	pickedStart := 0
	for i, n := range e.lines {
		if start <= last && start+n >= first {
			if picked == nil {
				pickedStart = start
			}
			picked = append(picked, i)
		}
		start += n
	}
	var b strings.Builder
	var ids []string
	for _, i := range picked {
		if i == 0 {
			b.WriteString(str(e.rec["content"]))
		} else {
			ids = append(ids, e.parts[i-1])
		}
	}
	texts, err := r.partTexts(ids)
	b.WriteString(strings.Join(texts, ""))
	if picked == nil {
		return "", err
	}
	shift := pickedStart - 1
	return selectLines(b.String(), fmt.Sprintf("%d:%d", max(first-shift, 1), last-shift)), err
}
