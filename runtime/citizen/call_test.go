package citizen

import (
	"context"
	"errors"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/transport"
)

type path struct {
	attempts int
	executed *int
	err      error
	// unheard is the number of first attempts that no one hears.
	unheard int
}

func (*path) Name() string                            { return "test" }
func (*path) Send(context.Context, nostr.Event) error { return nil }
func (*path) Fetch(context.Context, nostr.Filter) (transport.Batch, error) {
	return transport.Batch{}, nil
}
func (p *path) Exchange(context.Context, nostr.Event, nostr.Filter, func(nostr.Event) bool) (nostr.Event, error) {
	p.attempts++
	if p.attempts <= p.unheard {
		return nostr.Event{}, transport.NoListener(errors.New("msg: mute: no one was listening for this"))
	}
	if p.executed != nil {
		*p.executed++
	}
	return nostr.Event{}, p.err
}

func TestLostReplyDoesNotRepeatMutation(t *testing.T) {
	count := 0
	first := &path{executed: &count, err: errors.New("reply lost")}
	second := &path{executed: &count, err: errors.New("reply lost")}
	_, _, _, err := LiveCall(context.Background(), keys.Generate(), keys.Generate().Public, call.Request{Body: "increment"}, []transport.Transport{first, second}, time.Second)
	if err == nil || count != 1 || second.attempts != 0 {
		t.Fatalf("err=%v executions=%d second attempts=%d", err, count, second.attempts)
	}
}

func TestFailoverBeforeSubmission(t *testing.T) {
	first := &path{err: &transport.NotSubmittedError{Err: errors.New("connection refused")}}
	second := &path{err: errors.New("reply lost")}
	_, _, _, _ = LiveCall(context.Background(), keys.Generate(), keys.Generate().Public, call.Request{}, []transport.Transport{first, second}, time.Second)
	if first.attempts != 1 || second.attempts != 1 {
		t.Fatalf("attempts=%d,%d", first.attempts, second.attempts)
	}
}

// While the relay of a provider has no watch for it, as for a few seconds
// after the relay ends the watch, no one hears a call. The call tries
// again, and reaches the provider once it watches again (#424).
func TestACallThatNoOneHeardTriesAgain(t *testing.T) {
	p := &path{unheard: 2}
	if _, _, _, err := LiveCall(context.Background(), keys.Generate(), keys.Generate().Public, call.Request{}, []transport.Transport{p}, 10*time.Second); err != nil || p.attempts != 3 {
		t.Fatalf("err=%v attempts=%d, want the third attempt to work", err, p.attempts)
	}
}

// A try after a call that no one heard is safe, but a lost reply after it
// is an unknown outcome, and the call never runs again.
func TestALostReplyAfterAnUnheardCallIsNotRepeated(t *testing.T) {
	count := 0
	p := &path{unheard: 1, executed: &count, err: errors.New("reply lost")}
	_, _, _, err := LiveCall(context.Background(), keys.Generate(), keys.Generate().Public, call.Request{Body: "increment"}, []transport.Transport{p}, 10*time.Second)
	if err == nil || count != 1 || p.attempts != 2 {
		t.Fatalf("err=%v executions=%d attempts=%d, want one execution", err, count, p.attempts)
	}
}
