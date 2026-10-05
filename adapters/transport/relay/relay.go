// Package relay moves events through a Nostr relay, as NIP-01 defines.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip11"
	"fiatjaf.com/nostr/nip77"
	"github.com/gezibash/arc/core/transport"
)

// Timeout bounds one call to a relay.
const Timeout = 15 * time.Second

// errAuthRequired says that the relay wants NIP-42 authentication first.
var errAuthRequired = errors.New("the relay asks for authentication")

// sealedKinds says whether a filter names a kind that a relay may serve only
// to its author: a NIP-37 draft, a checkpoint, a part, or a private relay
// list.
func sealedKinds(filter nostr.Filter) bool {
	for _, k := range filter.Kinds {
		switch k {
		case 31234, 1234, 3275, 10013:
			return true
		}
	}
	return false
}

// reuseWait bounds the wait for an answer on a connection that Send used
// before. A connection can end without notice, for example while the machine
// sleeps. Send then opens a new connection, and sends the event again.
var reuseWait = 5 * time.Second

// Relay is one relay, by its WebSocket URL. A Relay opens a connection for
// each call. A Relay from New keeps one connection for Send.
type Relay struct {
	URL string
	// Signer answers the NIP-42 challenge of a relay, when one is set. A
	// relay accepts a protected event, NIP-70, only from its author after
	// this authentication.
	Signer nostr.Signer
	// kept holds the connection of a relay from New.
	kept *kept
}

// kept is the connection that Send uses again. It carries one Send at a
// time. A relay sends a new NIP-42 challenge with each refusal, and the
// library cannot answer a challenge while a second one arrives.
type kept struct {
	// turn holds one token. The Send that put it there owns conn.
	turn chan struct{}
	conn *nostr.Relay
}

// New returns a relay that keeps one connection for Send. An event then does
// not pay for a new connection and a new authentication. Send opens the
// connection, and opens it again after it ended. Close ends it.
func New(url string, signer nostr.Signer) Relay {
	return Relay{URL: url, Signer: signer, kept: &kept{turn: make(chan struct{}, 1)}}
}

// Close ends the kept connection. A later Send opens a new one.
func (r Relay) Close() error {
	if r.kept == nil {
		return nil
	}
	r.kept.turn <- struct{}{}
	defer func() { <-r.kept.turn }()
	return r.kept.drop()
}

// drop closes the kept connection, so the next Send connects.
func (k *kept) drop() error {
	if k.conn == nil {
		return nil
	}
	conn := k.conn
	k.conn = nil
	return conn.Close()
}

// Name says which relay this is.
func (r Relay) Name() string { return r.URL }

// sendKept publishes one event on the kept connection. If a connection that
// Send used before gives no answer of the relay, sendKept sends the event
// one more time on a new connection. The relay keeps one copy, by ID.
func (r Relay) sendKept(ctx context.Context, event nostr.Event) error {
	k := r.kept
	select {
	case k.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-k.turn }()
	if err := ctx.Err(); err != nil {
		return err
	}

	if k.conn != nil && k.conn.IsConnected() {
		first, cancel := context.WithTimeout(ctx, reuseWait)
		err := r.publish(first, k.conn, event)
		cancel()
		// A refusal is an answer of the relay: the connection works.
		if err == nil || refused(err) {
			return r.named(err)
		}
		if err := ctx.Err(); err != nil {
			_ = k.drop()
			return err
		}
	}
	_ = k.drop()
	conn, err := r.connect(ctx)
	if err != nil {
		return err
	}
	k.conn = conn
	if err = r.publish(ctx, conn, event); err != nil && !refused(err) {
		_ = k.drop()
	}
	return r.named(err)
}

// refused says whether an error is the answer of a relay that did not take
// an event.
func refused(err error) bool { return strings.HasPrefix(err.Error(), "msg: ") }

// named adds the relay to an error.
func (r Relay) named(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("relay %s: %w", r.URL, err)
}

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

	if r.kept != nil {
		return r.sendKept(ctx, event)
	}
	conn, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	return r.named(r.publish(ctx, conn, event))
}

// publish sends one event on a connection. If the relay asks for
// authentication first, publish answers the challenge and sends again.
func (r Relay) publish(ctx context.Context, conn *nostr.Relay, event nostr.Event) error {
	err := conn.Publish(ctx, event)
	if err != nil && strings.Contains(err.Error(), "auth-required") && r.Signer != nil {
		err = r.authenticate(ctx, conn)
		if err == nil {
			err = conn.Publish(ctx, event)
		}
	}
	return err
}

// authenticate answers the relay's challenge. The challenge can arrive just
// after the refusal that asked for it, so authenticate waits for it briefly.
func (r Relay) authenticate(ctx context.Context, conn *nostr.Relay) error {
	var err error
	for range 20 {
		if err = conn.Auth(ctx, r.Signer.SignEvent); err == nil || !strings.Contains(err.Error(), "no challenge") {
			return err
		}
		select {
		case <-time.After(25 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
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
	defer func() { _ = conn.Close() }()

	seen := map[nostr.ID]bool{}
	var batch transport.Batch

	page := filter
	for {
		events, err := stored(ctx, conn, page)
		if errors.Is(err, errAuthRequired) && r.Signer != nil {
			// The relay serves sealed data only to its author: answer the
			// challenge, and ask again.
			if err = r.authenticate(ctx, conn); err == nil {
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

// closed is the error for the reason of a CLOSED message.
func closed(reason string) error {
	if strings.HasPrefix(reason, "auth-required") {
		return errAuthRequired
	}
	return errors.New("the relay closed the query: " + reason)
}

// Exchange sends one event and waits for the first answer that matches. It
// subscribes, waits until the relay has taken the subscription, and only then
// sends, all on one connection. An answer to a live call is ephemeral, and a
// relay never stores it, so a subscription that came after it would miss it.
func (r Relay) Exchange(ctx context.Context, event nostr.Event, answers nostr.Filter, match func(nostr.Event) bool) (nostr.Event, error) {
	conn, err := r.connect(ctx)
	if err != nil {
		return nostr.Event{}, &transport.NotSubmittedError{Err: err}
	}
	defer func() { _ = conn.Close() }()

	sub, err := conn.Subscribe(ctx, answers, nostr.SubscriptionOptions{Label: "arc-exchange"})
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
			return nostr.Event{}, &transport.NotSubmittedError{Err: err}
		}

		return nostr.Event{}, fmt.Errorf("relay %s: %w", r.URL, err)
	}

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

// Reconcile compares the relay's events for a filter with a local set, with
// Negentropy, as NIP-77 defines. It does so only when the relay says in its
// information document that it supports NIP-77. A relay that does not know
// the protocol can stay silent, and a sync would then wait until its timeout.
func (r Relay) Reconcile(ctx context.Context, filter nostr.Filter, local nostr.Querier) ([]nostr.ID, []nostr.ID, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	// The Negentropy session opens its own connection, which cannot answer
	// a challenge. Sealed data therefore syncs by a full fetch.
	if sealedKinds(filter) {
		return nil, nil, false, nil
	}
	info, err := nip11.Fetch(ctx, r.URL)
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
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
// it arrives. It closes the channel when the context ends, or when the relay
// ends the subscription.
//
// Watch returns only after the relay sent its end of stored events. From then
// on, the relay delivers each new matching event, also an ephemeral one that
// it does not keep. If no end of stored events comes within Timeout, Watch
// returns an error.
func (r Relay) Watch(ctx context.Context, filter nostr.Filter) (<-chan nostr.Event, error) {
	conn, err := r.connect(ctx)
	if err != nil {
		return nil, err
	}

	// A relay that serves sealed data only to its author asks for
	// authentication first. A query of stored events finds that out, and
	// authenticates, before the watch begins.
	if r.Signer != nil && sealedKinds(filter) {
		probe := filter
		probe.Limit = 1
		if _, err := stored(ctx, conn, probe); errors.Is(err, errAuthRequired) {
			if err := r.authenticate(ctx, conn); err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("relay %s: %w", r.URL, err)
			}
		}
	}

	sub, err := conn.Subscribe(ctx, filter, nostr.SubscriptionOptions{
		Label:          "arc-watch",
		MaxWaitForEOSE: time.Duration(math.MaxInt64),
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("relay %s: %w", r.URL, err)
	}

	// A relay sends the end of stored events after it takes the
	// subscription. The library gives it only after the stored events are
	// read, so drain keeps them until then.
	wait, cancel := context.WithTimeout(ctx, Timeout)
	pending, err := drain(wait, sub)
	cancel()
	if err != nil {
		sub.Unsub()
		_ = conn.Close()
		return nil, fmt.Errorf("relay %s: the relay did not take the subscription: %w", r.URL, err)
	}

	out := make(chan nostr.Event)
	go func() {
		defer close(out)
		defer func() { _ = conn.Close() }()
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
					return
				}
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
