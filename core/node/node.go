// Package node joins a store to its transports.
//
// Every event that reaches the node goes through the store first, so it is
// verified before anything else sees it. The node never trusts a transport.
package node

import (
	"context"
	"errors"
	"fmt"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
)

// EventStore is the persistence needed by a node. Read failures are never
// represented as missing events.
type EventStore interface {
	Save(nostr.Event) (store.Result, error)
	Query(nostr.Filter) ([]nostr.Event, error)
	Has(nostr.ID) (bool, error)
}

// Batcher is an EventStore that saves the events of one fetch together, with
// one sync to disk. It is only for events that a transport still holds.
type Batcher interface {
	SaveAll([]nostr.Event) ([]store.Result, error)
}

// Node is the store of one citizen on one machine.
type Node struct {
	Store EventStore
}

// Sent is the outcome of one send to one transport.
type Sent struct {
	Transport string
	Err       error
}

// Publish keeps an event, then sends it over every transport. The event
// stays in the store when a transport fails, so a later sync sends it again.
func (n *Node) Publish(ctx context.Context, event nostr.Event, transports []transport.Transport) (store.Result, []Sent, error) {
	result, err := n.Store.Save(event)
	if err != nil || result.Outcome == store.Refused {
		return result, nil, err
	}

	sent := make([]Sent, 0, len(transports))
	for _, t := range transports {
		sent = append(sent, Sent{Transport: t.Name(), Err: t.Send(ctx, event)})
	}
	return result, sent, nil
}

// Report says what one sync did.
type Report struct {
	Transport  string
	Received   int
	Duplicate  int
	Superseded int
	Refused    []string
	Unreadable int
	Sent       int
	SendFailed []error
	// Reconciled says that the sync compared sets with Negentropy, and did
	// not fetch every event.
	Reconciled bool
}

// Sync reconciles the store with one transport for one filter. It keeps each
// event that the transport holds and the store lacks, and sends each event
// that the store holds and the transport lacks.
func (n *Node) Sync(ctx context.Context, filter nostr.Filter, t transport.Transport) (Report, error) {
	report := Report{Transport: t.Name()}

	if r, ok := t.(transport.Reconciler); ok {
		local, err := n.Store.Query(filter)
		if err != nil {
			return report, err
		}
		need, give, ok, err := r.Reconcile(ctx, filter, (*store.Events)(&local))
		if err != nil {
			return report, err
		}
		if ok {
			report.Reconciled = true
			return report, n.exchange(ctx, t, filter, need, give, &report)
		}
	}

	batch, err := t.Fetch(ctx, filter)
	if err != nil {
		return report, err
	}
	report.Unreadable = batch.Unreadable

	remote := make(map[nostr.ID]bool, len(batch.Events))
	for _, event := range batch.Events {
		remote[event.ID] = true
	}
	if err := n.keepAll(batch.Events, &report); err != nil {
		return report, err
	}

	events, err := n.Store.Query(filter)
	if err != nil {
		return report, err
	}
	for _, event := range events {
		if remote[event.ID] {
			continue
		}
		if err := t.Send(ctx, event); err != nil {
			report.SendFailed = append(report.SendFailed, err)
			continue
		}
		report.Sent++
	}
	return report, nil
}

// exchange fetches the events that the store needs, and sends the events that
// the transport needs, a batch at a time. A fetch keeps the kinds and the
// authors of the sync's filter: a relay serves sealed data only to a query
// that names its kind.
func (n *Node) exchange(ctx context.Context, t transport.Transport, filter nostr.Filter, need, give []nostr.ID, report *Report) error {
	const size = 100

	for i := 0; i < len(need); i += size {
		batch, err := t.Fetch(ctx, nostr.Filter{IDs: need[i:min(i+size, len(need))], Kinds: filter.Kinds, Authors: filter.Authors})
		if err != nil {
			return err
		}
		report.Unreadable += batch.Unreadable
		if err := n.keepAll(batch.Events, report); err != nil {
			return err
		}
	}

	for i := 0; i < len(give); i += size {
		events, err := n.Store.Query(nostr.Filter{IDs: give[i:min(i+size, len(give))]})
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := t.Send(ctx, event); err != nil {
				report.SendFailed = append(report.SendFailed, err)
				continue
			}
			report.Sent++
		}
	}
	return nil
}

func (n *Node) keep(event nostr.Event, report *Report) error {
	result, err := n.Store.Save(event)
	if err != nil {
		return err
	}
	count(event, result, report)
	return nil
}

// keepAll keeps the events of one fetch. A store that can batch saves each
// hundred events with one sync to disk.
func (n *Node) keepAll(events []nostr.Event, report *Report) error {
	const size = 100

	batcher, ok := n.Store.(Batcher)
	if !ok {
		for _, event := range events {
			if err := n.keep(event, report); err != nil {
				return err
			}
		}
		return nil
	}
	for i := 0; i < len(events); i += size {
		chunk := events[i:min(i+size, len(events))]
		results, err := batcher.SaveAll(chunk)
		if err != nil {
			return err
		}
		for j, result := range results {
			count(chunk[j], result, report)
		}
	}
	return nil
}

func count(event nostr.Event, result store.Result, report *Report) {
	switch result.Outcome {
	case store.Stored:
		report.Received++
	case store.Duplicate:
		report.Duplicate++
	case store.Superseded:
		report.Superseded++
	case store.Refused:
		report.Refused = append(report.Refused, event.ID.Hex()+": "+result.Reason)
	}
}

// Pull keeps each event that the transports hold for a filter, and sends
// nothing back. A transport that fails is skipped, and its error returned
// with the others.
func (n *Node) Pull(ctx context.Context, filter nostr.Filter, transports []transport.Transport) ([]Report, []error) {
	var reports []Report
	var errs []error

	for _, t := range transports {
		report := Report{Transport: t.Name()}
		batch, err := t.Fetch(ctx, filter)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		report.Unreadable = batch.Unreadable
		if err := n.keepAll(batch.Events, &report); err != nil {
			return nil, append(errs, err)
		}
		reports = append(reports, report)
	}
	return reports, errs
}

// Unreached is the error of a pull that no transport answered, or nil. A
// pull with no transports asked nothing, and is not an error.
func Unreached(reports []Report, errs []error) error {
	if len(reports) > 0 || len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("no relay answered: %w", errors.Join(errs...))
}

// Obtain returns the events with these IDs. It reads the store first, and
// asks the transports only for the events that the store lacks. It keeps what
// the transports return, after verification. When an event is still missing
// and no transport answered, it returns their errors.
func (n *Node) Obtain(ctx context.Context, ids []nostr.ID, transports []transport.Transport) (map[nostr.ID]nostr.Event, error) {
	found := make(map[nostr.ID]nostr.Event, len(ids))
	events, err := n.Store.Query(nostr.Filter{IDs: ids})
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		found[event.ID] = event
	}

	var errs []error
	answered := false
	for _, t := range transports {
		missing := lacking(ids, found)
		if len(missing) == 0 {
			break
		}

		batch, err := t.Fetch(ctx, nostr.Filter{IDs: missing})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		answered = true
		for _, event := range batch.Events {
			result, err := n.Store.Save(event)
			if err != nil {
				return found, err
			}
			if result.Outcome == store.Stored || result.Outcome == store.Duplicate {
				found[event.ID] = event
			}
		}
	}
	if !answered && len(errs) > 0 && len(lacking(ids, found)) > 0 {
		return found, fmt.Errorf("no relay answered: %w", errors.Join(errs...))
	}
	return found, nil
}

func lacking(ids []nostr.ID, found map[nostr.ID]nostr.Event) []nostr.ID {
	var out []nostr.ID
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// Watch keeps each event that a live transport sends, and passes on the ones
// that the store accepted.
func (n *Node) Watch(ctx context.Context, filter nostr.Filter, t transport.Live) (<-chan transport.Received, error) {
	ctx, cancel := context.WithCancel(ctx)
	in, err := t.Watch(ctx, filter)
	if err != nil {
		cancel()
		return nil, err
	}

	out := make(chan transport.Received)
	go func() {
		defer close(out)
		defer cancel()
		for {
			var event nostr.Event
			select {
			case e, ok := <-in:
				if !ok {
					return
				}
				event = e
			case <-ctx.Done():
				return
			}
			result, err := n.Store.Save(event)
			if err == nil && (result.Outcome == store.Refused || result.Outcome == store.Superseded) {
				continue
			}
			select {
			case out <- transport.Received{Event: event, Err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return out, nil
}
