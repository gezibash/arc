// Package relay moves events through a Nostr relay, as NIP-01 defines.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip11"
	"fiatjaf.com/nostr/nip77"
	"fiatjaf.com/nostr/nip77/negentropy"
	"fiatjaf.com/nostr/nip77/negentropy/storage/vector"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/transport"
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

// errAuthRequired says that the relay wants NIP-42 authentication first.
var errAuthRequired = errors.New("the relay asks for authentication")

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

// reuseWait bounds the wait for an answer on a connection that a call used
// before. A connection can end without notice, for example while the machine
// sleeps. Send then opens a new connection, and sends the event again.
var reuseWait = 5 * time.Second

// idleCheck is how long a kept connection can be idle before the next call
// first asks the relay for one answer on it. A connection that gives no
// answer within reuseWait is replaced. Calls that follow each other closely,
// as in a sync, do not pay for the check.
var idleCheck = 10 * time.Second

// challengeWait bounds the wait for the NIP-42 challenge of a relay. A relay
// that rings sends it on each new connection (docs/rings/SPEC.md section
// 3.3), and a relay sends it with a refusal that asks for authentication.
// Under load it can come more than 500 ms later. 3 s is a fifth of Timeout,
// so a read or send that gets no challenge still has time to finish.
var challengeWait = 3 * time.Second

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

// kept is the connection that each call of a Relay from New uses. Many
// subscriptions share it. Sends go one at a time: a relay sends a new NIP-42
// challenge with each refusal, and the library cannot answer a challenge
// while a second one arrives.
type kept struct {
	// turn holds one token. The Send that put it there sends next.
	turn chan struct{}

	// notice gets each NOTICE that starts with "arc-ring:". It lives here,
	// not in Relay, so that a Relay stays comparable.
	notice func(text string)
	// authFailed gets the first error of each streak of failed
	// authentications before a read of mail. told says that the current
	// streak was told.
	authFailed func(err error)
	told       bool
	// watchEnded gets who ended a watch and why, when a watch ends before
	// its caller stops it.
	watchEnded func(by, why string)
	// closed holds why arc closed each connection that it closed. mu
	// guards it.
	closed map[*nostr.Relay]string

	// mu guards conn and used. A call holds it while it connects.
	mu   sync.Mutex
	conn *nostr.Relay
	// used is when a call last took conn.
	used time.Time
	// heard is conn once the relay answered on it. A connection that
	// never answered has no answer to lose, so a stalled send does not
	// open a new one (#442).
	heard *nostr.Relay
	// down is the error of the last connect that failed, until downUntil:
	// a fetch in that time fails at once with it (#439).
	down      error
	downUntil time.Time
	// shared keeps the relays that did not connect for the other
	// processes of the same home, or nil.
	shared DownStore

	// info is the NIP-11 document, after the first fetch that worked.
	infoMu sync.Mutex
	info   *nip11.RelayInformationDocument

	// auth makes one authentication at a time. authed is the connection
	// that answered the challenge of the relay.
	auth   sync.Mutex
	authed *nostr.Relay

	// sessions sends each NIP-77 message on conn to its session.
	sessions *sessions
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

// ErrNoAnswer is the cause of the timeout of a call of the relay: the
// relay, not the caller, ended the call.
var ErrNoAnswer = errors.New("the relay gave no answer in time")

// downFor is how long a fetch fails at once after a connect failed.
var downFor = 30 * time.Second

// DownFor is how long a relay that did not answer stays down.
func DownFor() time.Duration { return downFor }

// DownStore keeps the relays that did not connect, so that the other
// processes of the same home do not wait for them again. A store that
// fails says so as no record: it never fails a call.
type DownStore interface {
	// Down returns until when a relay is down, and why, if it is.
	Down(url string) (until time.Time, why string, ok bool)
	// MarkDown keeps that a relay is down until a time.
	MarkDown(url string, until time.Time, why string)
	// Up forgets that a relay is down.
	Up(url string)
}

// ShareDown makes a Relay from New keep the relays that did not connect in
// s, and read them there, for downFor. Call it before the first call.
func (r Relay) ShareDown(s DownStore) {
	if r.kept != nil {
		r.kept.shared = s
	}
}

// DownError is the error of a call to a relay that did not connect a short
// time before, in this process or in another process of the same home.
type DownError struct {
	URL, Why string
	Until    time.Time
	// Err is the error of the connect, in this process, or nil.
	Err error
}

func (e *DownError) Error() string {
	return fmt.Sprintf("relay %s is skipped: %s at the last try; arc tries it again in %s", e.URL, e.Why, e.Again())
}

// Again is the time until arc tries the relay again.
func (e *DownError) Again() time.Duration {
	return max(time.Until(e.Until), 0).Round(time.Second)
}

// downReason is the short reason that a relay is down: a host that does not
// exist, or no answer. A refusal never marks a relay down.
func downReason(err error) string {
	if dns, ok := errors.AsType[*net.DNSError](err); ok && dns.IsNotFound {
		return "no such host"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "the connection was refused"
	}
	if closedAtOnce(err) {
		return "the connection was closed"
	}
	return "no answer"
}

// quickFor is how long this process remembers a connect that failed at
// once: a refused connection, or one that the host closed or reset. The
// calls of one command then do not dial the relay again for each call, and
// a long process, such as arc serve, uses the relay soon after it comes
// back. Such a failure is never kept for the other processes (#444).
var quickFor = 5 * time.Second

// closedAtOnce says whether err is a connection that the host refused,
// closed or reset at once.
func closedAtOnce(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (e *DownError) Unwrap() error { return e.Err }

// isDown is the error of the last connect to url that failed, while it is
// newer than downFor, or nil.
func (k *kept) isDown(url string) error {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	down, until, shared := k.down, k.downUntil, k.shared
	k.mu.Unlock()
	if down != nil && time.Now().Before(until) {
		return &DownError{URL: url, Why: downReason(down), Until: until, Err: down}
	}
	// The store says which marks count: it reads them once for the whole
	// command, so a mark that runs out in the middle of it does not make
	// the command dial a relay that it skipped (#444).
	if shared != nil {
		if until, why, ok := shared.Down(url); ok {
			return &DownError{URL: url, Why: why, Until: until}
		}
	}
	return nil
}

// silent judges a call on a connection that the relay took, and that gave
// no EOSE, OK or CLOSED within the relay's own timeout (#439). It asks the
// relay one question that each relay answers at once: a relay that answers
// is up, and silent says so; one that does not is down for downFor. A
// refusal is an answer, and the deadline of the caller is not the relay's.
func (k *kept) silent(ctx context.Context, conn *nostr.Relay, err error) (up bool) {
	if err == nil || answered(err) || !errors.Is(context.Cause(ctx), ErrNoAnswer) {
		return false
	}
	if conn != nil && conn.IsConnected() && alive(context.Background(), conn) {
		return true
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.down, k.downUntil = err, time.Now().Add(downFor)
	if k.shared != nil && conn != nil {
		k.shared.MarkDown(conn.URL, k.downUntil, downReason(err))
	}
	return false
}

// failedToReach says whether err is a first attempt that cost time and
// reached no relay: one that stalled, a dial or a lookup that timed out, or
// a lookup or an address that failed. A mark of a relay exists to save
// that time. A host that refused, closed or reset the connection answered
// at once: a mark would save nothing, and would skip the relay after it
// came back (#444). reach tries the other address family after any failed
// attempt, so only its first attempt says whether the relay answered.
func failedToReach(err error) bool {
	for e := err; e != nil; {
		if joined, ok := e.(interface{ Unwrap() []error }); ok {
			if errs := joined.Unwrap(); len(errs) > 0 {
				err = errs[0]
			}
			break
		}
		e = errors.Unwrap(e)
	}
	var timeout net.Error
	var dns *net.DNSError
	var addr *net.AddrError
	return errors.Is(err, ErrStalled) || errors.As(err, &timeout) && timeout.Timeout() || errors.As(err, &dns) || errors.As(err, &addr)
}

// hear keeps that the relay answered on conn.
func (k *kept) hear(conn *nostr.Relay) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.conn == conn {
		k.heard = conn
	}
}

// heardOn says whether the relay answered on conn.
func (k *kept) heardOn(conn *nostr.Relay) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.heard == conn
}

// drop closes conn, and keeps why. If conn is the kept connection, the next
// call connects.
func (k *kept) drop(conn *nostr.Relay, why string) error {
	if conn == nil {
		return nil
	}
	k.mu.Lock()
	if k.conn == conn {
		k.conn = nil
	}
	k.because(conn, why)
	k.mu.Unlock()
	return conn.Close()
}

// because keeps why arc closes conn. The caller holds mu.
func (k *kept) because(conn *nostr.Relay, why string) {
	// A connection with no watch keeps its reason until here: a few dozen
	// reasons are enough for the watches that end.
	if k.closed == nil || len(k.closed) > 64 {
		k.closed = map[*nostr.Relay]string{}
	}
	k.closed[conn] = why
}

// endedBy says who ended the connection of a watch, and why.
func (k *kept) endedBy(conn *nostr.Relay) (string, string) {
	cause := context.Cause(conn.Context())
	k.mu.Lock()
	why, ok := k.closed[conn]
	delete(k.closed, conn)
	k.mu.Unlock()
	switch {
	case ok:
		return "arc", why
	case errors.Is(cause, nostr.ErrPingFailed):
		return "arc", "3 pings of the connection failed"
	case cause != nil:
		return "the relay or the network", cause.Error()
	}
	return "unknown", "the subscription ended"
}

// open returns the connection for one call. For a Relay from New, it is the
// kept connection, and reused says whether an earlier call opened it. The
// caller must call done when the call ends, with the error of the call.
func (r Relay) open(ctx context.Context) (conn *nostr.Relay, reused bool, done func(error), err error) {
	k := r.kept
	if k == nil {
		conn, err = r.connect(ctx, &sessions{})
		if err != nil {
			return nil, false, nil, err
		}
		return conn, false, func(error) { _ = conn.Close() }, nil
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.conn != nil && k.conn.IsConnected() && time.Since(k.used) > idleCheck && !alive(ctx, k.conn) {
		k.because(k.conn, fmt.Sprintf("the connection was idle %s, and gave no answer to a check within %s", time.Since(k.used).Round(time.Second), reuseWait))
		_ = k.conn.Close()
	}
	if k.conn != nil && k.conn.IsConnected() {
		reused = true
	} else {
		if k.conn != nil {
			_ = k.conn.Close()
			k.conn = nil
		}
		if k.conn, err = r.connect(ctx, k.sessions); err != nil {
			// A relay that did not connect is down for a while: the other
			// calls of a command do not wait for it again (#439). A caller
			// that stopped says nothing about the relay.
			// Only the relay's own timeout, or a dial that failed, marks it
			// down: never a refusal, nor the deadline of the caller.
			if ctx.Err() == nil && failedToReach(err) || errors.Is(context.Cause(ctx), ErrNoAnswer) {
				k.down, k.downUntil = err, time.Now().Add(downFor)
				if k.shared != nil {
					k.shared.MarkDown(r.URL, k.downUntil, downReason(err))
				}
			} else if ctx.Err() == nil && closedAtOnce(err) {
				k.down, k.downUntil = err, time.Now().Add(quickFor)
			}
			return nil, false, nil, err
		}
		if k.down != nil && k.shared != nil {
			k.shared.Up(r.URL)
		}
		k.down = nil
	}
	k.used = time.Now()
	conn = k.conn
	return conn, reused, func(err error) {
		// A relay that connected, and then gave no EOSE, OK or CLOSED
		// within its own timeout, is down as one that did not connect: each
		// call to it would wait the whole timeout again (#439). One that
		// answers a probe then is up: only that call got no answer, as from
		// a relay that ignores one kind.
		if err == nil || answered(err) {
			k.hear(conn)
		}
		if k.silent(ctx, conn, err) {
			return
		}
		// An answer of the relay shows that the connection works. A caller
		// that stopped says nothing about it. Any other failure, also a
		// deadline, can come from a connection that stalled, so the next call
		// opens a new one.
		if err != nil && !answered(err) && !errors.Is(err, context.Canceled) {
			_ = k.drop(conn, "a call on the connection failed: "+err.Error())
		}
	}, nil
}

// alive says whether a connection still carries an answer of the relay. It
// asks for an event that no relay holds, and waits for the end of stored
// events.
func alive(ctx context.Context, conn *nostr.Relay) bool {
	ctx, cancel := context.WithTimeout(ctx, reuseWait)
	defer cancel()
	_, err := stored(ctx, conn, nostr.Filter{IDs: []nostr.ID{{}}})
	return err == nil || answered(err)
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
	} else if !k.silent(ctx, conn, err) {
		_ = k.drop(conn, "a send failed: "+err.Error())
	}
	return r.named(err)
}

// refused says whether an error is the answer of a relay that did not take
// an event.
func refused(err error) bool { return strings.HasPrefix(err.Error(), "msg: ") }

// answerError is the reason of a CLOSED message: an answer of the relay.
type answerError struct{ reason string }

func (e answerError) Error() string { return "the relay closed the query: " + e.reason }

// answered says whether an error is an answer of the relay, so the connection
// that carried it works.
func answered(err error) bool {
	var answer answerError
	return refused(err) || errors.Is(err, errAuthRequired) || errors.As(err, &answer)
}

// named adds the relay to an error.
func (r Relay) named(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("relay %s: %w", r.URL, err)
}

// connect opens a connection to the relay. It sends each NIP-77 message to
// its session in s. If the first attempt fails or stalls, connect tries the
// other address family once.
func (r Relay) connect(ctx context.Context, s *sessions) (*nostr.Relay, error) {
	var conn *nostr.Relay
	err := reach(ctx, r.URL, func(ctx context.Context, client *http.Client) error {
		conn = nostr.NewRelay(context.Background(), r.URL, nostr.RelayOptions{
			NoticeHandler: func(_ *nostr.Relay, text string) {
				if r.kept != nil && r.kept.notice != nil && strings.HasPrefix(text, ringNotice) {
					r.kept.notice(text)
				}
			},
			CustomHandler: s.dispatch,
		})
		if err := conn.ConnectWithClient(ctx, client); err != nil {
			_ = conn.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("relay %s: %w", r.URL, err)
	}
	return conn, nil
}

// sessions holds the NIP-77 sessions that run on one connection, by
// subscription ID.
type sessions struct {
	next    atomic.Int64
	mu      sync.Mutex
	replies map[string]func(nostr.Envelope)
}

// add registers a session, and returns its subscription ID, the channel of
// its messages, and a function that removes it.
func (s *sessions) add(ctx context.Context) (string, <-chan nostr.Envelope, func()) {
	id := "arc-negentropy-" + strconv.FormatInt(s.next.Add(1), 10)
	replies := make(chan nostr.Envelope)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replies == nil {
		s.replies = map[string]func(nostr.Envelope){}
	}
	s.replies[id] = func(env nostr.Envelope) {
		select {
		case replies <- env:
		case <-ctx.Done():
		}
	}
	return id, replies, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.replies, id)
	}
}

// dispatch gives one NIP-77 message to its session.
func (s *sessions) dispatch(data string) {
	env := negMessage(data)
	if env == nil {
		return
	}
	var id string
	switch env := env.(type) {
	case *nip77.MessageEnvelope:
		id = env.SubscriptionID
	case *nip77.ErrorEnvelope:
		id = env.SubscriptionID
	default:
		return
	}
	s.mu.Lock()
	deliver := s.replies[id]
	s.mu.Unlock()
	if deliver != nil {
		deliver(env)
	}
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

// unheard marks the answer of a relay that no one got a live event, which
// khatru gives when no watch matched it: the event reached no one.
func unheard(err error) error {
	if err != nil && strings.Contains(err.Error(), "mute: no one was listening") {
		return transport.NoListener(err)
	}
	return err
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

// closed is the error for the reason of a CLOSED message.
func closed(reason string) error {
	if strings.HasPrefix(reason, "auth-required") {
		return errAuthRequired
	}
	return answerError{reason}
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
	var done func(error)
	if r.kept != nil {
		conn, _, done, err = r.open(ctx)
	} else if conn, err = r.connect(ctx, s); err == nil {
		done = func(error) { _ = conn.Close() }
	}
	if err != nil {
		return nil, nil, err
	}
	defer func() { done(err) }()
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
				done(err)
				return nil, fmt.Errorf("relay %s: %w", r.URL, err)
			}
		}
	}

	sub, err := conn.Subscribe(ctx, filter, nostr.SubscriptionOptions{
		Label:          "arc-watch",
		MaxWaitForEOSE: realEOSE,
	})
	if err != nil {
		done(err)
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
		done(err)
		return nil, fmt.Errorf("relay %s: the relay did not take the subscription: %w", r.URL, err)
	}

	out := make(chan nostr.Event)
	go func() {
		defer close(out)
		defer done(nil)
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
