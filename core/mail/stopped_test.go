package mail

import (
	"context"
	"errors"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/transport"
)

// errQuiet is the error of a send that got no answer.
var errQuiet = errors.New("the relay gave no answer in time")

// quietAfter is a relay that takes the first took wraps, and then answers
// no send.
type quietAfter struct {
	took  int
	sends int
}

func (*quietAfter) Name() string { return "ws://quiet" }
func (q *quietAfter) Send(context.Context, nostr.Event) error {
	q.sends++
	if q.sends > q.took {
		return errQuiet
	}
	return nil
}
func (*quietAfter) Fetch(context.Context, nostr.Filter) (transport.Batch, error) {
	return transport.Batch{}, nil
}

// A relay that stops answering partway through a sync gets no more sends
// in that sync: each would wait in vain. The sync says how many wraps it
// took, and the wraps that did not go go at the next sync (#465).
func TestASyncStopsAtARelayThatStopsAnswering(t *testing.T) {
	m, _, _, _ := openMail(t, nil)
	m.Unanswered = func(err error) bool { return errors.Is(err, errQuiet) }
	for range 3 {
		if _, err := m.Send(context.Background(), []nostr.PubKey{keys.Generate().Public}, "hello"); err != nil {
			t.Fatal(err)
		}
	}
	quiet := &quietAfter{took: 2}
	_, err := m.Sync(context.Background(), quiet)
	stopped, ok := errors.AsType[*StoppedError](err)
	if !ok || stopped.Sent != 2 || stopped.Waiting < 3 || quiet.sends != 3 {
		t.Fatalf("the sync gave %v after %d sends, want it to stop at the first send that got no answer, after 2 wraps", err, quiet.sends)
	}
	again := &quietAfter{took: 100}
	if _, err := m.Sync(context.Background(), again); err != nil {
		t.Fatal(err)
	}
	if again.sends != stopped.Waiting-2 {
		t.Errorf("the next sync sent %d wraps, want the %d that did not go", again.sends, stopped.Waiting-2)
	}
}
