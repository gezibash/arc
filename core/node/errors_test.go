package node_test

import (
	"context"
	"errors"
	"testing"

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
