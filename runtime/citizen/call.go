// Package citizen holds application operations shared by ARC entry points.
package citizen

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/transport"
)

// LiveCall tries another path only when the previous path did not submit the
// request. A lost reply is an unknown outcome, never permission to execute again.
func LiveCall(ctx context.Context, signer keys.Signer, provider nostr.PubKey, request call.Request, targets []transport.Transport, timeout time.Duration) (call.Reply, time.Duration, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type answer struct {
		reply   call.Reply
		elapsed time.Duration
		via     string
	}
	got, err := retryUnheard(ctx, func() (answer, error) {
		reply, elapsed, via, err := liveCall(ctx, signer, provider, request, targets)
		return answer{reply, elapsed, via}, err
	})
	return got.reply, got.elapsed, got.via, err
}

// unheardWait is how long a call or a session open tries again when no
// provider heard it. A relay that ended the watch of a provider has a new
// watch within a few seconds, and in that time each live event reaches no
// one (#424).
const unheardWait = 6 * time.Second

// unheardStep is the time between two tries.
const unheardStep = 250 * time.Millisecond

// clock is the time of retryUnheard. A test sets it to see when a try
// would end.
var clock = time.Now

// retryUnheard runs try again while it fails because no one heard it, for
// at most unheardWait. A try that no one heard reached no provider, so to
// repeat it never repeats an operation.
func retryUnheard[T any](ctx context.Context, try func() (T, error)) (T, error) {
	deadline := clock().Add(unheardWait)
	var last T
	var lastErr error
	for {
		start := clock()
		got, err := try()
		took := clock().Sub(start)
		// A try that the deadline of the caller cut off says less than the
		// try before it, which no one heard and which names the relay of
		// the provider: give that one. A try that submitted its request
		// keeps its own result, whose outcome may be unknown (#453).
		if err != nil && lastErr != nil && ctx.Err() != nil && notSubmitted(err) {
			return last, lastErr
		}
		if err == nil || !errors.Is(err, transport.ErrNoListener) || clock().After(deadline) {
			return got, err
		}
		// A try that cannot end before the deadline of the caller would be
		// cut off after its request went out, with an outcome that is
		// unknown. The try that no one heard says more: give it.
		if end, ok := ctx.Deadline(); ok && end.Sub(clock()) < unheardStep+took {
			return got, err
		}
		last, lastErr = got, err
		select {
		case <-time.After(unheardStep):
		case <-ctx.Done():
			return got, err
		}
	}
}

// notSubmitted says whether a try submitted no request.
func notSubmitted(err error) bool {
	_, ok := errors.AsType[*transport.NotSubmittedError](err)
	return ok
}

func liveCall(ctx context.Context, signer keys.Signer, provider nostr.PubKey, request call.Request, targets []transport.Transport) (call.Reply, time.Duration, string, error) {
	var failures []error
	for _, t := range targets {
		exchange, ok := t.(call.Exchanger)
		if !ok {
			continue
		}
		reply, elapsed, err := call.Live(ctx, signer, provider, request, exchange)
		if err == nil {
			return reply, elapsed, t.Name(), nil
		}
		if _, ok := errors.AsType[*transport.NotSubmittedError](err); !ok {
			return call.Reply{}, 0, t.Name(), fmt.Errorf("call outcome unknown via %s; the operation may have executed: %w", t.Name(), err)
		}
		failures = append(failures, fmt.Errorf("%s: %w", t.Name(), err))
		if ctx.Err() != nil {
			break
		}
	}
	return call.Reply{}, 0, "", fmt.Errorf("no live path to %s: %w", keys.Name(provider[:]), errors.Join(append(failures, errors.New("no request was submitted"))...))
}
