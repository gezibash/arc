package citizen

import (
	"context"
	"errors"
	"strings"
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

// unheardAt is the answer of a relay where no provider listens.
func unheardAt(relay string) error {
	return transport.NoListener(errors.New(relay + ": msg: mute: no one was listening for this"))
}

// When the deadline of the caller cuts off a try after one that no one
// heard, the call gives the try before it, which names the relay of the
// provider, never the context error of the caller (#453). The test
// cancels the context from inside the tries, so no timing decides it.
func TestADeadlineInARetryGivesTheUnheardResult(t *testing.T) {
	for _, where := range []string{"in a try", "between tries"} {
		ctx, cancel := context.WithCancel(context.Background())
		tries := 0
		_, err := retryUnheard(ctx, func() (int, error) {
			tries++
			if tries == 1 {
				if where == "between tries" {
					cancel()
				}
				return 0, unheardAt("ws://provider")
			}
			cancel()
			return 0, &transport.NotSubmittedError{Err: ctx.Err()}
		})
		cancel()
		if err == nil || !strings.Contains(err.Error(), "ws://provider") || errors.Is(err, context.Canceled) {
			t.Errorf("a deadline %s gave %v, want the unheard try at the relay of the provider", where, err)
		}
	}
}

// A try that submitted its request keeps its own result when the deadline
// cuts it off: its outcome is unknown, and the call never says that no one
// got it (#436).
func TestADeadlineAfterASubmittedTryKeepsItsResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tries := 0
	uncertain := errors.New("call outcome unknown via ws://provider; the operation may have executed")
	_, err := retryUnheard(ctx, func() (int, error) {
		tries++
		if tries == 1 {
			return 0, unheardAt("ws://provider")
		}
		cancel()
		return 0, uncertain
	})
	if !errors.Is(err, uncertain) || tries != 2 {
		t.Errorf("a submitted try cut off by the deadline gave %v after %d tries, want its unknown outcome", err, tries)
	}
}

// A try that cannot end before the deadline of the caller is not started:
// the deadline would cut it off after its request went out, with an
// unknown outcome, where the try before it says that no one heard. The
// clock of the test moves 2 s at each try, and the caller has 5 s, so the
// third try would end after the deadline.
func TestATryThatCannotEndBeforeTheDeadlineIsNotStarted(t *testing.T) {
	now := time.Now()
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = time.Now })
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(5*time.Second))
	defer cancel()
	tries := 0
	_, err := retryUnheard(ctx, func() (int, error) {
		tries++
		now = now.Add(2 * time.Second)
		if deadline, _ := ctx.Deadline(); now.After(deadline) {
			return 0, errors.New("call outcome unknown via ws://caller; the operation may have executed")
		}
		return 0, unheardAt("ws://provider")
	})
	if tries != 2 || !strings.Contains(err.Error(), "ws://provider") {
		t.Errorf("after %d tries the call gave %v, want 2 tries and the unheard try at the relay of the provider", tries, err)
	}
}

// A try that could end before the deadline starts. When it then takes
// longer, and the deadline cuts it off after its request went out, the
// call still says that its outcome is unknown: arc never hides it behind
// the try that no one heard (#455).
func TestATryThatStartedAndWasCutOffKeepsItsUnknownOutcome(t *testing.T) {
	now := time.Now()
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = time.Now })
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(5*time.Second))
	defer cancel()
	tries := 0
	uncertain := errors.New("call outcome unknown via ws://caller; the operation may have executed")
	_, err := retryUnheard(ctx, func() (int, error) {
		tries++
		if tries == 1 {
			now = now.Add(time.Second)
			return 0, unheardAt("ws://provider")
		}
		now = now.Add(5 * time.Second)
		return 0, uncertain
	})
	if tries != 2 || !errors.Is(err, uncertain) {
		t.Errorf("after %d tries the call gave %v, want 2 tries and the unknown outcome", tries, err)
	}
}
