package call

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/provider/wire"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
)

// SessionKind carries versioned session frames inside authenticated live wraps.
const SessionKind nostr.Kind = 3276

type sessionMessage struct {
	Frame   session.Frame `json:"frame"`
	Request *Request      `json:"request,omitempty"`
}

func sendSession(ctx context.Context, me keys.Signer, to nostr.PubKey, t transport.Transport, f session.Frame, request *Request) error {
	body, err := json.Marshal(sessionMessage{f, request})
	if err != nil {
		return err
	}
	rumor := private.Rumor(me, SessionKind, string(body), nostr.Tags{{"p", to.Hex()}, {"session", f.ID}}, time.Now())
	wrap, err := private.Wrap(ctx, me, to, rumor, private.RelayForm, private.LiveWrapKind, time.Now().Add(LiveWindow))
	if err != nil {
		return err
	}
	return t.Send(ctx, wrap)
}
func readSession(rumor nostr.Event) (sessionMessage, error) {
	var m sessionMessage
	if len(rumor.Content) > 3*session.MaxChunk {
		return m, session.ErrProtocol
	}
	if err := json.Unmarshal([]byte(rumor.Content), &m); err != nil {
		return m, session.ErrProtocol
	}
	if m.Frame.ID != tag(rumor, "session") {
		return m, session.ErrProtocol
	}
	return m, m.Frame.Validate()
}

// OpenSession uses any Live event adapter. It subscribes before submitting the
// open, verifies every peer frame, and ends when that watch is lost. It does
// not silently reconnect, change paths or replay an uncertain open.
func OpenSession(ctx context.Context, me keys.Signer, to nostr.PubKey, request Request, mode session.Mode, t transport.Live) (*session.Stream, error) {
	if len(request.Body) > session.MaxChunk {
		return nil, &transport.NotSubmittedError{Err: errors.New("session initial request exceeds 16 KiB")}
	}
	ctx, cancel := session.Budget(ctx, wire.Deadline(ctx))
	request.DeadlineMS = wire.Deadline(ctx)
	id := session.ID()
	stream, err := session.New(ctx, id, mode, true, func(ctx context.Context, f session.Frame) error { return sendSession(ctx, me, to, t, f, nil) })
	if err != nil {
		cancel()
		return nil, &transport.NotSubmittedError{Err: err}
	}
	events, err := t.Watch(ctx, nostr.Filter{Kinds: []nostr.Kind{private.LiveWrapKind}, Tags: nostr.TagMap{"p": {me.PublicKey().Hex()}}})
	if err != nil {
		cancel()
		return nil, &transport.NotSubmittedError{Err: err}
	}
	go func() {
		fail := func(err error) {
			stream.Abort(err)
			notice, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer stop()
			_ = sendSession(notice, me, to, t, session.Frame{Version: 1, ID: id, Op: "cancel"}, nil)
		}
		defer cancel()
		defer func() {
			if ctx.Err() != nil {
				_ = stream.Close()
			}
		}()
		for {
			select {
			case <-stream.Context().Done():
				return
			case <-ctx.Done():
				return
			case wrap, ok := <-events:
				if !ok {
					fail(session.ErrDisconnected)
					return
				}
				if store.Verify(wrap) != nil {
					continue
				}
				opened, err := private.Unwrap(ctx, me, wrap)
				if err != nil || opened.Author() != to || opened.Rumor.Kind != SessionKind {
					continue
				}
				m, err := readSession(opened.Rumor)
				if m.Frame.ID != id {
					continue
				}
				if err != nil {
					fail(err)
					return
				}
				if err = stream.Receive(m.Frame); err != nil {
					fail(err)
					return
				}
			}
		}
	}()
	frame := session.Frame{Version: 1, ID: id, Op: "open", Mode: mode}
	if err = sendSession(ctx, me, to, t, frame, &request); err == nil {
		err = stream.WaitReady()
	}
	if err != nil {
		_ = stream.Close()
		cancel()
		return nil, fmt.Errorf("session open failed; outcome may be unknown: %w", err)
	}
	return stream, nil
}

// SessionCaller is application routing and consent for provider-initiated sessions.
type SessionCaller func(context.Context, Outbound, session.Mode) (*session.Stream, error)

type sessionRoute struct {
	from   nostr.PubKey
	ctx    context.Context
	cancel context.CancelFunc
	in     chan wire.Event
	out    chan session.Frame
	via    transport.Transport
	once   sync.Once
}

func (s *Server) handleSessionFrame(ctx context.Context, rumor nostr.Event, via transport.Transport) {
	m, err := readSession(rumor)
	if err != nil {
		return
	}
	f := m.Frame
	s.mu.Lock()
	route := s.sessions[f.ID]
	s.mu.Unlock()
	if route != nil {
		if route.from != rumor.PubKey {
			return
		}
		if f.Op == "open" {
			return
		}
		e := wire.Event{Op: "session", RequestID: f.ID, Session: &f}
		select {
		case route.in <- e:
		case <-route.ctx.Done():
		default:
			route.cancel()
		}
		return
	}
	if f.Op != "open" || m.Request == nil {
		return
	}
	reject := func(reason string) {
		_ = sendSession(ctx, s.key, rumor.PubKey, via, session.Frame{Version: 1, ID: f.ID, Op: "close", Error: reason}, nil)
	}
	request := m.Request
	if request.Capability != s.capability {
		reject("unknown_capability")
		return
	}
	if len(request.Body) > min(s.maxBytes, session.MaxChunk) {
		reject("request_too_large")
		return
	}
	if !session.Supports(s.interactions, f.Mode) {
		reject(session.ErrUnsupported.Error())
		return
	}
	lifetime, cancel := session.Budget(ctx, request.DeadlineMS)
	if lifetime.Err() != nil {
		cancel()
		reject("provider_timeout")
		return
	}
	s.mu.Lock()
	if len(s.sessions)+len(s.waiting) >= wire.MaxConcurrent {
		s.mu.Unlock()
		cancel()
		reject("provider_busy")
		return
	}
	if _, seen := s.seen[f.ID]; seen {
		s.mu.Unlock()
		cancel()
		return
	}
	s.seen[f.ID] = time.Now()
	for id, at := range s.seen {
		if time.Since(at) > 2*LiveWindow {
			delete(s.seen, id)
		}
	}
	route = &sessionRoute{from: rumor.PubKey, ctx: lifetime, cancel: cancel, via: via, in: make(chan wire.Event, 4), out: make(chan session.Frame, 4)}
	s.sessions[f.ID] = route
	s.mu.Unlock()
	stop := context.AfterFunc(s.ctx, cancel)
	cleanup := func() {
		route.once.Do(func() {
			cancel()
			stop()
			s.mu.Lock()
			delete(s.sessions, f.ID)
			s.mu.Unlock()
			notice, done := context.WithTimeout(context.WithoutCancel(s.ctx), time.Second)
			defer done()
			closed := session.Frame{Version: 1, ID: f.ID, Op: "cancel"}
			_ = s.process.Send(notice, wire.Event{Op: "session", RequestID: f.ID, Session: &closed})
		})
	}
	route.in <- wire.Event{Op: "session", RequestID: f.ID, Session: &f, From: rumor.PubKey.Hex(), Message: wire.Text(request.Body),
		Meta: map[string]any{"method": request.Method, "path": request.Path, "capability": request.Capability}, DeadlineMS: wire.Deadline(lifetime)}
	go func() {
		defer cleanup()
		for {
			select {
			case <-lifetime.Done():
				return
			case e := <-route.in:
				if err := s.process.Send(lifetime, e); err != nil {
					return
				}
				if e.Session.Op == "cancel" {
					return
				}
			}
		}
	}()
	go func() {
		defer cleanup()
		for {
			select {
			case <-lifetime.Done():
				return
			case frame := <-route.out:
				if err := sendSession(lifetime, s.key, route.from, via, frame, nil); err != nil {
					return
				}
				if frame.Op == "close" || frame.Op == "cancel" {
					return
				}
			}
		}
	}()
}

// routeSessionOutput cannot let a slow transport block the provider's shared
// stdout reader. Its bounded per-session queue also carries control frames.
func (s *Server) routeSessionOutput(e wire.Event) {
	if e.Session == nil || e.Session.Validate() != nil {
		return
	}
	f := *e.Session
	if e.CallID != "" {
		if e.CallID != f.ID {
			return
		}
		s.mu.Lock()
		stream := s.outgoingSessions[f.ID]
		s.mu.Unlock()
		if stream != nil {
			_ = stream.Receive(f)
			return
		}
		if f.Op == "open" {
			s.startSessionCall(e)
		}
		return
	}
	if e.RequestID != f.ID {
		return
	}
	s.mu.Lock()
	route := s.sessions[f.ID]
	s.mu.Unlock()
	if route == nil {
		return
	}
	select {
	case route.out <- f:
	case <-route.ctx.Done():
	default:
		route.cancel()
	}
}
func (s *Server) startSessionCall(e wire.Event) {
	f := *e.Session
	select {
	case s.callSlots <- struct{}{}:
	default:
		select {
		case s.sessionRejections <- f.ID:
		default:
			s.log.Warn("provider session rejection queue is full")
		}
		return
	}
	ctx, cancel := session.Budget(s.ctx, e.DeadlineMS)
	down, err := session.New(ctx, f.ID, f.Mode, false, func(ctx context.Context, f session.Frame) error {
		return s.process.Send(ctx, wire.Event{Op: "session", CallID: f.ID, Session: &f})
	})
	if err != nil {
		cancel()
		<-s.callSlots
		return
	}
	s.mu.Lock()
	s.outgoingSessions[f.ID] = down
	s.mu.Unlock()
	go func() {
		defer func() { cancel(); s.mu.Lock(); delete(s.outgoingSessions, f.ID); s.mu.Unlock(); <-s.callSlots }()
		fail := func(err error) { _ = down.Finish(err.Error()) }
		if e.Address == "" || e.Body == nil {
			fail(errors.New("invalid_call"))
			return
		}
		s.mu.Lock()
		caller := s.sessionCaller
		s.mu.Unlock()
		if caller == nil {
			fail(errors.New("calls_off"))
			return
		}
		up, err := caller(down.Context(), Outbound{Address: e.Address, Body: *e.Body}, f.Mode)
		if err != nil {
			fail(err)
			return
		}
		defer up.Close()
		if err = down.Accept(); err != nil {
			return
		}
		if f.Mode == session.Duplex {
			go func() {
				_, err := io.Copy(up, down)
				if err == nil {
					err = up.CloseWrite()
				}
				if err != nil {
					up.Abort(err)
					down.Abort(err)
				}
			}()
		}
		_, err = io.Copy(down, up)
		if err == nil {
			err = up.Wait()
		}
		if err != nil {
			fail(err)
		} else {
			_ = down.Finish("")
		}
	}()
}
func (s *Server) sessionCallError(id, reason string) {
	ctx, cancel := context.WithTimeout(s.ctx, time.Second)
	defer cancel()
	f := session.Frame{Version: 1, ID: id, Op: "close", Error: reason}
	_ = s.process.Send(ctx, wire.Event{Op: "session", CallID: id, Session: &f})
}

// SetSessionCaller installs application consent and routing for nested sessions.
func (s *Server) SetSessionCaller(caller SessionCaller) {
	s.mu.Lock()
	s.sessionCaller = caller
	s.mu.Unlock()
}

func (s *Server) rejectSessionCalls() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case id := <-s.sessionRejections:
			s.sessionCallError(id, "provider_busy")
		}
	}
}
