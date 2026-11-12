package relay

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
)

// errAuthRequired says that the relay wants NIP-42 authentication first.
var errAuthRequired = errors.New("the relay asks for authentication")

// challengeWait bounds the wait for the NIP-42 challenge of a relay. A relay
// that rings sends it on each new connection (docs/rings/SPEC.md section
// 3.3), and a relay sends it with a refusal that asks for authentication.
// Under load it can come more than 500 ms later. 3 s is a fifth of Timeout,
// so a read or send that gets no challenge still has time to finish.
var challengeWait = 3 * time.Second

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
	if err := k.isDown(r.URL); err != nil {
		return err
	}

	conn, reused, _, err := r.open(ctx)
	if err != nil {
		return err
	}
	if reused && k.heardOn(conn) {
		first, cancel := context.WithTimeout(ctx, reuseWait)
		err := r.publish(first, conn, event)
		cancel()
		// A refusal is an answer of the relay: the connection works.
		if err == nil || refused(err) {
			k.hear(conn)
			return r.named(err)
		}
		_ = k.drop(conn, "a send on the reused connection got no answer: "+err.Error())
		if err := ctx.Err(); err != nil {
			return err
		}
		if conn, _, _, err = r.open(ctx); err != nil {
			return err
		}
	}
	if err = r.publish(ctx, conn, event); err == nil || refused(err) {
		k.hear(conn)
	} else if closed := k.closedUnheard(ctx, conn, err); closed != nil {
		return r.named(closed)
	} else if !k.silent(ctx, conn, err) {
		_ = k.drop(conn, "a send failed: "+err.Error())
	}
	return r.named(err)
}

// Send publishes one event. A relay that already holds it accepts it again.
func (r Relay) Send(ctx context.Context, event nostr.Event) error {
	ctx, cancel := context.WithTimeoutCause(ctx, callTimeout, ErrNoAnswer)
	defer cancel()

	if r.kept != nil {
		return unheard(r.sendKept(ctx, event))
	}
	conn, err := r.connect(ctx, &sessions{})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	return unheard(r.named(r.publish(ctx, conn, event)))
}

// publish sends one event on a connection. If the relay asks for
// authentication first, publish answers the challenge and sends again. A
// connection that authenticated with the device key cannot send a protected
// event of the citizen, NIP-70; publish then sends it on a new connection
// that authenticates with the citizen key.
func (r Relay) publish(ctx context.Context, conn *nostr.Relay, event nostr.Event) error {
	err := conn.Publish(ctx, event)
	if err != nil && strings.Contains(err.Error(), "auth-required") && r.Signer != nil {
		err = r.authenticate(ctx, conn)
		if err == nil {
			err = conn.Publish(ctx, event)
		}
	}
	if err != nil && strings.Contains(err.Error(), "must be published by event author") && r.Delegation != nil && r.Signer != nil {
		err = r.publishAsCitizen(ctx, event)
	}
	return err
}

// publishAsCitizen sends one event on a new connection that authenticates
// with the citizen key.
func (r Relay) publishAsCitizen(ctx context.Context, event nostr.Event) error {
	conn, err := r.connect(ctx, &sessions{})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := r.challenge(ctx, conn, r.Signer); err != nil {
		return err
	}
	if r.Delegation != nil {
		r.Delegation.Answered(ctx)
	}
	return conn.Publish(ctx, event)
}

// authenticate answers the relay's challenge. The challenge can arrive after
// the connection or the refusal that asked for it, so authenticate waits for
// it up to challengeWait.
// On the kept connection, one call answers, and the calls that wait for it
// then use its answer.
func (r Relay) authenticate(ctx context.Context, conn *nostr.Relay) error {
	if k := r.kept; k != nil {
		k.auth.Lock()
		defer k.auth.Unlock()
		if k.authed == conn {
			return nil
		}
		if err := r.answer(ctx, conn); err != nil {
			return err
		}
		k.authed = conn
		return nil
	}
	return r.answer(ctx, conn)
}

// answer answers the challenge of the relay on conn. A device with a token
// answers with its device key, and gives the token, when the relay takes
// tokens. Else the citizen key answers through Signer.
func (r Relay) answer(ctx context.Context, conn *nostr.Relay) error {
	if r.Delegation != nil {
		if device, token, ok := r.Delegation.Token(ctx); ok && r.takesTokens(ctx) {
			if err := r.challenge(ctx, conn, device); err != nil {
				return err
			}
			return r.giveToken(ctx, conn, token)
		}
	}
	if err := r.challenge(ctx, conn, r.Signer); err != nil {
		return err
	}
	if r.Delegation != nil {
		r.Delegation.Answered(ctx)
	}
	return nil
}

// takesTokens says whether the NIP-11 document of the relay lists
// devices.TokenTag.
func (r Relay) takesTokens(ctx context.Context) bool {
	info, err := r.information(ctx)
	return err == nil && slices.Contains(info.Tags, devices.TokenTag)
}

// giveToken sends the token on conn. The relay keeps it for the
// connection, and broadcasts it to nobody, so khatru answers that nobody
// listened.
func (r Relay) giveToken(ctx context.Context, conn *nostr.Relay, token nostr.Event) error {
	err := conn.Publish(ctx, token)
	if err != nil && !strings.Contains(err.Error(), "mute: no one was listening") {
		return fmt.Errorf("the relay refused the token of this device: %w", err)
	}
	return nil
}

// challenge answers the challenge of the relay on conn with signer. It
// waits up to challengeWait for the challenge to arrive. The library gives
// no signal when the challenge arrives, so challenge asks again every 25 ms.
func (r Relay) challenge(ctx context.Context, conn *nostr.Relay, signer nostr.Signer) error {
	deadline := time.Now().Add(challengeWait)
	for {
		err := conn.Auth(ctx, signer.SignEvent)
		if err == nil || !strings.Contains(err.Error(), "no challenge") || time.Now().After(deadline) {
			return err
		}
		select {
		case <-time.After(25 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
