package citizen

import (
	"context"
	"errors"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/call"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/transport"
)

type path struct {
	attempts int
	executed *int
	err      error
}

func (*path) Name() string                            { return "test" }
func (*path) Send(context.Context, nostr.Event) error { return nil }
func (*path) Fetch(context.Context, nostr.Filter) (transport.Batch, error) {
	return transport.Batch{}, nil
}
func (p *path) Exchange(context.Context, nostr.Event, nostr.Filter, func(nostr.Event) bool) (nostr.Event, error) {
	p.attempts++
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
