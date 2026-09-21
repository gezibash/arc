// Package relay moves events through a Nostr relay, as NIP-01 defines.
package relay

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/transport"
)

// Timeout bounds one call to a relay.
const Timeout = 15 * time.Second

// Relay is one relay, by its WebSocket URL.
type Relay struct {
	URL string
}

// Name says which relay this is.
func (r Relay) Name() string { return r.URL }

func (r Relay) connect(ctx context.Context) (*nostr.Relay, error) {
	conn, err := nostr.RelayConnect(ctx, r.URL, nostr.RelayOptions{
		NoticeHandler: func(*nostr.Relay, string) {},
	})
	if err != nil {
		return nil, fmt.Errorf("relay %s: %w", r.URL, err)
	}
	return conn, nil
}

// Send publishes one event. A relay that already holds it accepts it again.
func (r Relay) Send(ctx context.Context, event nostr.Event) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	conn, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.Publish(ctx, event); err != nil {
		return fmt.Errorf("relay %s: %w", r.URL, err)
	}
	return nil
}

// Fetch returns every stored event that matches the filter. A relay caps how
// many events one query returns, so Fetch asks again for older events until a
// page brings nothing new.
func (r Relay) Fetch(ctx context.Context, filter nostr.Filter) (transport.Batch, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	conn, err := r.connect(ctx)
	if err != nil {
		return transport.Batch{}, err
	}
	defer conn.Close()

	seen := map[nostr.ID]bool{}
	var batch transport.Batch

	page := filter
	for {
		events, err := stored(ctx, conn, page)
		if err != nil {
			return batch, fmt.Errorf("relay %s: %w", r.URL, err)
		}

		oldest, fresh := nostr.Timestamp(math.MaxInt64), 0
		for _, event := range events {
			if event.CreatedAt < oldest {
				oldest = event.CreatedAt
			}
			if !seen[event.ID] {
				seen[event.ID] = true
				batch.Events = append(batch.Events, event)
				fresh++
			}
		}

		if fresh == 0 || oldest == 0 || (filter.Since != 0 && oldest <= filter.Since) {
			return batch, nil
		}
		page.Until = oldest
	}
}

// stored reads the stored events of one subscription, up to its end of
// stored events.
func stored(ctx context.Context, conn *nostr.Relay, filter nostr.Filter) ([]nostr.Event, error) {
	sub, err := conn.Subscribe(ctx, filter, nostr.SubscriptionOptions{Label: "arc"})
	if err != nil {
		return nil, err
	}
	defer sub.Unsub()

	var out []nostr.Event
	for {
		select {
		case event := <-sub.Events:
			out = append(out, event)
		case <-sub.EndOfStoredEvents:
			return out, nil
		case reason := <-sub.ClosedReason:
			return out, errors.New("the relay closed the query: " + reason)
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
}

// Watch sends the stored events that match the filter, then each new one as
// it arrives, until the context ends.
func (r Relay) Watch(ctx context.Context, filter nostr.Filter) (<-chan nostr.Event, error) {
	conn, err := r.connect(ctx)
	if err != nil {
		return nil, err
	}

	sub, err := conn.Subscribe(ctx, filter, nostr.SubscriptionOptions{
		Label:          "arc-watch",
		MaxWaitForEOSE: time.Duration(math.MaxInt64),
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("relay %s: %w", r.URL, err)
	}

	out := make(chan nostr.Event)
	go func() {
		defer close(out)
		defer conn.Close()
		defer sub.Unsub()

		for {
			select {
			case event := <-sub.Events:
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			case <-sub.ClosedReason:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
