package citizen

import (
	"context"
	"errors"
	"fmt"

	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/catalog"
)

// OpenSessionAddress is shared by the CLI and provider-initiated sessions.
// It preserves install consent and selects only live paths. A submitted open
// is never repeated on another path when its outcome is uncertain.
func (sess *Session) OpenSessionAddress(ctx context.Context, installs catalog.Installs, address, body string, mode session.Mode) (*session.Stream, error) {
	provider, offer, path, err := sess.installedService(ctx, installs, address, mode)
	if err != nil {
		return nil, err
	}
	if err = sess.Waker.Wake(ctx, provider[:], sess.online); err != nil {
		return nil, err
	}
	targets, err := sess.liveRelays(ctx, provider)
	if err != nil {
		return nil, err
	}
	request := call.Request{Capability: offer.ID, Method: offer.Method, Path: path, Body: body}
	var failures []error
	for _, target := range targets {
		live, ok := target.(transport.Live)
		if !ok {
			continue
		}
		stream, err := call.OpenSession(ctx, sess.Signer, provider, request, mode, live)
		if err == nil {
			sess.Waker.Answered(provider[:])
			return stream, nil
		}
		var before *transport.NotSubmittedError
		if !errors.As(err, &before) {
			return nil, err
		}
		failures = append(failures, err)
		if ctx.Err() != nil {
			break
		}
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("no live session path: %w", session.ErrUnsupported)
	}
	return nil, fmt.Errorf("no live session path: %w", errors.Join(failures...))
}
