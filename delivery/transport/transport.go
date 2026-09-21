// Package transport names what every transport gives a node.
//
// A transport moves events between two nodes. It never verifies an event:
// the store does that, whatever transport the event came through.
package transport

import (
	"context"

	"fiatjaf.com/nostr"
)

// Batch is what a transport returned for one filter.
type Batch struct {
	Events []nostr.Event
	// Unreadable counts items that the transport held but could not parse.
	Unreadable int
	// Hops holds the hop limit that a courier wrote beside an event. Only a
	// carrier transport fills it.
	Hops map[nostr.ID]int
}

// Transport moves events.
type Transport interface {
	// Name says where the transport reaches, for reports.
	Name() string
	// Send gives one event to the transport.
	Send(ctx context.Context, event nostr.Event) error
	// Fetch returns every event that the transport holds for a filter.
	Fetch(ctx context.Context, filter nostr.Filter) (Batch, error)
}

// Carrier is a transport that a person carries between nodes, such as a
// directory. Anyone who reads it gets a copy, so a hop limit beside each event
// bounds how far couriers spread it.
type Carrier interface {
	Transport
	// SendHops gives one event to the transport, with the number of couriers
	// that may still carry it on.
	SendHops(ctx context.Context, event nostr.Event, hops int) error
}

// Reconciler is a transport that can compare its events with a local set
// without sending either set whole.
type Reconciler interface {
	Transport
	// Reconcile returns the IDs that the local set lacks, and the IDs that
	// the transport lacks. ok is false when the transport cannot reconcile
	// now; the caller then fetches instead.
	Reconcile(ctx context.Context, filter nostr.Filter, local nostr.Querier) (need, give []nostr.ID, ok bool, err error)
}

// Live is a transport that can deliver an event while both nodes are
// present, and can tell a node about new events as they arrive.
type Live interface {
	Transport
	// Watch sends each event that matches the filter, stored or new, until
	// the context ends.
	Watch(ctx context.Context, filter nostr.Filter) (<-chan nostr.Event, error)
}
