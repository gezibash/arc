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
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/private"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/provider/host"
)

// The kinds and windows of a call.
const (
	RequestKind nostr.Kind = 3272
	ReplyKind   nostr.Kind = 3273

	// LiveWindow is how old a live request can be. A provider refuses an
	// older one, so a relay cannot replay it later.
	LiveWindow = 5 * time.Minute
	// Timeout bounds how long a provider takes to answer one request.
	Timeout = 60 * time.Second
)

// Request is one call.
type Request struct {
	Capability string
	Method     string
	Path       string
	Body       string
}

// RequestRumor makes the rumor of a request.
func RequestRumor(me keys.Signer, provider nostr.PubKey, r Request, now time.Time) nostr.Event {
	// The nonce makes each request its own. Without it, two equal requests
	// in one second have one ID, and the provider refuses the second as a
	// replay.
	nonce := make([]byte, 8)
	rand.Read(nonce)
	return private.Rumor(me, RequestKind, r.Body, nostr.Tags{
		{"p", provider.Hex()},
		{"capability", r.Capability},
		{"method", r.Method},
		{"path", r.Path},
		{"nonce", hex.EncodeToString(nonce)},
	}, now)
}

// ReadRequest reads a request rumor.
func ReadRequest(rumor nostr.Event) Request {
	return Request{
		Capability: tag(rumor, "capability"), Method: tag(rumor, "method"),
		Path: tag(rumor, "path"), Body: rumor.Content,
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
	now := time.Now()
	rumor := RequestRumor(me, provider, r, now)

	wrap, err := private.Wrap(me, provider, rumor, private.RelayForm, private.LiveWrapKind, now.Add(LiveWindow))
	if err != nil {
		return Reply{}, 0, err
	}

	var reply Reply
	answers := nostr.Filter{Kinds: []nostr.Kind{private.LiveWrapKind}, Tags: nostr.TagMap{"p": {me.PublicKey().Hex()}}}
	start := time.Now()

	_, err = exchange.Exchange(ctx, wrap, answers, func(answer nostr.Event) bool {
		if store.Verify(answer) != nil {
			return false
		}
		opened, err := private.Unwrap(me, answer)
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

// Server passes requests to a provider program, and matches its answers to
// them.
type Server struct {
	key        keys.Signer
	capability string
	process    *host.Process
	maxBytes   int
	log        *slog.Logger

	mu      sync.Mutex
	waiting map[string]chan map[string]any
	seen    map[string]time.Time
	done    chan struct{}
}

// NewServer serves one capability with a running provider program.
func NewServer(k keys.Signer, capability string, process *host.Process, maxBytes int, log *slog.Logger) *Server {
	s := &Server{
		key: k, capability: capability, process: process, maxBytes: maxBytes, log: log,
		waiting: map[string]chan map[string]any{}, seen: map[string]time.Time{},
		done: make(chan struct{}),
	}
	go s.read()
	return s
}

// Done closes when the provider program stops.
func (s *Server) Done() <-chan struct{} { return s.done }

// read passes each answer of the provider to the request that waits for it.
func (s *Server) read() {
	for answer := range s.process.Lines() {
		id, _ := answer["request_id"].(string)
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

	id := rumor.ID.Hex()
	s.mu.Lock()
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
	wait := make(chan map[string]any, 1)
	s.waiting[id] = wait
	s.mu.Unlock()

	err := s.process.Send(map[string]any{
		"op":             "request",
		"message":        request.Body,
		"from":           hex.EncodeToString(rumor.PubKey[:]),
		"meta":           map[string]any{"method": request.Method, "path": request.Path, "capability": request.Capability},
		"arc_session_id": nil,
		"app_session_id": nil,
		"request_id":     id,
		"framed":         true,
	})
	if err != nil {
		s.forget(id)
		return Reply{Err: "provider_unavailable " + err.Error()}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	select {
	case answer, ok := <-wait:
		if !ok {
			return Reply{Err: "provider_unavailable the provider stopped"}, nil
		}
		if message, failed := answer["error"]; failed {
			return Reply{Err: fmt.Sprint(message)}, nil
		}
		reply, _ := answer["reply"].(string)
		return Reply{Body: reply}, nil
	case <-ctx.Done():
		s.forget(id)
		return Reply{Err: "provider_timeout the provider did not answer"}, nil
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

	for wrap := range requests {
		go s.answerLive(ctx, wrap, relay)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("call: the relay ended the watch")
}

func (s *Server) answerLive(ctx context.Context, wrap nostr.Event, relay transport.Transport) {
	if store.Verify(wrap) != nil {
		return
	}
	opened, err := private.Unwrap(s.key, wrap)
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
	out, err := private.Wrap(s.key, opened.Author(), answer, private.RelayForm, private.LiveWrapKind, time.Now().Add(LiveWindow))
	if err != nil {
		return
	}
	if err := relay.Send(ctx, out); err != nil {
		s.log.Warn("the reply did not reach the relay", "error", err)
	}
}
