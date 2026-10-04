// Package gate refuses the callers that an app does not allow, before their
// requests reach the app program.
//
// A gate stands between call.Server and the program. Live calls, calls that
// waited in a mailbox, and sessions all reach the program through it. The
// gate answers a refused request with access_denied, and a refused session
// with a close frame. The program sees neither.
package gate

import (
	"context"
	"errors"
	"sync"

	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/core/wire"
)

// Refused is the error code that a refused caller gets.
const Refused = "access_denied"

// Gate is a call.Client that lets only allowed callers through.
type Gate struct {
	program call.Client
	allowed func(from string) bool
	lines   chan wire.Event
	refusal chan wire.Event
	stopped chan struct{}

	mu sync.Mutex
	// shut holds the sessions that the gate refused. Their later frames do
	// not reach the program.
	shut map[string]bool
}

// New puts a gate in front of the program. allowed gets the public key of
// a caller, in hex.
func New(program call.Client, allowed func(from string) bool) *Gate {
	g := &Gate{
		program: program, allowed: allowed,
		lines: make(chan wire.Event), refusal: make(chan wire.Event),
		stopped: make(chan struct{}), shut: map[string]bool{},
	}
	go g.forward()
	return g
}

// Send passes an event to the program, unless it comes from a caller that
// the gate refuses.
func (g *Gate) Send(ctx context.Context, e wire.Event) error {
	switch {
	case e.Op == "request" && !g.allowed(e.From):
		return g.answer(ctx, wire.Event{Op: "error", RequestID: e.RequestID, Error: Refused})
	case e.Op == "session" && e.Session != nil && e.CallID == "":
		// A session that a caller opens. A session that the program opens
		// carries a call_id, and the gate lets it pass.
		id := e.Session.ID
		g.mu.Lock()
		shut := g.shut[id]
		if e.Session.Op == "open" && !shut && !g.allowed(e.From) {
			g.shut[id] = true
			shut = true
			g.mu.Unlock()
			closed := session.Frame{Version: session.Version, ID: id, Op: "close", Error: Refused}
			return g.answer(ctx, wire.Event{Op: "session", RequestID: id, Session: &closed})
		}
		if shut && (e.Session.Op == "cancel" || e.Session.Op == "close") {
			delete(g.shut, id)
		}
		g.mu.Unlock()
		if shut {
			return nil
		}
	}
	return g.program.Send(ctx, e)
}

// Lines returns the answers of the program and the refusals of the gate.
// It closes when the program stops.
func (g *Gate) Lines() <-chan wire.Event { return g.lines }

func (g *Gate) answer(ctx context.Context, e wire.Event) error {
	select {
	case g.refusal <- e:
		return nil
	case <-g.stopped:
		return errors.New("gate: the program stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Gate) forward() {
	defer close(g.lines)
	answers := g.program.Lines()
	for {
		var e wire.Event
		select {
		case one, ok := <-answers:
			if !ok {
				close(g.stopped)
				return
			}
			e = one
		case e = <-g.refusal:
		}
		g.lines <- e
	}
}
