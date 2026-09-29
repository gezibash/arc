// Package call carries a call to a capability, and its reply.
//
// A request is a rumor of kind 3272, and a reply is a rumor of kind 3273 that
// names its request with an e tag. Both travel as private events. A live call
// uses an ephemeral gift wrap, kind 21059, that no relay stores, and fails at
// once when no path exists. A store-and-forward call uses the mail layer, and
// can travel for days.
package call

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/private"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/provider/wire"
)

// The kinds and windows of a call.
const (
	RequestKind nostr.Kind = 3272
	ReplyKind   nostr.Kind = 3273

	// LiveWindow is how old a live request can be. A provider refuses an
	// older one, so a relay cannot replay it later.
	LiveWindow = 5 * time.Minute
	// Timeout bounds how long a provider takes to answer one request.
	Timeout = wire.RequestTimeout
	// CallTimeout bounds one call that a provider program makes. It is less
	// than Timeout, so a request that makes one call still gets its answer.
	CallTimeout = wire.WorkTimeout
)

// Request is one call.
type Request struct {
	Capability string
	Method     string
	Path       string
	Body       string
	// DeadlineMS is set for live calls, and omitted for queued requests.
	DeadlineMS int64
}

// RequestRumor makes the rumor of a request.
func RequestRumor(me keys.Signer, provider nostr.PubKey, r Request, now time.Time) nostr.Event {
	// The nonce makes each request its own. Without it, two equal requests
	// in one second have one ID, and the provider refuses the second as a
	// replay.
	nonce := make([]byte, 8)
	rand.Read(nonce)
	tags := nostr.Tags{
		{"p", provider.Hex()},
		{"capability", r.Capability},
		{"method", r.Method},
		{"path", r.Path},
		{"nonce", hex.EncodeToString(nonce)},
	}
	if r.DeadlineMS > 0 {
		tags = append(tags, nostr.Tag{"deadline", strconv.FormatInt(r.DeadlineMS, 10)})
	}
	return private.Rumor(me, RequestKind, r.Body, tags, now)
}

// ReadRequest reads a request rumor.
func ReadRequest(rumor nostr.Event) Request {
	deadline, _ := strconv.ParseInt(tag(rumor, "deadline"), 10, 64)
	return Request{
		Capability: tag(rumor, "capability"), Method: tag(rumor, "method"),
		Path: tag(rumor, "path"), Body: rumor.Content, DeadlineMS: deadline,
	}
}

func tag(event nostr.Event, name string) string {
	if t := event.Tags.Find(name); len(t) > 1 {
		return t[1]
	}
	return ""
}

// Reply is the answer of a provider. Err is set when the provider refused or
// failed the request.
type Reply struct {
	Body string
	Err  string
}

// ReplyRumor makes the rumor of a reply to a request.
func ReplyRumor(me keys.Signer, request nostr.Event, reply Reply, now time.Time) nostr.Event {
	status, body := "ok", reply.Body
	if reply.Err != "" {
		status, body = "error", reply.Err
	}
	return private.Rumor(me, ReplyKind, body, nostr.Tags{
		{"e", request.ID.Hex()},
		{"p", request.PubKey.Hex()},
		{"status", status},
	}, now)
}

// ReadReply reads a reply rumor: the request that it answers, and the reply.
func ReadReply(rumor nostr.Event) (string, Reply) {
	if tag(rumor, "status") == "error" {
		return tag(rumor, "e"), Reply{Err: rumor.Content}
	}
	return tag(rumor, "e"), Reply{Body: rumor.Content}
}

// Live makes one live call over a relay, and returns the reply and the time
// that the round trip took.
func Live(ctx context.Context, me keys.Signer, provider nostr.PubKey, r Request, exchange Exchanger) (Reply, time.Duration, error) {
	ctx, cancel := wire.Budget(ctx, 0)
	defer cancel()
	r.DeadlineMS = wire.Deadline(ctx)
	now := time.Now()
	rumor := RequestRumor(me, provider, r, now)

	wrap, err := private.Wrap(ctx, me, provider, rumor, private.RelayForm, private.LiveWrapKind, now.Add(LiveWindow))
	if err != nil {
		return Reply{}, 0, &transport.NotSubmittedError{Err: err}
	}

	var reply Reply
	answers := nostr.Filter{Kinds: []nostr.Kind{private.LiveWrapKind}, Tags: nostr.TagMap{"p": {me.PublicKey().Hex()}}}
	start := time.Now()

	_, err = exchange.Exchange(ctx, wrap, answers, func(answer nostr.Event) bool {
		if store.Verify(answer) != nil {
			return false
		}
		opened, err := private.Unwrap(ctx, me, answer)
		if err != nil || opened.Rumor.Kind != ReplyKind || opened.Author() != provider {
			return false
		}
		id, got := ReadReply(opened.Rumor)
		if id != rumor.ID.Hex() {
			return false
		}
		reply = got
		return true
	})
	if err != nil {
		return Reply{}, 0, err
	}
	return reply, time.Since(start), nil
}

// Exchanger sends one event and waits for a matching answer. The relay
// transport is one.
type Exchanger interface {
	Exchange(ctx context.Context, event nostr.Event, answers nostr.Filter, match func(nostr.Event) bool) (nostr.Event, error)
}

// Client is the context-aware provider protocol boundary. A subprocess is one
// adapter; tests and embedded providers can supply another.
type Client interface {
	Send(context.Context, wire.Event) error
	Lines() <-chan wire.Event
}

// Server passes requests to a provider program, and matches its answers to
// them.
type Server struct {
	key        keys.Signer
	capability string
	process    Client
	maxBytes   int
	log        *slog.Logger

	mu          sync.Mutex
	waiting     map[string]chan wire.Event
	seen        map[string]time.Time
	done        chan struct{}
	caller      Caller
	ctx         context.Context
	cancel      context.CancelFunc
	slots       chan struct{}
	callSlots   chan struct{}
	activeCalls map[string]context.CancelFunc
}

// Outbound is a call that the provider program makes.
type Outbound struct {
	// Address names the capability: <scheme>+arc://<provider>/<path>.
	Address string
	Body    string
}

// Caller makes the calls of a provider program. It returns the reply of the
// provider that got the call. It returns an error when it could not make the
// call.
type Caller func(ctx context.Context, out Outbound) (Reply, error)

// NewServer serves one capability with a running provider program. The
// caller makes the calls of the program. With a nil caller, each call of the
// program fails.
func NewServer(k keys.Signer, capability string, process Client, maxBytes int, caller Caller, log *slog.Logger) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		key: k, capability: capability, process: process, maxBytes: maxBytes, log: log,
		waiting: map[string]chan wire.Event{}, seen: map[string]time.Time{},
		activeCalls: map[string]context.CancelFunc{},
		done:        make(chan struct{}), caller: caller,
		ctx: ctx, cancel: cancel, slots: make(chan struct{}, wire.MaxConcurrent), callSlots: make(chan struct{}, wire.MaxConcurrent),
	}
	go s.read()
	return s
}

// Done closes when the provider program stops.
func (s *Server) Done() <-chan struct{} { return s.done }

// read passes each answer of the provider to the request that waits for it,
// and makes each call of the provider.
func (s *Server) read() {
	for answer := range s.process.Lines() {
		if answer.Op == "cancel" && answer.CallID != "" {
			s.mu.Lock()
			cancel := s.activeCalls[answer.CallID]
			s.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			continue
		}
		if answer.Op == "call" {
			if answer.CallID == "" {
				s.log.Warn("the provider wrote a call with no call_id")
				continue
			}
			select {
			case s.callSlots <- struct{}{}:
				ctx, cancel := wire.Budget(s.ctx, answer.DeadlineMS)
				s.mu.Lock()
				_, duplicate := s.activeCalls[answer.CallID]
				if !duplicate {
					s.activeCalls[answer.CallID] = cancel
				}
				s.mu.Unlock()
				if duplicate {
					cancel()
					<-s.callSlots
					s.log.Warn("duplicate outbound call", "call_id", answer.CallID)
					continue
				}
				go func() {
					defer func() {
						cancel()
						s.mu.Lock()
						delete(s.activeCalls, answer.CallID)
						s.mu.Unlock()
						<-s.callSlots
					}()
					s.call(ctx, answer)
				}()
			default:
				ctx, cancel := context.WithTimeout(s.ctx, time.Second)
				_ = s.process.Send(ctx, wire.Event{Op: "result", CallID: answer.CallID, Error: "provider_busy"})
				cancel()
			}
			continue
		}
		id, _ := answer.RequestID.(string)
		s.mu.Lock()
		wait := s.waiting[id]
		delete(s.waiting, id)
		s.mu.Unlock()

		if wait == nil {
			s.log.Debug("the provider answered a request that is not waiting", "request", id)
			continue
		}
		wait <- answer
	}

	// The provider stopped. Every waiting request fails.
	defer close(s.done)
	defer s.cancel()
	s.mu.Lock()
	for id, wait := range s.waiting {
		close(wait)
		delete(s.waiting, id)
	}
	s.mu.Unlock()
}

// ErrDuplicate says that the server has answered this request before.
var ErrDuplicate = errors.New("call: this request was answered before")

// Handle passes one request rumor to the provider, and returns its answer.
// The author of the rumor is the caller, and the provider sees them as from.
func (s *Server) Handle(ctx context.Context, rumor nostr.Event) (Reply, error) {
	if rumor.Kind != RequestKind {
		return Reply{}, fmt.Errorf("call: kind %d is not a request", rumor.Kind)
	}
	request := ReadRequest(rumor)
	if request.Capability != s.capability {
		return Reply{Err: "unknown_capability " + request.Capability}, nil
	}
	if len(request.Body) > s.maxBytes {
		return Reply{Err: "request_too_large the body is over the limit"}, nil
	}

	ctx, cancel := wire.Budget(ctx, request.DeadlineMS)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Reply{Err: "provider_timeout the request deadline has passed"}, nil
	}
	id := rumor.ID.Hex()
	s.mu.Lock()
	if len(s.waiting) >= wire.MaxConcurrent {
		s.mu.Unlock()
		return Reply{Err: "provider_busy"}, nil
	}
	if _, done := s.seen[id]; done {
		s.mu.Unlock()
		return Reply{}, ErrDuplicate
	}
	s.seen[id] = time.Now()
	for old, at := range s.seen {
		if time.Since(at) > 2*LiveWindow {
			delete(s.seen, old)
		}
	}
	wait := make(chan wire.Event, 1)
	s.waiting[id] = wait
	s.mu.Unlock()

	err := s.process.Send(ctx, wire.Event{
		Op: "request", Message: wire.Text(request.Body), From: rumor.PubKey.Hex(),
		Meta:      map[string]any{"method": request.Method, "path": request.Path, "capability": request.Capability},
		RequestID: id, Framed: true, DeadlineMS: wire.Deadline(ctx),
	})
	if err != nil {
		s.forget(id)
		return Reply{Err: "provider_unavailable " + err.Error()}, nil
	}

	select {
	case answer, ok := <-wait:
		if !ok {
			return Reply{Err: "provider_unavailable the provider stopped"}, nil
		}
		if answer.Error != "" {
			return Reply{Err: answer.Error}, nil
		}
		if answer.Reply == nil {
			return Reply{Err: "invalid_reply"}, nil
		}
		return Reply{Body: *answer.Reply}, nil
	case <-ctx.Done():
		s.forget(id)
		stopCtx, stop := context.WithTimeout(s.ctx, time.Second)
		_ = s.process.Send(stopCtx, wire.Event{Op: "cancel", RequestID: id})
		stop()
		return Reply{Err: "provider_timeout the provider did not answer"}, nil
	}
}

// call makes one call of the provider program, and writes its result to the
// program. See the package provider for the lines.
func (s *Server) call(ctx context.Context, line wire.Event) {
	result := wire.Event{Op: "result", CallID: line.CallID}
	switch {
	case line.Address == "" || line.Body == nil:
		result.Error = "invalid_call: a call needs an address and a body"
	case s.caller == nil:
		result.Error = "calls_off: this host makes no calls"
	default:
		reply, err := s.caller(ctx, Outbound{Address: line.Address, Body: *line.Body})
		switch {
		case err != nil:
			result.Error = err.Error()
		case reply.Err != "":
			result.Refused = reply.Err
		default:
			result.Reply = wire.Text(reply.Body)
		}
	}
	if err := s.process.Send(ctx, result); err != nil {
		s.log.Warn("the result of a call did not reach the provider", "error", err)
	}
}

func (s *Server) forget(id string) {
	s.mu.Lock()
	delete(s.waiting, id)
	s.mu.Unlock()
}

// ServeLive answers live calls that reach this provider through a relay. It
// refuses a request older than LiveWindow, so a relay cannot replay one later.
// It calls ready, when ready is not nil, once the relay has taken the watch.
// It returns when the relay ends the watch or the context ends.
func (s *Server) ServeLive(ctx context.Context, relay transport.Live, ready func()) error {
	ctx, cancel := context.WithCancel(ctx)
	var group sync.WaitGroup
	defer func() { cancel(); group.Wait() }()
	requests, err := relay.Watch(ctx, nostr.Filter{
		Kinds: []nostr.Kind{private.LiveWrapKind},
		Tags:  nostr.TagMap{"p": {s.key.PublicKey().Hex()}},
	})
	if err != nil {
		return err
	}
	if ready != nil {
		ready()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case wrap, ok := <-requests:
			if !ok {
				return errors.New("call: the relay ended the watch")
			}
			select {
			case s.slots <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			group.Add(1)
			go func() { defer group.Done(); defer func() { <-s.slots }(); s.answerLive(ctx, wrap, relay) }()
		}
	}
}

func (s *Server) answerLive(ctx context.Context, wrap nostr.Event, relay transport.Transport) {
	if store.Verify(wrap) != nil {
		return
	}
	opened, err := private.Unwrap(ctx, s.key, wrap)
	if err != nil || opened.Rumor.Kind != RequestKind {
		return
	}

	age := time.Since(time.Unix(int64(opened.Rumor.CreatedAt), 0))
	if age > LiveWindow || age < -LiveWindow {
		s.log.Info("refused a live request outside the window", "age", age.Round(time.Second))
		return
	}

	reply, err := s.Handle(ctx, opened.Rumor)
	if err != nil {
		return
	}

	answer := ReplyRumor(s.key, opened.Rumor, reply, time.Now())
	out, err := private.Wrap(ctx, s.key, opened.Author(), answer, private.RelayForm, private.LiveWrapKind, time.Now().Add(LiveWindow))
	if err != nil {
		return
	}
	if err := relay.Send(ctx, out); err != nil {
		s.log.Warn("the reply did not reach the relay", "error", err)
	}
}
