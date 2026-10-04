// Package session owns ARC interaction modes and live session behavior.
// Connections supply Send; authenticated peers supply Receive. Application
// state (a REPL environment or SQL transaction) belongs to the handler.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
)

type Mode string

const (
	RequestReply Mode = "request_reply"
	ServerStream Mode = "server_stream"
	Duplex       Mode = "duplex"
	MaxChunk          = 16 * 1024
	Version           = 1
)

func (m Mode) Valid() bool { return m == RequestReply || m == ServerStream || m == Duplex }

// Supports treats an omitted declaration as the existing request/reply contract.
func Supports(modes []Mode, mode Mode) bool {
	if len(modes) == 0 {
		return mode == RequestReply
	}
	return slices.Contains(modes, mode)
}

var (
	ErrProtocol     = errors.New("session_protocol_error")
	ErrUnsupported  = errors.New("interaction_not_supported")
	ErrClosed       = errors.New("session_closed")
	ErrDisconnected = errors.New("session_disconnected; outcome may be unknown")
)

// Frame is the shared protocol over event transports and provider I/O.
// Open carries initial request metadata in its enclosing call/provider message.
type Frame struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Op      string `json:"op"`
	Mode    Mode   `json:"mode,omitempty"`
	Seq     uint64 `json:"seq,omitempty"`
	Data    []byte `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

func ID() string { var b [32]byte; rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func (f Frame) Validate() error {
	id, err := hex.DecodeString(f.ID)
	if err != nil || len(id) != 32 || f.Version != Version || len(f.Data) > MaxChunk || len(f.Error) > 1024 {
		return ErrProtocol
	}
	switch f.Op {
	case "open", "accept":
		if !f.Mode.Valid() || f.Seq != 0 || len(f.Data) != 0 || f.Error != "" {
			return ErrProtocol
		}
	case "data":
		if f.Seq == 0 || len(f.Data) == 0 || f.Mode != "" || f.Error != "" {
			return ErrProtocol
		}
	case "ack", "end":
		if f.Seq == 0 || len(f.Data) != 0 || f.Mode != "" || f.Error != "" {
			return ErrProtocol
		}
	case "close", "cancel":
		if f.Seq != 0 || len(f.Data) != 0 || f.Mode != "" {
			return ErrProtocol
		}
	default:
		return ErrProtocol
	}
	return nil
}

// Send must honor its context. A successful send is transport acceptance,
// not evidence that the peer executed or consumed the operation.
type Send func(context.Context, Frame) error

type Stream struct {
	id          string
	mode        Mode
	initiator   bool
	send        Send
	ctx         context.Context
	cancel      context.CancelCauseFunc
	ready       chan struct{}
	input       chan Frame
	ack         chan uint64
	gate        chan struct{}
	readMu      sync.Mutex
	remainder   []byte
	mu          sync.Mutex
	accepted    bool
	sent        uint64
	received    uint64
	consumed    uint64
	inputEnded  bool
	outputEnded bool
	terminal    error
	doneOnce    sync.Once
}

// New starts the local endpoint. The caller sends open or calls Accept, and
// feeds authenticated peer frames to Receive. Loss ends this session; v1 never
// replays commands or resumes a replacement connection implicitly.
func New(ctx context.Context, id string, mode Mode, initiator bool, send Send) (*Stream, error) {
	if err := (Frame{Version: Version, ID: id, Op: "open", Mode: mode}).Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	s := &Stream{id: id, mode: mode, initiator: initiator, send: send, ctx: ctx, cancel: cancel,
		ready: make(chan struct{}), input: make(chan Frame, 1), ack: make(chan uint64, 1), gate: make(chan struct{}, 1)}
	s.inputEnded = !initiator && mode != Duplex
	s.outputEnded = initiator && mode != Duplex
	return s, nil
}
func (s *Stream) ID() string               { return s.id }
func (s *Stream) Mode() Mode               { return s.mode }
func (s *Stream) Context() context.Context { return s.ctx }
func (s *Stream) frame(op string) Frame    { return Frame{Version: Version, ID: s.id, Op: op} }
func (s *Stream) WaitReady() error {
	s.mu.Lock()
	ready := s.accepted
	s.mu.Unlock()
	if ready {
		return nil
	}
	select {
	case <-s.ready:
		return nil
	case <-s.ctx.Done():
		s.mu.Lock()
		ready = s.accepted
		s.mu.Unlock()
		if ready {
			return nil
		}
		return s.err()
	}
}
func (s *Stream) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal != nil {
		return s.terminal
	}
	return context.Cause(s.ctx)
}
func (s *Stream) Accept() error {
	s.mu.Lock()
	if s.initiator || s.accepted {
		s.mu.Unlock()
		return ErrProtocol
	}
	s.accepted = true
	close(s.ready)
	s.mu.Unlock()
	f := s.frame("accept")
	f.Mode = s.mode
	if err := s.send(s.ctx, f); err != nil {
		s.Abort(err)
		return err
	}
	return nil
}

// Receive never blocks on application input. One unacknowledged data chunk is
// allowed per direction. A gap, unsupported input, or excess credit ends the
// session explicitly, instead of dropping data or growing a queue.
func (s *Stream) Receive(f Frame) error {
	if f.Validate() != nil || f.ID != s.id {
		return s.protocol()
	}
	s.mu.Lock()
	if s.terminal != nil || s.ctx.Err() != nil {
		s.mu.Unlock()
		return s.err()
	}
	switch f.Op {
	case "accept":
		if !s.initiator || f.Mode != s.mode {
			s.mu.Unlock()
			return s.protocol()
		}
		if !s.accepted {
			s.accepted = true
			close(s.ready)
		}
	case "data":
		if !s.accepted || s.inputEnded {
			s.mu.Unlock()
			return s.protocol()
		}
		if f.Seq <= s.received {
			s.mu.Unlock()
			return nil
		}
		if f.Seq != s.received+1 || s.received != s.consumed {
			s.mu.Unlock()
			return s.protocol()
		}
		f.Data = append([]byte(nil), f.Data...)
		s.received = f.Seq
		s.input <- f
	case "ack":
		if !s.accepted || f.Seq > s.sent {
			s.mu.Unlock()
			return s.protocol()
		}
		if f.Seq == s.sent {
			select {
			case s.ack <- f.Seq:
			default:
			}
		}
	case "end":
		if !s.accepted || f.Seq != s.received+1 {
			s.mu.Unlock()
			return s.protocol()
		}
		s.inputEnded = true
		// A sentinel wakes an idle reader. A queued data chunk is read first.
		select {
		case s.input <- f:
		default:
		}
	case "close", "cancel":
		err := error(io.EOF)
		if f.Error != "" {
			err = RemoteError(f.Error)
		} else if f.Op == "cancel" {
			err = context.Canceled
		}
		if !s.accepted && f.Error == "" && f.Op == "close" {
			s.mu.Unlock()
			return s.protocol()
		}
		s.mu.Unlock()
		s.Abort(err)
		return nil
	default:
		s.mu.Unlock()
		return s.protocol()
	}
	s.mu.Unlock()
	return nil
}
func (s *Stream) protocol() error { s.Abort(ErrProtocol); return ErrProtocol }

func (s *Stream) acquire() error {
	if err := s.err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
		if err := s.err(); err != nil {
			<-s.gate
			return err
		}
		return nil
	case <-s.ctx.Done():
		return s.err()
	}
}

// Write sends bounded chunks. Credit returns only when the peer reads a chunk.
// RequestReply has one response message and no subsequent client input.
func (s *Stream) Write(p []byte) (int, error) {
	if err := s.WaitReady(); err != nil {
		return 0, err
	}
	if err := s.acquire(); err != nil {
		return 0, err
	}
	defer func() { <-s.gate }()
	total := 0
	for len(p) > 0 {
		s.mu.Lock()
		if s.outputEnded {
			s.mu.Unlock()
			return total, ErrUnsupported
		}
		s.sent++
		seq := s.sent
		select {
		case <-s.ack:
		default:
		}
		s.mu.Unlock()
		n := min(len(p), MaxChunk)
		f := s.frame("data")
		f.Seq = seq
		f.Data = append([]byte(nil), p[:n]...)
		if err := s.send(s.ctx, f); err != nil {
			s.Abort(err)
			return total, err
		}
		for {
			select {
			case ack := <-s.ack:
				if ack != seq {
					continue
				}
			case <-s.ctx.Done():
				return total, s.err()
			}
			break
		}
		total += n
		p = p[n:]
	}
	return total, nil
}
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if len(s.remainder) > 0 {
		n := copy(p, s.remainder)
		s.remainder = s.remainder[n:]
		return n, nil
	}
	s.mu.Lock()
	ended := s.inputEnded && s.received == s.consumed
	s.mu.Unlock()
	if ended {
		return 0, io.EOF
	}
	var f Frame
	// Drain an already accepted chunk before observing final closure.
	select {
	case f = <-s.input:
	default:
		select {
		case f = <-s.input:
		case <-s.ctx.Done():
			select {
			case f = <-s.input:
			default:
				return 0, s.err()
			}
		}
	}
	if f.Op == "end" {
		return 0, io.EOF
	}
	s.mu.Lock()
	s.consumed = f.Seq
	s.mu.Unlock()
	ack := s.frame("ack")
	ack.Seq = f.Seq
	if err := s.send(s.ctx, ack); err != nil {
		s.Abort(err)
		return 0, err
	}
	n := copy(p, f.Data)
	s.remainder = f.Data[n:]
	return n, nil
}

// CloseWrite ends caller input while allowing provider output to continue.
func (s *Stream) CloseWrite() error {
	if err := s.WaitReady(); err != nil {
		return err
	}
	if err := s.acquire(); err != nil {
		return err
	}
	defer func() { <-s.gate }()
	s.mu.Lock()
	if s.outputEnded {
		s.mu.Unlock()
		return nil
	}
	s.outputEnded = true
	seq := s.sent + 1
	s.mu.Unlock()
	f := s.frame("end")
	f.Seq = seq
	if err := s.send(s.ctx, f); err != nil {
		s.Abort(err)
		return err
	}
	return nil
}

// Finish reports a provider's final status. A handler returns only after its
// writes complete. Empty successful replies are represented by close alone.
func (s *Stream) Finish(code string) error {
	if len(code) > 1024 {
		code = "session_failed"
	}
	f := s.frame("close")
	f.Error = code
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), time.Second)
	defer cancel()
	err := s.send(ctx, f)
	if err != nil {
		s.Abort(err)
	} else if code != "" {
		s.Abort(errors.New(code))
	} else {
		s.Abort(io.EOF)
	}
	return err
}

// Abort records disconnection or a terminal peer outcome without sending.
func (s *Stream) Abort(err error) {
	if err == nil {
		err = ErrClosed
	}
	s.doneOnce.Do(func() { s.mu.Lock(); s.terminal = err; s.mu.Unlock(); s.cancel(err) })
}

// Close cancels local work and sends one bounded cancellation notice.
func (s *Stream) Close() error {
	s.mu.Lock()
	finished := s.terminal != nil
	s.mu.Unlock()
	if finished {
		return nil
	}
	cause := context.Cause(s.ctx)
	if cause == nil {
		cause = context.Canceled
	}
	s.Abort(cause)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), time.Second)
	defer cancel()
	f := s.frame("cancel")
	return s.send(ctx, f)
}
func (s *Stream) String() string { return fmt.Sprintf("%s/%s", s.mode, s.id) }

// Budget caps live sessions at thirty minutes. An omitted lifetime defaults
// to two minutes; an earlier caller deadline always wins.
func Budget(ctx context.Context, deadlineMS int64) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(30 * time.Minute)
	if deadlineMS == 0 {
		deadline = time.Now().Add(2 * time.Minute)
	} else if supplied := time.UnixMilli(deadlineMS); supplied.Before(deadline) {
		deadline = supplied
	}
	return context.WithDeadline(ctx, deadline)
}

// Wait waits for the final outcome. Read EOF alone may be only a half-close.
func (s *Stream) Wait() error {
	<-s.ctx.Done()
	err := s.err()
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// RemoteError is an explicit final status from the authenticated peer.
// It is distinct from a local adapter failure with an uncertain outcome.
type RemoteError string

func (e RemoteError) Error() string { return string(e) }
func (e RemoteError) Is(target error) bool {
	return (target == ErrUnsupported || target == ErrProtocol) && string(e) == target.Error()
}
