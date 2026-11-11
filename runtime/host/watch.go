package host

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/transport"
)

// watchAll starts a live watch on each relay. It returns when each relay
// has begun or failed its first watch, and at least one relay has the watch.
// If no relay has the watch, watchAll waits until one relay takes it. It
// returns the relays whose first watch failed. Those relays get the watch
// when they come back.
func watchAll(ctx context.Context, server *call.Server, relays []transport.Transport, log *slog.Logger) ([]string, error) {
	type result struct {
		index int
		err   error
	}
	results := make(chan result)
	started := make(chan struct{})
	defer close(started)
	count := 0
	var missing []string
	for i, t := range relays {
		live, ok := t.(transport.Live)
		if !ok {
			missing = append(missing, t.Name())
			continue
		}
		count++
		go keepServing(ctx, server, live, func(err error) {
			select {
			case results <- result{i, err}:
			case <-started:
			case <-ctx.Done():
			}
		}, log)
	}

	answered := map[int]bool{}
	watches := 0
	for len(answered) < count || (watches == 0 && count > 0) {
		select {
		case r := <-results:
			if r.err == nil {
				watches++
			} else if !answered[r.index] {
				missing = append(missing, relays[r.index].Name())
			}
			answered[r.index] = true
		case <-server.Done():
			return nil, errors.New("the app program stopped")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return missing, nil
}

// keepServing answers live calls through one relay, and watches again when
// the relay drops the connection. After each attempt to watch, it calls
// report: with nil when the watch begins, and with the error when the watch
// does not begin.
func keepServing(ctx context.Context, server *call.Server, r transport.Live, report func(error), log *slog.Logger) {
	for {
		began := false
		var start time.Time
		err := server.ServeLive(ctx, r, func() {
			began, start = true, time.Now()
			report(nil)
		})
		if ctx.Err() != nil {
			return
		}
		// While no watch runs, each call to this provider reaches no one,
		// so a watch that ran a while starts again at once (#424). A watch
		// that ends at once waits, so that a relay that ends each watch
		// does not get a new one in a loop.
		wait := 3 * time.Second
		if began {
			log.Warn("the relay ended the watch; watching again", "relay", r.Name(), "error", err)
			if time.Since(start) > 10*time.Second {
				wait = 0
			}
		} else {
			report(err)
			log.Warn("the relay did not take the watch; trying again", "relay", r.Name(), "error", err)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}
