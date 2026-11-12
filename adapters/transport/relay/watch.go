package relay

import (
	"context"
	"errors"
	"fmt"

	"fiatjaf.com/nostr"
)

// Watch sends the stored events that match the filter, then each new one as
// it arrives. It closes the channel when the context ends, or when the relay
// ends the subscription.
//
// Watch returns only after the relay sent its end of stored events. From then
// on, the relay delivers each new matching event, also an ephemeral one that
// it does not keep. If no end of stored events comes within Timeout, Watch
// returns an error.
func (r Relay) Watch(ctx context.Context, filter nostr.Filter) (<-chan nostr.Event, error) {
	conn, _, done, err := r.open(ctx)
	if err != nil {
		return nil, err
	}

	r.beforeRead(ctx, conn, filter)

	// A relay that serves sealed data only to its author asks for
	// authentication first. A query of stored events finds that out, and
	// authenticates, before the watch begins.
	if r.Signer != nil && sealedKinds(filter) {
		probe := filter
		probe.Limit = 1
		if _, err := stored(ctx, conn, probe); errors.Is(err, errAuthRequired) {
			if err := r.authenticate(ctx, conn); err != nil {
				return nil, fmt.Errorf("relay %s: %w", r.URL, done(err))
			}
		}
	}

	sub, err := conn.Subscribe(ctx, filter, nostr.SubscriptionOptions{
		Label:          "arc-watch",
		MaxWaitForEOSE: realEOSE,
	})
	if err != nil {
		return nil, fmt.Errorf("relay %s: %w", r.URL, done(err))
	}

	// A relay sends the end of stored events after it takes the
	// subscription. The library gives it only after the stored events are
	// read, so drain keeps them until then.
	wait, cancel := context.WithTimeout(ctx, Timeout)
	pending, err := drain(wait, sub)
	cancel()
	if err != nil {
		sub.Unsub()
		return nil, fmt.Errorf("relay %s: the relay did not take the subscription: %w", r.URL, done(err))
	}

	out := make(chan nostr.Event)
	go func() {
		defer close(out)
		defer func() { _ = done(nil) }()
		defer sub.Unsub()

		for _, event := range pending {
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
		for {
			select {
			case event, ok := <-sub.Events:
				// A closed channel means that the subscription ended. Reading
				// on would return empty events at once, forever.
				if !ok {
					if r.kept != nil && r.kept.watchEnded != nil && ctx.Err() == nil {
						r.kept.watchEnded(r.kept.endedBy(conn))
					}
					return
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			case reason := <-sub.ClosedReason:
				if r.kept != nil && r.kept.watchEnded != nil && ctx.Err() == nil {
					r.kept.watchEnded("the relay", "CLOSED: "+reason)
				}
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
