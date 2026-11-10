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

// filter builds the filter of a query of sealed kinds.
func (r *run) filter(q *manifest.Query) (nostr.Filter, error) {
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
	filter := r.selfFilter([]nostr.Kind{draft.Kind}, nostr.TagMap{"k": kinds})
	if q.History {
		if d == "" {
			return nostr.Filter{}, errors.New("history needs the d tag of one draft")
		}
		return r.selfFilter([]nostr.Kind{draft.CheckpointKind}, nostr.TagMap{"a": {draft.Coordinate(me, d)}}), nil
	}
	if d != "" {
		filter.Tags["d"] = []string{d}
	}
	for _, bound := range []struct {
		template manifest.Number
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

func (r *run) limitOf(q *manifest.Query) (int, error) {
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
func (r *run) openAll(q *manifest.Query, events []nostr.Event) []*entry {
	out := []*entry{}
	for _, event := range events {
		opened, err := r.opener().Open(r.ctx, event)
		if err != nil {
			fmt.Fprintf(r.stdio.Err, "left out %s: %s\n", event.ID.Hex()[:8], terminal.EscapeLine(err.Error())) //arc:raw-ok an event ID in hex
			continue
		}
		// Core keeps its own drafts under d tags that start with arc-.
		if opened.Deleted || strings.HasPrefix(opened.D, "arc-") {
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

func (r *run) querySealed(q *manifest.Query) ([]*entry, error) {
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
		deletions := r.selfFilter([]nostr.Kind{nostr.KindDeletion}, nostr.TagMap{"a": filter.Tags["a"]})
		if _, err := r.fetchSelf(deletions); err != nil {
			return nil, err
		}
	}
	events, err := r.fetchSelf(filter)
	if err != nil {
		return nil, err
	}
	out := r.openAll(q, events)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// plainEntry makes the record of a signed event, or of a rumor.
func plainEntry(event nostr.Event, name string) *entry {
	tags := map[string]any{}
	for _, tag := range event.Tags {
		if len(tag) >= 2 {
			if _, seen := tags[tag[0]]; !seen {
				tags[tag[0]] = tag[1]
			}
		}
	}
	return &entry{rec: Record{
		"id": event.ID.Hex(), "kind": name, "author": event.PubKey.Hex(),
		"created": int64(event.CreatedAt), "tags": tags, "content": event.Content,
	}}
}

// plainAll makes the records of events that a query found.
func (r *run) plainAll(q *manifest.Query, events []nostr.Event) []*entry {
	out := []*entry{}
	for _, event := range events {
		if name := r.kindName(q.Kinds, event.Kind); name != "" {
			out = append(out, plainEntry(event, name))
		}
	}
	return out
}

// authors reads the authors of a query: nil means any.
func (r *run) authors(q *manifest.Query) ([]nostr.PubKey, error) {
	switch q.Authors {
	case "", "any":
		return nil, nil
	case "me":
		return []nostr.PubKey{r.env.Me()}, nil
	case "author":
		return []nostr.PubKey{r.in.Author}, nil
	}
	text, err := r.template(q.Authors)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	pk, err := nostr.PubKeyFromHex(text)
	if err != nil {
		return nil, fmt.Errorf("the authors of a query must be a key, not %q", text)
	}
	return []nostr.PubKey{pk}, nil
}

// plainFilter builds the filter of a query of public or group kinds, and
// names the relays to ask.
func (r *run) plainFilter(q *manifest.Query, visibility string) (nostr.Filter, []string, error) {
	filter := nostr.Filter{Tags: nostr.TagMap{}}
	for _, name := range q.Kinds {
		filter.Kinds = append(filter.Kinds, nostr.Kind(r.kindOf(name).Kind))
	}
	var err error
	if filter.Authors, err = r.authors(q); err != nil {
		return filter, nil, err
	}
	for name, tmpl := range q.Tags {
		value, err := r.template(tmpl)
		if err != nil {
			return filter, nil, err
		}
		if value != "" {
			filter.Tags[name] = []string{value}
		}
	}
	if d, err := r.template(q.D); err != nil {
		return filter, nil, err
	} else if d != "" {
		filter.Tags["d"] = []string{d}
	}
	if ids, err := r.template(q.IDs); err != nil {
		return filter, nil, err
	} else if ids != "" {
		for word := range strings.FieldsSeq(ids) {
			id, err := nostr.IDFromHex(word)
			if err != nil {
				return filter, nil, fmt.Errorf("%q is not an event ID", word)
			}
			filter.IDs = append(filter.IDs, id)
		}
	}
	limit, err := r.limitOf(q)
	if err != nil {
		return filter, nil, err
	}
	filter.Limit = limit
	var relays []string
	if visibility == "group" {
		filter.Tags["h"] = []string{r.in.Manifest.Group.ID}
		relays = []string{r.in.Manifest.Group.Relay}
	}
	return filter, relays, nil
}

func (r *run) query(q *manifest.Query) ([]*entry, error) {
	visibility, err := r.visibility(q.Kinds)
	if err != nil {
		return nil, err
	}
	switch visibility {
	case "sealed":
		return r.querySealed(q)
	case "private":
		return r.queryPrivate(q)
	}
	filter, relays, err := r.plainFilter(q, visibility)
	if err != nil {
		return nil, err
	}
	events, err := r.env.Fetch(r.ctx, filter, relays)
	if err != nil {
		return nil, err
	}
	out := r.plainAll(q, events)
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// queryPrivate reads the private events that the citizen received and sent.
func (r *run) queryPrivate(q *manifest.Query) ([]*entry, error) {
	var kinds []nostr.Kind
	for _, name := range q.Kinds {
		kinds = append(kinds, nostr.Kind(r.kindOf(name).Kind))
	}
	authors, err := r.authors(q)
	if err != nil {
		return nil, err
	}
	rumors, err := r.env.Private(r.ctx, kinds)
	if err != nil {
		return nil, err
	}
	tags := map[string]string{}
	for name, tmpl := range q.Tags {
		if tags[name], err = r.template(tmpl); err != nil {
			return nil, err
		}
	}
	out := []*entry{}
	for _, rumor := range rumors {
		if authors != nil && !slices.Contains(authors, rumor.PubKey) {
			continue
		}
		match := true
		for name, value := range tags {
			if value != "" && !rumor.Tags.ContainsAny(name, []string{value}) {
				match = false
			}
		}
		if match {
			out = append(out, plainEntry(rumor, r.kindName(q.Kinds, rumor.Kind)))
		}
	}
	return out, nil
}
