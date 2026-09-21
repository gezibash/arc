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

// Live is a transport that can deliver an event while both nodes are
// present, and can tell a node about new events as they arrive.
type Live interface {
	Transport
	// Watch sends each event that matches the filter, stored or new, until
	// the context ends.
	Watch(ctx context.Context, filter nostr.Filter) (<-chan nostr.Event, error)
}
