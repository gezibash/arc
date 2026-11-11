package relay

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/transport"
)

// sealedKinds says whether a filter names a kind that a relay may serve only
// to its author: a NIP-37 draft, a checkpoint, a part, a private relay
// list, a storage list, a device list, or the data roots. ARC no longer
// makes a private relay list, but a device lists the old ones to delete
// them.
func sealedKinds(filter nostr.Filter) bool {
	for _, k := range filter.Kinds {
		switch k {
		case 31234, 1234, 3275, 10013, 10273, 10274, 10275:
			return true
		}
	}
	return false
}

// ringsTag is the NIP-11 tag of a relay that rings a citizen when mail
// waits unread (docs/rings/SPEC.md). Such a relay counts a read only on a
// connection that authenticated.
const ringsTag = "arc-rings"

// ringNotice starts each NOTICE of a relay about the rings of the citizen
// (docs/rings/SPEC.md, section 3.7).
const ringNotice = "arc-ring:"

// readsMail says whether a filter reads the gift wraps of a recipient.
func readsMail(filter nostr.Filter) bool {
	return len(filter.Tags["p"]) > 0 && (len(filter.Kinds) == 0 || slices.Contains(filter.Kinds, 1059))
}

// beforeRead authenticates on conn before a read of mail, when the relay
// rings: the read then ends the unread period of the citizen. A read that
// cannot authenticate still reads, and the first error of each streak goes
// to the function of OnAuthFailure.
func (r Relay) beforeRead(ctx context.Context, conn *nostr.Relay, filter nostr.Filter) {
	if r.Signer == nil || !readsMail(filter) {
		return
	}
	if info, err := r.information(ctx); err != nil || !slices.Contains(info.Tags, ringsTag) {
		return
	}
	err := r.authenticate(ctx, conn)
	if r.kept == nil {
		return
	}
	// A success ends a streak of failures, so the next failure is told.
	r.kept.auth.Lock()
	first := err != nil && !r.kept.told
	r.kept.told = err != nil
	r.kept.auth.Unlock()
	if first && r.kept.authFailed != nil {
		r.kept.authFailed(err)
	}
}

// Fetch returns every stored event that matches the filter. A relay caps how
// many events one query returns, so Fetch asks again for older events until a
// page brings nothing new.
func (r Relay) Fetch(ctx context.Context, filter nostr.Filter) (transport.Batch, error) {
	if err := r.kept.isDown(r.URL); err != nil {
		return transport.Batch{}, err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, callTimeout, ErrNoAnswer)
	defer cancel()

	conn, _, done, err := r.open(ctx)
	if err != nil {
		return transport.Batch{}, err
	}
	r.beforeRead(ctx, conn, filter)
	batch, err := r.fetch(ctx, conn, filter)
	done(err)
	return batch, err
}

// fetch reads the pages of a query on one connection.
func (r Relay) fetch(ctx context.Context, conn *nostr.Relay, filter nostr.Filter) (transport.Batch, error) {
	seen := map[nostr.ID]bool{}
	var batch transport.Batch

	page := filter
	for {
		events, err := stored(ctx, conn, page)
		if errors.Is(err, errAuthRequired) && r.Signer != nil {
			// The relay serves sealed data only to its author: answer the
			// challenge, and ask again.
			if aerr := r.authenticate(ctx, conn); aerr != nil {
				// The relay still asked for authentication: keep that it
				// refused, with why the answer failed (#463).
				err = fmt.Errorf("%w, and arc could not answer: %v", errAuthRequired, aerr)
			} else {
				events, err = stored(ctx, conn, page)
			}
		}
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

		// A filter with IDs names its whole result, and a relay can ignore
		// until for it, so a second page would only bring the same events.
		if len(filter.IDs) > 0 || fresh == 0 || oldest == 0 || (filter.Since != 0 && oldest <= filter.Since) {
			return batch, nil
		}
		page.Until = oldest
	}
}

// stored reads the stored events of one subscription, up to its end of
// stored events.
func stored(ctx context.Context, conn *nostr.Relay, filter nostr.Filter) ([]nostr.Event, error) {
	sub, err := conn.Subscribe(ctx, filter, nostr.SubscriptionOptions{Label: "arc", MaxWaitForEOSE: realEOSE})
	if err != nil {
		return nil, err
	}
	defer sub.Unsub()
	return drain(ctx, sub)
}

// drain reads the events of a subscription until its end of stored events.
func drain(ctx context.Context, sub *nostr.Subscription) ([]nostr.Event, error) {
	var out []nostr.Event
	for {
		select {
		case event, ok := <-sub.Events:
			if ok {
				out = append(out, event)
				continue
			}
			// The library closes Events when the subscription ends. On a
			// CLOSED message, it first puts the reason on ClosedReason, and
			// then closes Events. Both channels can be ready at once, and
			// select then picks one at random. Look for the reason first, so
			// that a request for authentication is not lost.
			select {
			case <-sub.EndOfStoredEvents:
				return out, nil
			case reason := <-sub.ClosedReason:
				return out, closed(reason)
			default:
				return out, errors.New("the relay ended the query before its stored events")
			}
		case <-sub.EndOfStoredEvents:
			return out, nil
		case reason := <-sub.ClosedReason:
			return out, closed(reason)
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
}

// Exchange sends one event and waits for the first answer that matches. It
// subscribes, waits until the relay has taken the subscription, and only then
// sends, all on one connection. An answer to a live call is ephemeral, and a
// relay never stores it, so a subscription that came after it would miss it.
func (r Relay) Exchange(ctx context.Context, event nostr.Event, answers nostr.Filter, match func(nostr.Event) bool) (answer nostr.Event, err error) {
	if err := r.kept.isDown(r.URL); err != nil {
		return nostr.Event{}, &transport.NotSubmittedError{Err: err}
	}
	conn, _, done, err := r.open(ctx)
	if err != nil {
		return nostr.Event{}, &transport.NotSubmittedError{Err: err}
	}
	// After the relay took the event, the connection works. A provider that
	// does not answer in time says nothing about it.
	taken := false
	defer func() {
		if taken {
			done(nil)
		} else {
			done(err)
		}
	}()

	sub, err := conn.Subscribe(ctx, answers, nostr.SubscriptionOptions{Label: "arc-exchange", MaxWaitForEOSE: realEOSE})
	if err != nil {
		return nostr.Event{}, &transport.NotSubmittedError{Err: fmt.Errorf("relay %s: %w", r.URL, err)}
	}
	defer sub.Unsub()

	select {
	case <-sub.EndOfStoredEvents:
	case reason := <-sub.ClosedReason:
		return nostr.Event{}, &transport.NotSubmittedError{Err: fmt.Errorf("relay %s: the relay closed the subscription: %s", r.URL, reason)}
	case <-ctx.Done():
		return nostr.Event{}, &transport.NotSubmittedError{Err: ctx.Err()}
	}

	if err := r.publish(ctx, conn, event); err != nil {
		// This explicit refusal means no subscriber received the ephemeral event.
		if err.Error() == "msg: mute: no one was listening for this" {
			return nostr.Event{}, transport.NoListener(err)
		}

		return nostr.Event{}, fmt.Errorf("relay %s: %w", r.URL, err)
	}
	taken = true

	for {
		select {
		case answer, ok := <-sub.Events:
			if !ok {
				return nostr.Event{}, fmt.Errorf("relay %s: the relay ended the subscription", r.URL)
			}
			if match(answer) {
				return answer, nil
			}
		case reason := <-sub.ClosedReason:
			return nostr.Event{}, fmt.Errorf("relay %s: the relay closed the subscription: %s", r.URL, reason)
		case <-ctx.Done():
			return nostr.Event{}, ctx.Err()
		}
	}
}
