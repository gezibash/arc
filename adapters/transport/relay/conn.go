package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip11"
	"fiatjaf.com/nostr/nip77"
)

// reuseWait bounds the wait for an answer on a connection that a call used
// before. A connection can end without notice, for example while the machine
// sleeps. Send then opens a new connection, and sends the event again.
var reuseWait = 5 * time.Second

// idleCheck is how long a kept connection can be idle before the next call
// first asks the relay for one answer on it. A connection that gives no
// answer within reuseWait is replaced. Calls that follow each other closely,
// as in a sync, do not pay for the check.
var idleCheck = 10 * time.Second

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
	_, closed := errors.AsType[*ClosedError](err)
	return closed || errors.Is(err, syscall.ECONNREFUSED) || hostClosed(err)
}

// ClosedError is a connection that the host took and closed or reset at
// once. Before the WebSocket handshake, the client reads EOF, a reset, a
// broken pipe while it still writes its request, or, on Linux, an idle
// connection that the server closed, as timing falls (#444). After the
// handshake, and before any answer of the relay, the first call fails as
// the library sees the close (#468). Each says the same, so a caller tests
// for this one type.
type ClosedError struct{ Err error }

func (e *ClosedError) Error() string { return e.Err.Error() }

func (e *ClosedError) Unwrap() error { return e.Err }

// hostClosed says whether err holds a connection that the host closed or
// reset. net/http does not export its error for an idle connection that the
// server closed, so its text says it.
func hostClosed(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "server closed idle connection")
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

// closedUnheard judges a call that failed on a connection on which the
// relay never answered, when no deadline ended it: the host took the WebSocket upgrade, and then closed or reset
// the connection (#468). It is a close at once, as one before the upgrade:
// this process remembers it for quickFor, and the other processes of the
// home never see it (#444). It returns the error as a ClosedError, or nil
// for any other call.
func (k *kept) closedUnheard(ctx context.Context, conn *nostr.Relay, err error) error {
	// A deadline of any kind, also the inner one of a watch, is a silence,
	// not a close.
	if err == nil || answered(err) || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNoAnswer) || k.heardOn(conn) {
		return nil
	}
	closed := &ClosedError{Err: err}
	k.mu.Lock()
	k.down, k.downUntil = closed, time.Now().Add(quickFor)
	if k.conn == conn {
		k.conn = nil
	}
	k.because(conn, "the relay closed the connection before any answer")
	k.mu.Unlock()
	_ = conn.Close()
	return closed
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
// caller must call done when the call ends, with the error of the call, and
// return the error that done gives.
func (r Relay) open(ctx context.Context) (conn *nostr.Relay, reused bool, done func(error) error, err error) {
	k := r.kept
	if k == nil {
		conn, err = r.connect(ctx, &sessions{})
		if err != nil {
			return nil, false, nil, err
		}
		return conn, false, func(err error) error { _ = conn.Close(); return err }, nil
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
	return conn, reused, func(err error) error {
		// A relay that connected, and then gave no EOSE, OK or CLOSED
		// within its own timeout, is down as one that did not connect: each
		// call to it would wait the whole timeout again (#439). One that
		// answers a probe then is up: only that call got no answer, as from
		// a relay that ignores one kind.
		if err == nil || answered(err) {
			k.hear(conn)
		}
		if closed := k.closedUnheard(ctx, conn, err); closed != nil {
			return closed
		}
		if k.silent(ctx, conn, err) {
			return err
		}
		// An answer of the relay shows that the connection works. A caller
		// that stopped says nothing about it. Any other failure, also a
		// deadline, can come from a connection that stalled, so the next call
		// opens a new one.
		if err != nil && !answered(err) && !errors.Is(err, context.Canceled) {
			_ = k.drop(conn, "a call on the connection failed: "+err.Error())
		}
		return err
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
		err = fmt.Errorf("relay %s: %w", r.URL, err)
		// A stall or a timeout cost time, and a refusal has its own reason.
		var timeout net.Error
		if hostClosed(err) && !errors.Is(err, ErrStalled) && !errors.Is(err, syscall.ECONNREFUSED) && !(errors.As(err, &timeout) && timeout.Timeout()) {
			err = &ClosedError{Err: err}
		}
		return nil, err
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
