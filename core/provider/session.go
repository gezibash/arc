package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gezibash/arc/core/provider/wire"
	"github.com/gezibash/arc/core/session"
)

// SessionHandler serves streaming and duplex interactions. It applies the same
// access checks as HandleRequest. Application state lives in this invocation.
type SessionHandler interface {
	HandleSession(context.Context, Request, *session.Stream) error
}

// SessionCaller opens another installed capability through the serving host.
// A provider can be a consumer without implementing a transport or protocol.
type SessionCaller interface {
	OpenSession(context.Context, string, string, session.Mode) (*session.Stream, error)
}
type WantsSessionCaller interface{ SetSessionCaller(SessionCaller) }

func (r *runtime) sessionSend(ctx context.Context, f session.Frame, outbound bool) error {
	e := wire.Event{Op: "session", Session: &f}
	if outbound {
		e.CallID = f.ID
	} else {
		e.RequestID = f.ID
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return r.out.write(ctx, body)
}
func (r *runtime) sessionError(id string, err error) {
	f := session.Frame{Version: 1, ID: id, Op: "close", Error: safeSessionError(r.options.Log, err)}
	ctx, cancel := wire.Budget(r.out.ctx, 0)
	defer cancel()
	_ = r.sessionSend(ctx, f, false)
}
func safeSessionError(log io.Writer, err error) string {
	var remote session.RemoteError
	if errors.As(err, &remote) {
		return remote.Error()
	}
	if errors.Is(err, session.ErrUnsupported) {
		return session.ErrUnsupported.Error()
	}
	if errors.Is(err, session.ErrDisconnected) {
		return session.ErrDisconnected.Error()
	}
	if errors.Is(err, session.ErrProtocol) {
		return session.ErrProtocol.Error()
	}
	return safeError(log, err)
}
func (r *runtime) rejectSession(id string, err error) {
	rejected := rejection{sessionID: id, err: err, done: make(chan struct{})}
	select {
	case r.rejections <- rejected:
		r.rejected = rejected.done
	case <-r.out.ctx.Done():
	default:
		r.out.abort(ErrBusy)
	}
}
func (r *runtime) dispatchSession(ctx context.Context, e wire.Event) {
	f := *e.Session
	r.requestsMu.Lock()
	stream := r.streams[f.ID]
	r.requestsMu.Unlock()
	if err := f.Validate(); err != nil {
		if stream != nil {
			stream.Abort(session.ErrProtocol)
		} else {
			r.reject(nil, ErrInvalidRequest)
		}
		return
	}
	if f.Op != "open" {
		if stream != nil {
			if err := stream.Receive(f); err != nil && !errors.Is(err, context.Canceled) {
				stream.Abort(err)
			}
		}
		return
	}
	if stream != nil {
		return
	} // The active session never opens twice.
	if e.Message == nil || e.Meta == nil || e.RequestID != f.ID {
		r.rejectSession(f.ID, ErrInvalidRequest)
		return
	}
	handler, canStream := r.handler.(SessionHandler)
	if f.Mode != session.RequestReply && !canStream {
		r.rejectSession(f.ID, session.ErrUnsupported)
		return
	}
	select {
	case r.slots <- struct{}{}:
	default:
		r.rejectSession(f.ID, ErrBusy)
		return
	}
	ctx, cancel := session.Budget(ctx, e.DeadlineMS)
	if ctx.Err() != nil {
		cancel()
		<-r.slots
		r.rejectSession(f.ID, context.DeadlineExceeded)
		return
	}
	stream, err := session.New(ctx, f.ID, f.Mode, false, func(ctx context.Context, f session.Frame) error { return r.sessionSend(ctx, f, false) })
	if err != nil {
		cancel()
		<-r.slots
		r.rejectSession(f.ID, err)
		return
	}
	r.requestsMu.Lock()
	if _, exists := r.requests[f.ID]; exists || len(r.streams) >= r.options.MaxConcurrent {
		r.requestsMu.Unlock()
		cancel()
		<-r.slots
		r.rejectSession(f.ID, ErrBusy)
		return
	}
	r.streams[f.ID] = stream
	r.requests[f.ID] = cancel
	r.requestsMu.Unlock()
	req := Request{Op: "session", RequestID: f.ID, From: e.From, Message: *e.Message, Meta: e.Meta, ArcSessionID: f.ID, Framed: true}
	prior := r.rejected
	r.group.Add(1)
	go func() {
		defer r.group.Done()
		defer func() {
			cancel()
			r.requestsMu.Lock()
			delete(r.streams, f.ID)
			delete(r.requests, f.ID)
			r.requestsMu.Unlock()
			<-r.slots
		}()
		if prior != nil {
			<-prior
		}
		if err := stream.Accept(); err != nil {
			return
		}
		err := r.handleSession(stream.Context(), handler, req, stream)
		if cause := context.Cause(stream.Context()); cause != nil && !errors.Is(cause, io.EOF) {
			err = cause
		}
		code := ""
		if err != nil {
			code = safeSessionError(r.options.Log, err)
		}
		_ = stream.Finish(code)
	}()
}
func (r *runtime) handleSession(ctx context.Context, handler SessionHandler, req Request, stream *session.Stream) (err error) {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(r.options.Log, "provider: session handler panicked: %v\n", p)
			err = ErrInternal
		}
	}()
	if stream.Mode() == session.RequestReply {
		reply, err := r.call(ctx, req)
		if err != nil {
			return err
		}
		_, err = stream.Write([]byte(reply))
		return err
	}
	return handler.HandleSession(ctx, req, stream)
}

func (r *runtime) OpenSession(ctx context.Context, address, body string, mode session.Mode) (*session.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := session.Budget(ctx, wire.Deadline(ctx))
	id := session.ID()
	stream, err := session.New(ctx, id, mode, true, func(ctx context.Context, f session.Frame) error { return r.sessionSend(ctx, f, true) })
	if err != nil {
		cancel()
		return nil, err
	}
	r.requestsMu.Lock()
	if len(r.streams) >= r.options.MaxConcurrent {
		r.requestsMu.Unlock()
		cancel()
		return nil, ErrBusy
	}
	r.streams[id] = stream
	r.requestsMu.Unlock()
	go func() {
		defer func() {
			if ctx.Err() != nil {
				_ = stream.Close()
			}
		}()
		select {
		case <-ctx.Done():
			_ = stream.Close()
		case <-stream.Context().Done():
		case <-r.stopped:
			stream.Abort(session.ErrDisconnected)
		}
		cancel()
		r.requestsMu.Lock()
		delete(r.streams, id)
		r.requestsMu.Unlock()
	}()
	f := session.Frame{Version: 1, ID: id, Op: "open", Mode: mode}
	encoded, err := json.Marshal(wire.Event{Op: "session", CallID: id, Session: &f, Address: address, Body: wire.Text(body), DeadlineMS: wire.Deadline(ctx)})
	if err == nil {
		err = r.out.write(ctx, encoded)
	}
	if err == nil {
		err = stream.WaitReady()
	}
	if err != nil {
		// Close sends the cancel notice so the host releases its side.
		_ = stream.Close()
		cancel()
		return nil, err
	}
	return stream, nil
}
