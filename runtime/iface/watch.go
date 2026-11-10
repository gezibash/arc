package iface

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/iface/template"
)

// watch shows what a query holds now, then each new version as it arrives.
func (r *run) watch(q *Query) error {
	visibility, err := r.visibility(q.Kinds)
	if err != nil {
		return err
	}
	var filter nostr.Filter
	var relays []string
	var stored []*entry
	switch visibility {
	case "sealed":
		if filter, err = r.filter(q); err != nil {
			return err
		}
		events, err := r.fetchSelf(filter)
		if err != nil {
			return err
		}
		if len(events) > 0 && q.D != "" {
			events = events[:1]
		}
		stored = r.openAll(q, events)
		slices.Reverse(stored)
		inOrder(stored)
	case "public", "group":
		if filter, relays, err = r.plainFilter(q, visibility); err != nil {
			return err
		}
		events, err := r.env.Fetch(r.ctx, filter, relays)
		if err != nil {
			return err
		}
		stored = r.plainAll(q, events)
		slices.Reverse(stored)
	default:
		return errors.New("this arc does not watch private kinds yet: run the query again, or sync")
	}
	if err := r.show(stored); err != nil {
		return err
	}

	filter.Since = nostr.Timestamp(r.env.Now().Unix())
	filter.Limit = 0
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	live, err := r.env.Watch(ctx, filter, relays)
	if err != nil {
		return err
	}
	for open := true; open; {
		received, ok := <-live
		if !ok {
			break
		}
		// A poll of the store sends its events one after another, and a tie
		// in seconds leaves their order to chance: take the events that come
		// at once, and show them in time order (#420).
		batch := []transport.Received{received}
		timer := time.NewTimer(watchBatch)
	collect:
		for {
			select {
			case more, ok := <-live:
				if !ok {
					open = false
					break collect
				}
				batch = append(batch, more)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()
		var entries []*entry
		for _, received := range batch {
			if received.Err != nil {
				return received.Err
			}
			event := received.Event
			if !filter.Matches(event) {
				continue
			}
			if visibility == "sealed" {
				entries = append(entries, r.openAll(q, []nostr.Event{event})...)
			} else {
				entries = append(entries, r.plainAll(q, []nostr.Event{event})...)
			}
		}
		if visibility == "sealed" {
			inOrder(entries)
		}
		if err := r.show(entries); err != nil {
			return err
		}
	}
	if r.ctx.Err() != nil {
		return nil
	}
	return errors.New("every relay ended the watch")
}

// watchBatch is how long a watch waits for more events after one comes.
const watchBatch = 100 * time.Millisecond

// inOrder sorts sealed records by time. The time of an entry of a notebook
// is its ms tag, and a head comes before the entries of its second.
func inOrder(entries []*entry) {
	key := func(e *entry) (int64, int) {
		ms := int64(number(e.rec["created"])) * 1000
		tags, _ := e.rec["tags"].(map[string]any)
		if v, err := strconv.ParseInt(template.Str(tags["ms"]), 10, 64); err == nil && v/1000 == ms/1000 {
			return v, 1
		}
		return ms, 0
	}
	slices.SortStableFunc(entries, func(a, b *entry) int {
		ka, pa := key(a)
		kb, pb := key(b)
		if ka != kb {
			return cmp.Compare(ka, kb)
		}
		return pa - pb
	})
}
