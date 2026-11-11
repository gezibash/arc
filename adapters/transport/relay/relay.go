// Package relay moves events through a Nostr relay, as NIP-01 defines.
package relay

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip11"
)

// The Nostr library writes an info line to standard error for each event
// that it drops: an event that does not match the filter of its
// subscription, an event with a bad signature, and an OK for an event that
// it did not send. The drop is correct, and arc reports its own errors, so
// the line only confuses the user of arc.
func init() { nostr.InfoLogger.SetOutput(io.Discard) }

// Timeout bounds one call to a relay.
const Timeout = 15 * time.Second

// realEOSE turns off the end of stored events that the Nostr library makes
// up when a relay does not send one within 7 seconds. A loaded relay can be
// slower. The made-up end drops the stored events that are still on the
// way, so a query would give too few events, and no error. A caller then
// takes an announced provider for offline. Each call waits for the real end
// of stored events, within its own deadline.
const realEOSE = time.Duration(math.MaxInt64)

// Relay is one relay, by its WebSocket URL. A Relay opens a connection for
// each call. A Relay from New keeps one connection, and each of its calls
// uses it.
type Relay struct {
	URL string
	// Signer answers the NIP-42 challenge of a relay, when one is set. A
	// relay accepts a protected event, NIP-70, only from its author after
	// this authentication.
	Signer nostr.Signer
	// Delegation lets a device read the sealed data of its citizen with its
	// own key, when the relay takes tokens. It can be nil.
	Delegation Delegation
	// kept holds the connection of a relay from New.
	kept *kept
}

// Delegation is the device key and the token of a device. A relay whose
// NIP-11 document lists devices.TokenTag then serves the sealed data of the
// citizen to the device key, with no signature of the citizen key.
type Delegation interface {
	// Token returns the device key, and a token that lasts for some time
	// still. It returns false when the device holds no such token.
	Token(ctx context.Context) (nostr.Signer, nostr.Event, bool)
	// Answered says that the signer answered the challenge of a relay for
	// the citizen key. The device can then ask the signer for a new token.
	Answered(ctx context.Context)
}

// New returns a relay that keeps one connection for all of its calls. A call
// then does not pay for a new connection and a new authentication. A call
// opens the connection, and opens it again after it ended. Close ends it.
func New(url string, signer nostr.Signer) Relay {
	return Relay{URL: url, Signer: signer, kept: &kept{turn: make(chan struct{}, 1), sessions: &sessions{}}}
}

// OnNotice makes a Relay from New give each NOTICE that starts with
// "arc-ring:", such as the warning that the rings of the citizen fail, to
// notice. Call it before the first call to the relay.
func (r Relay) OnNotice(notice func(text string)) {
	if r.kept != nil {
		r.kept.notice = notice
	}
}

// OnAuthFailure makes a Relay from New give the first error of an
// authentication before a read of mail to failed, once for each streak of
// failures: after an authentication that works, the next failure is told. Such
// a read still reads, but the relay does not count it for the rings of the
// citizen. The error can hold text that the relay chose. Call it before the
// first call to the relay.
func (r Relay) OnAuthFailure(failed func(err error)) {
	if r.kept != nil {
		r.kept.authFailed = failed
	}
}

// OnWatchEnd makes a Relay from New give who ended a watch, and why, each
// time a watch ends before its caller stops it (#437): the relay with a
// CLOSED message, the relay or the network that ended the connection, or
// arc that closed the connection, also for failed pings. Call it before the
// first call to the relay.
func (r Relay) OnWatchEnd(ended func(by, why string)) {
	if r.kept != nil {
		r.kept.watchEnded = ended
	}
}

// Tags returns the tags of the NIP-11 document of the relay.
func (r Relay) Tags(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	info, err := r.information(ctx)
	if err != nil {
		return nil, fmt.Errorf("relay %s: %w", r.URL, err)
	}
	return info.Tags, nil
}

// information returns the NIP-11 document of the relay. A Relay from New
// fetches it once.
func (r Relay) information(ctx context.Context) (nip11.RelayInformationDocument, error) {
	if r.kept == nil {
		return information(ctx, r.URL)
	}
	r.kept.infoMu.Lock()
	defer r.kept.infoMu.Unlock()
	if r.kept.info != nil {
		return *r.kept.info, nil
	}
	info, err := information(ctx, r.URL)
	if err == nil {
		r.kept.info = &info
	}
	return info, err
}

// Close ends the kept connection. A later call opens a new one.
func (r Relay) Close() error {
	if r.kept == nil {
		return nil
	}
	r.kept.turn <- struct{}{}
	defer func() { <-r.kept.turn }()
	r.kept.mu.Lock()
	conn := r.kept.conn
	r.kept.mu.Unlock()
	return r.kept.drop(conn, "Close of the relay")
}

// callTimeout is Timeout, the time that a send, a query or a NIP-77
// session gives the relay. A test makes it short.
var callTimeout = Timeout

// Name says which relay this is.
func (r Relay) Name() string { return r.URL }
