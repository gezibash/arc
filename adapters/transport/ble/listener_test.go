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

func TestListenerWaitsAfterDroppedSetup(t *testing.T) {
	first, peer, _, key := pair(t)
	var opens atomic.Int32
	l := listen(t.Context(), first.config, func(ctx context.Context, _ DirectConfig) (*Direct, error) {
		switch opens.Add(1) {
		case 1:
			return nil, linkError{ErrDisconnected}
		case 2:
			return first, nil
		default:
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	defer func() { first.Close(); l.Close() }()
	ch, err := l.Watch(t.Context(), nostr.Filter{Kinds: []nostr.Kind{1}})
	if err != nil {
		t.Fatalf("a dropped setup ended the listener: %v", err)
	}
	e := signed(t, key, 1, "after a dropped setup")
	if err = peer.Send(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got.ID != e.ID {
			t.Fatal("wrong event")
		}
	case <-time.After(time.Second):
		t.Fatal("watch not ready")
	}
}

// A wrong proof from a stranger in radio range must not stop the listener for
// the expected peer. Seen on the Pis: one wrong key stopped advertising.
func TestListenerKeepsListeningAfterWrongIdentity(t *testing.T) {
	first, peer, _, key := pair(t)
	var opens atomic.Int32
	l := listen(t.Context(), first.config, func(ctx context.Context, _ DirectConfig) (*Direct, error) {
		switch opens.Add(1) {
		case 1:
			return nil, errIdentity
		case 2:
			return first, nil
		default:
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	defer func() { first.Close(); l.Close() }()
	ch, err := l.Watch(t.Context(), nostr.Filter{Kinds: []nostr.Kind{1}})
	if err != nil {
		t.Fatalf("a wrong identity stopped the listener: %v", err)
	}
	e := signed(t, key, 1, "after a wrong identity")
	if err = peer.Send(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got.ID != e.ID {
			t.Fatal("wrong event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch not ready")
	}
}

// Seen on the Pis: after Bluetooth went off and on, the listener had stopped.
func TestListenerWaitsForAdapterPower(t *testing.T) {
	first, peer, _, key := pair(t)
	var opens atomic.Int32
	l := listen(t.Context(), first.config, func(ctx context.Context, _ DirectConfig) (*Direct, error) {
		switch opens.Add(1) {
		case 1:
			return nil, errAdapterOff
		case 2:
			return first, nil
		default:
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	defer func() { first.Close(); l.Close() }()
	ch, err := l.Watch(t.Context(), nostr.Filter{Kinds: []nostr.Kind{1}})
	if err != nil {
		t.Fatalf("a powered-off adapter stopped the listener: %v", err)
	}
	e := signed(t, key, 1, "after power returned")
	if err = peer.Send(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got.ID != e.ID {
			t.Fatal("wrong event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch not ready")
	}
}

func TestListenerStopsOnConfigurationError(t *testing.T) {
	var opens atomic.Int32
	bad := errors.New("bluetooth: adapter hci9 is missing")
	l := listen(t.Context(), DirectConfig{}, func(context.Context, DirectConfig) (*Direct, error) {
		opens.Add(1)
		return nil, bad
	})
	defer l.Close()
	if _, err := l.Watch(t.Context(), nostr.Filter{}); !errors.Is(err, bad) {
		t.Fatalf("configuration error: %v", err)
	}
	if opens.Load() != 1 {
		t.Fatalf("retried a configuration error %d times", opens.Load())
	}
}

func TestRetryable(t *testing.T) {
	for err, want := range map[error]bool{
		linkError{ErrDisconnected}:           true,
		linkError{errors.New("bus refused")}: true,
		errIdentity:                          false,
		linkError{errIdentity}:               false,
		linkError{errRejected}:               false,
		linkError{context.DeadlineExceeded}:  false,
		ErrDisconnected:                      false,
	} {
		if got := retryable(err); got != want {
			t.Errorf("%v: retryable %v, want %v", err, got, want)
		}
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
