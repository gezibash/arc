// Package citizen holds application operations shared by ARC entry points.
package citizen

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/call"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/transport"
)

// LiveCall tries another path only when the previous path did not submit the
// request. A lost reply is an unknown outcome, never permission to execute again.
func LiveCall(ctx context.Context, signer keys.Signer, provider nostr.PubKey, request call.Request, targets []transport.Transport, timeout time.Duration) (call.Reply, time.Duration, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
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
		var before *transport.NotSubmittedError
		if !errors.As(err, &before) {
			return call.Reply{}, 0, t.Name(), fmt.Errorf("call outcome unknown via %s; the operation may have executed: %w", t.Name(), err)
		}
		failures = append(failures, fmt.Errorf("%s: %w", t.Name(), err))
		if ctx.Err() != nil {
			break
		}
	}
	return call.Reply{}, 0, "", fmt.Errorf("no live path to %s: %w", keys.Name(provider[:]), errors.Join(append(failures, errors.New("no request was submitted"))...))
}
