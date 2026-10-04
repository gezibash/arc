package gate_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/core/wire"
	"github.com/gezibash/arc/runtime/gate"
)

const friend, stranger = "aa", "bb"

// program is a fake app program. It keeps what it gets.
type program struct {
	mu    sync.Mutex
	got   []wire.Event
	lines chan wire.Event
}

func (p *program) Send(_ context.Context, e wire.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, e)
	return nil
}
func (p *program) Lines() <-chan wire.Event { return p.lines }
func (p *program) received() []wire.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]wire.Event(nil), p.got...)
}

func start(t *testing.T) (*program, *gate.Gate) {
	t.Helper()
	p := &program{lines: make(chan wire.Event, 4)}
	g := gate.New(p, func(from string) bool { return from == friend })
	t.Cleanup(func() { close(p.lines) })
	return p, g
}

func next(t *testing.T, g *gate.Gate) wire.Event {
	t.Helper()
	select {
	case e := <-g.Lines():
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("the gate wrote no line")
		return wire.Event{}
	}
}

func frame(op string) *session.Frame {
	return &session.Frame{Version: 1, ID: "0000000000000000000000000000000000000000000000000000000000000001", Op: op, Mode: session.Duplex}
}

func TestAnAllowedCallerReachesTheProgram(t *testing.T) {
	p, g := start(t)
	ctx := context.Background()
	if err := g.Send(ctx, wire.Event{Op: "request", RequestID: "r1", From: friend, Message: new("hi")}); err != nil {
		t.Fatal(err)
	}
	if got := p.received(); len(got) != 1 || got[0].RequestID != "r1" {
		t.Fatalf("the program got %+v", got)
	}
	p.lines <- wire.Event{Op: "reply", RequestID: "r1", Reply: new("hello")}
	if e := next(t, g); e.Op != "reply" || *e.Reply != "hello" {
		t.Fatalf("the answer of the program: %+v", e)
	}
}

func TestARefusedRequestNeverReachesTheProgram(t *testing.T) {
	p, g := start(t)
	if err := g.Send(context.Background(), wire.Event{Op: "request", RequestID: "r2", From: stranger, Message: new("hi")}); err != nil {
		t.Fatal(err)
	}
	e := next(t, g)
	if e.Op != "error" || e.RequestID != "r2" || e.Error != gate.Refused {
		t.Fatalf("the refusal: %+v", e)
	}
	if got := p.received(); len(got) != 0 {
		t.Fatalf("the program got %+v", got)
	}
}

// A refused session gets a close frame that the session protocol accepts.
// Its later frames do not reach the program.
func TestARefusedSessionNeverReachesTheProgram(t *testing.T) {
	p, g := start(t)
	ctx := context.Background()
	open := frame("open")
	if err := g.Send(ctx, wire.Event{Op: "session", RequestID: open.ID, From: stranger, Session: open}); err != nil {
		t.Fatal(err)
	}
	e := next(t, g)
	if e.Op != "session" || e.RequestID != open.ID || e.Session.Op != "close" || e.Session.Error != gate.Refused {
		t.Fatalf("the refusal: %+v", e)
	}
	if err := e.Session.Validate(); err != nil {
		t.Fatalf("the close frame is not valid: %v", err)
	}
	cancel := &session.Frame{Version: 1, ID: open.ID, Op: "cancel"}
	if err := g.Send(ctx, wire.Event{Op: "session", RequestID: open.ID, Session: cancel}); err != nil {
		t.Fatal(err)
	}
	if got := p.received(); len(got) != 0 {
		t.Fatalf("the program got %+v", got)
	}
}

func TestAnAllowedSessionAndTheProgramsOwnSessionsPass(t *testing.T) {
	p, g := start(t)
	ctx := context.Background()
	open := frame("open")
	if err := g.Send(ctx, wire.Event{Op: "session", RequestID: open.ID, From: friend, Session: open}); err != nil {
		t.Fatal(err)
	}
	// A frame of a session that the program opened has a call_id, and no
	// caller key.
	accept := &session.Frame{Version: 1, ID: open.ID, Op: "accept", Mode: session.Duplex}
	if err := g.Send(ctx, wire.Event{Op: "session", CallID: accept.ID, Session: accept}); err != nil {
		t.Fatal(err)
	}
	if got := p.received(); len(got) != 2 {
		t.Fatalf("the program got %d lines, want 2", len(got))
	}
}

// After the program stops, a refusal fails at once, and Lines closes.
func TestARefusalAfterTheProgramStopsDoesNotHang(t *testing.T) {
	p := &program{lines: make(chan wire.Event)}
	g := gate.New(p, func(string) bool { return false })
	close(p.lines)
	if _, open := <-g.Lines(); open {
		t.Fatal("Lines did not close")
	}
	done := make(chan error, 1)
	go func() {
		done <- g.Send(context.Background(), wire.Event{Op: "request", RequestID: "r3", From: stranger})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a refusal after the stop reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the refusal hung after the program stopped")
	}
}
