package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"sync"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip77"
	"fiatjaf.com/nostr/nip77/negentropy"
	"fiatjaf.com/nostr/nip77/negentropy/storage/vector"
)

// Reconcile compares the relay's events for a filter with a local set, with
// Negentropy, as NIP-77 defines. It does so only when the relay says in its
// information document that it supports NIP-77. A relay that does not know
// the protocol can stay silent, and a sync would then wait until its timeout.
func (r Relay) Reconcile(ctx context.Context, filter nostr.Filter, local nostr.Querier) ([]nostr.ID, []nostr.ID, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	if err := r.kept.isDown(r.URL); err != nil {
		return nil, nil, true, err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, callTimeout, ErrNoAnswer)
	defer cancel()

	info, err := r.information(ctx)
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	if err != nil || !supports(info.SupportedNIPs, 77) {
		return nil, nil, false, nil
	}

	need, give, err := r.negentropy(ctx, filter, local)
	if err != nil {
		return nil, nil, true, fmt.Errorf("relay %s: %w", r.URL, err)
	}
	return need, give, true, nil
}

// negentropy runs one NIP-77 session. The session of the nostr library opens
// its own connection, which cannot answer a challenge, so a relay would
// refuse it sealed data. This session runs on the connection of the other
// calls, answers the challenge, and opens again.
func (r Relay) negentropy(ctx context.Context, filter nostr.Filter, local nostr.Querier) (need []nostr.ID, give []nostr.ID, err error) {
	const frame = 60_000

	s := &sessions{}
	if r.kept != nil {
		s = r.kept.sessions
	}
	var conn *nostr.Relay
	var done func(error) error
	if r.kept != nil {
		conn, _, done, err = r.open(ctx)
	} else if conn, err = r.connect(ctx, s); err == nil {
		done = func(err error) error { _ = conn.Close(); return err }
	}
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = done(err) }()
	id, replies, remove := s.add(ctx)
	defer remove()

	vec := vector.New()
	for event := range local.QueryEvents(filter) {
		vec.Insert(event.CreatedAt, event.ID)
	}
	vec.Seal()

	open := func() (*negentropy.Negentropy, error) {
		neg := negentropy.New(vec, frame, true, true)
		msg, _ := nip77.OpenEnvelope{SubscriptionID: id, Filter: filter, Message: neg.Start()}.MarshalJSON()
		return neg, conn.WriteWithError(msg)
	}
	neg, err := open()
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		msg, _ := nip77.CloseEnvelope{SubscriptionID: id}.MarshalJSON()
		conn.Write(msg)
	}()

	// Reconcile writes the IDs on two channels, and blocks when they are
	// full, so they are read while the session runs.
	var wg sync.WaitGroup
	reading, authed := false, false
	for {
		var env nostr.Envelope
		select {
		case env = <-replies:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		switch env := env.(type) {
		case *nip77.ErrorEnvelope:
			if authed || r.Signer == nil || !strings.HasPrefix(env.Reason, "auth-required:") {
				return nil, nil, answerError{env.Reason}
			}
			if err := r.authenticate(ctx, conn); err != nil {
				return nil, nil, err
			}
			authed = true
			if neg, err = open(); err != nil {
				return nil, nil, err
			}
		case *nip77.MessageEnvelope:
			if !reading {
				reading = true
				wg.Go(func() { need = slices.AppendSeq(need, chanSeq(ctx, neg.HaveNots)) })
				wg.Go(func() { give = slices.AppendSeq(give, chanSeq(ctx, neg.Haves)) })
			}
			next, err := neg.Reconcile(env.Message)
			if err != nil {
				return nil, nil, err
			}
			if next == "" {
				wg.Wait()
				return need, give, nil
			}
			msg, _ := nip77.MessageEnvelope{SubscriptionID: id, Message: next}.MarshalJSON()
			if err := conn.WriteWithError(msg); err != nil {
				return nil, nil, err
			}
		}
	}
}

// negMessage parses a NIP-77 message. NIP-77 names the error NEG-ERR, but a
// khatru relay writes NEG-ERROR, which the parser of the library drops.
func negMessage(data string) nostr.Envelope {
	if env := nip77.ParseNegMessage(data); env != nil {
		return env
	}
	var fields []string
	if json.Unmarshal([]byte(data), &fields) == nil && len(fields) == 3 && fields[0] == "NEG-ERROR" {
		return &nip77.ErrorEnvelope{SubscriptionID: fields[1], Reason: fields[2]}
	}
	return nil
}

// chanSeq reads c until it closes or ctx ends. A session that fails never
// closes c, and Reconcile then cancels ctx.
func chanSeq(ctx context.Context, c <-chan nostr.ID) iter.Seq[nostr.ID] {
	return func(yield func(nostr.ID) bool) {
		for {
			select {
			case id, ok := <-c:
				if !ok || !yield(id) {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}
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
