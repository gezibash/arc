// Package relay moves events through a Nostr relay, as NIP-01 defines.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip11"
	"fiatjaf.com/nostr/nip77"
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

// Reconcile compares the relay's events for a filter with a local set, with
// Negentropy, as NIP-77 defines. It does so only when the relay says in its
// information document that it supports NIP-77. A relay that does not know
// the protocol can stay silent, and a sync would then wait until its timeout.
func (r Relay) Reconcile(ctx context.Context, filter nostr.Filter, local nostr.Querier) ([]nostr.ID, []nostr.ID, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	info, err := nip11.Fetch(ctx, r.URL)
	if err != nil || !supports(info.SupportedNIPs, 77) {
		return nil, nil, false, nil
	}

	var mu sync.Mutex
	var need, give []nostr.ID
	err = nip77.NegentropySync(ctx, r.URL, filter, local, discard{},
		func(_ context.Context, dir nip77.Direction) {
			for id := range dir.Items {
				mu.Lock()
				if dir.From == local {
					give = append(give, id)
				} else {
					need = append(need, id)
				}
				mu.Unlock()
			}
		})
	if err != nil {
		return nil, nil, true, fmt.Errorf("relay %s: %w", r.URL, err)
	}
	return need, give, true, nil
}

func supports(nips []any, nip int) bool {
	for _, n := range nips {
		switch v := n.(type) {
		case float64:
			if int(v) == nip {
				return true
			}
		case int:
			if v == nip {
				return true
			}
		case json.Number:
			if v.String() == strconv.Itoa(nip) {
				return true
			}
		}
	}
	return false
}

// discard is a target that turns on the download direction of a sync. The
// caller fetches and verifies each event itself.
type discard struct{}

func (discard) Publish(context.Context, nostr.Event) error { return nil }

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
