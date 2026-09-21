// Package node joins a store to its transports.
//
// Every event that reaches the node goes through the store first, so it is
// verified before anything else sees it. The node never trusts a transport.
package node

import (
	"context"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/transport"
)

// Node is the store of one citizen on one machine.
type Node struct {
	Store *store.Store
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
		need, give, ok, err := r.Reconcile(ctx, filter, n.Store)
		if err != nil {
			return report, err
		}
		if ok {
			report.Reconciled = true
			return report, n.exchange(ctx, t, need, give, &report)
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
		if err := n.keep(event, &report); err != nil {
			return report, err
		}
	}

	for _, event := range n.Store.Query(filter) {
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
// the transport needs, a batch at a time.
func (n *Node) exchange(ctx context.Context, t transport.Transport, need, give []nostr.ID, report *Report) error {
	const size = 100

	for i := 0; i < len(need); i += size {
		batch, err := t.Fetch(ctx, nostr.Filter{IDs: need[i:min(i+size, len(need))]})
		if err != nil {
			return err
		}
		report.Unreadable += batch.Unreadable
		for _, event := range batch.Events {
			if err := n.keep(event, report); err != nil {
				return err
			}
		}
	}

	for i := 0; i < len(give); i += size {
		for _, event := range n.Store.Query(nostr.Filter{IDs: give[i:min(i+size, len(give))]}) {
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
	return nil
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
		for _, event := range batch.Events {
			if err := n.keep(event, &report); err != nil {
				errs = append(errs, err)
				break
			}
		}
		reports = append(reports, report)
	}
	return reports, errs
}

// Obtain returns the events with these IDs. It reads the store first, and
// asks the transports only for the events that the store lacks. It keeps what
// the transports return, after verification.
func (n *Node) Obtain(ctx context.Context, ids []nostr.ID, transports []transport.Transport) (map[nostr.ID]nostr.Event, error) {
	found := make(map[nostr.ID]nostr.Event, len(ids))
	for _, event := range n.Store.Query(nostr.Filter{IDs: ids}) {
		found[event.ID] = event
	}

	for _, t := range transports {
		missing := lacking(ids, found)
		if len(missing) == 0 {
			break
		}

		batch, err := t.Fetch(ctx, nostr.Filter{IDs: missing})
		if err != nil {
			continue
		}
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
func (n *Node) Watch(ctx context.Context, filter nostr.Filter, t transport.Live) (<-chan nostr.Event, error) {
	in, err := t.Watch(ctx, filter)
	if err != nil {
		return nil, err
	}

	out := make(chan nostr.Event)
	go func() {
		defer close(out)
		for event := range in {
			result, err := n.Store.Save(event)
			if err != nil || result.Outcome == store.Refused || result.Outcome == store.Superseded {
				continue
			}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
