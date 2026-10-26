package node_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
)

type failedStore struct{ err error }

func (s failedStore) Save(nostr.Event) (store.Result, error)    { return store.Result{}, s.err }
func (s failedStore) Query(nostr.Filter) ([]nostr.Event, error) { return nil, s.err }
func (s failedStore) Has(nostr.ID) (bool, error)                { return false, s.err }

type oneEvent struct{ event nostr.Event }

func (oneEvent) Name() string                            { return "test live transport" }
func (oneEvent) Send(context.Context, nostr.Event) error { return nil }
func (s oneEvent) Fetch(context.Context, nostr.Filter) (transport.Batch, error) {
	return transport.Batch{Events: []nostr.Event{s.event}}, nil
}
func (s oneEvent) Watch(context.Context, nostr.Filter) (<-chan nostr.Event, error) {
	ch := make(chan nostr.Event, 1)
	ch <- s.event
	close(ch)
	return ch, nil
}

func TestPersistenceFailureIsReportedByPullAndWatch(t *testing.T) {
	failure := errors.New("disk unavailable")
	n := &node.Node{Store: failedStore{failure}}
	source := oneEvent{event: nostr.Event{Content: "must be saved"}}
	if err := node.Unreached(n.Pull(context.Background(), nostr.Filter{}, []transport.Transport{source})); !errors.Is(err, failure) {
		t.Fatalf("pull hid persistence failure: %v", err)
	}
	ch, err := n.Watch(context.Background(), nostr.Filter{}, source)
	if err != nil {
		t.Fatal(err)
	}
	received, ok := <-ch
	if !ok || !errors.Is(received.Err, failure) {
		t.Fatalf("watch result=%+v open=%v", received, ok)
	}
	if _, ok := <-ch; ok {
		t.Fatal("failed watch continued")
	}
}

// silent is a relay that takes a request and never answers, until the
// caller stops it.
type silent struct{}

func (silent) Name() string                            { return "silent relay" }
func (silent) Send(context.Context, nostr.Event) error { return nil }
func (silent) Fetch(ctx context.Context, _ nostr.Filter) (transport.Batch, error) {
	<-ctx.Done()
	return transport.Batch{}, ctx.Err()
}

type keepsAll struct{}

func (keepsAll) Save(nostr.Event) (store.Result, error) {
	return store.Result{Outcome: store.Stored}, nil
}
func (keepsAll) Query(nostr.Filter) ([]nostr.Event, error) { return nil, nil }
func (keepsAll) Has(nostr.ID) (bool, error)                { return false, nil }

// A relay that never answers stalls a pull for at most PullGrace after
// another relay answered, wherever it is in the list (#439). The pull keeps
// what the other relay gave, and says that the silent one gave no answer.
func TestASilentRelayStallsAPullOnlyForTheGrace(t *testing.T) {
	n := &node.Node{Store: keepsAll{}}
	answers := oneEvent{event: nostr.Event{Content: "kept"}}
	for _, order := range [][]transport.Transport{{silent{}, answers}, {answers, silent{}}} {
		start := time.Now()
		reports, errs := n.Pull(context.Background(), nostr.Filter{}, order)
		if took := time.Since(start); took > node.PullGrace+time.Second {
			t.Errorf("a pull with a silent relay took %s", took)
		}
		if len(reports) != 1 || reports[0].Received != 1 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "silent relay gave no answer within") {
			t.Errorf("the pull gave %+v and %v, want the event of one relay and the silence of the other", reports, errs)
		}
	}
}
