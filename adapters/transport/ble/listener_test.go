package ble

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func TestListenerNewConnectionDoesNotResumeOldWatch(t *testing.T) {
	first, peer1, _, _ := pair(t)
	second, peer2, _, key2 := pair(t)
	var opens atomic.Int32
	l := listen(t.Context(), first.config, func(ctx context.Context, _ DirectConfig) (*Direct, error) {
		switch opens.Add(1) {
		case 1:
			return first, nil
		case 2:
			return second, nil
		default:
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	defer func() { first.Close(); second.Close(); l.Close() }()
	ch, err := l.Watch(t.Context(), nostr.Filter{Kinds: []nostr.Kind{1}})
	if err != nil {
		t.Fatal(err)
	}
	peer1.Close()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("old watch did not end")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not close watch")
	}
	next, err := l.Watch(t.Context(), nostr.Filter{Kinds: []nostr.Kind{1}})
	if err != nil {
		t.Fatal(err)
	}
	e := signed(t, key2, 1, "new connection")
	if err = peer2.Send(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-next:
		if got.ID != e.ID {
			t.Fatal("wrong event")
		}
	case <-time.After(time.Second):
		t.Fatal("new watch not ready")
	}
}

func TestListenerDoesNotQueueSendBeforeConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var opens atomic.Int32
	l := listen(ctx, DirectConfig{}, func(ctx context.Context, _ DirectConfig) (*Direct, error) {
		opens.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err := l.Send(ctx, nostr.Event{}); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("send claimed success: %v", err)
	}
	if opens.Load() != 0 {
		t.Fatal("publishing before watches started the radio")
	}
	cancel()
	l.Close()
}
