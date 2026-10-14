package ble

import (
	"context"
	"errors"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/transport"
)

// Listener accepts successive connections from one trusted citizen. Each Watch
// belongs to one connection and ends on disconnect. It never replays Send.
type Listener struct {
	start   chan struct{}
	once    sync.Once
	ctx     context.Context
	cancel  context.CancelFunc
	config  DirectConfig
	mu      sync.Mutex
	current *Direct
	changed chan struct{}
	err     error
	done    chan struct{}
}

func Listen(ctx context.Context, c DirectConfig) (*Listener, error) {
	if err := directSupported(); err != nil {
		return nil, err
	}
	if c.Signer == nil || c.Store == nil || c.Share == nil || c.Peer == (nostr.PubKey{}) {
		return nil, errors.New("bluetooth: signer, peer, store and share policy are required")
	}
	if _, err := normalize(Options{Adapter: c.Adapter, Timeout: operationTimeout}); err != nil {
		return nil, err
	}
	if c.Address != "" {
		return nil, errors.New("bluetooth: listener must not select an address")
	}
	return listen(ctx, c, OpenDirect), nil
}

func listen(ctx context.Context, c DirectConfig, open func(context.Context, DirectConfig) (*Direct, error)) *Listener {
	ctx, cancel := context.WithCancel(ctx)
	l := &Listener{ctx: ctx, cancel: cancel, config: c, changed: make(chan struct{}), done: make(chan struct{}), start: make(chan struct{})}
	go func() {
		defer close(l.done)
		select {
		case <-l.start:
		case <-ctx.Done():
			return
		}
		for ctx.Err() == nil {
			d, err := open(ctx, c)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
					continue
				}
				// Bluetooth that was switched off comes back; wait for it.
				if errors.Is(err, errAdapterOff) && ctx.Err() == nil {
					select {
					case <-ctx.Done():
						return
					case <-time.After(2 * time.Second):
					}
					continue
				}
				// A central that dropped during setup sent no ARC data; wait for
				// its next connection. A central that proved another identity is
				// refused, and the listener waits for the expected one: anyone in
				// radio range can send a wrong proof, so it must not stop the
				// listener. The longer pause bounds how often that can repeat.
				if (retryable(err) || errors.Is(err, errIdentity)) && ctx.Err() == nil {
					pause := 250 * time.Millisecond
					if errors.Is(err, errIdentity) {
						pause = time.Second
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(pause):
					}
					continue
				}
				l.mu.Lock()
				l.err = err
				close(l.changed)
				l.mu.Unlock()
				return
			}
			l.mu.Lock()
			l.current = d
			close(l.changed)
			l.changed = make(chan struct{})
			l.mu.Unlock()
			<-d.ctx.Done()
			_ = d.Close()
			l.mu.Lock()
			l.current = nil
			close(l.changed)
			l.changed = make(chan struct{})
			l.mu.Unlock()
			// Give BlueZ time to remove the old registration. No application operation
			// is carried into this new connection.
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()
	return l
}
func (l *Listener) Name() string { return "ble:" + l.config.Peer.Hex() }
func (l *Listener) connection(ctx context.Context, wait bool) (*Direct, error) {
	if wait {
		l.once.Do(func() { close(l.start) })
	}
	for {
		l.mu.Lock()
		d, err, changed := l.current, l.err, l.changed
		l.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if d != nil && d.ctx.Err() == nil {
			return d, nil
		}
		if !wait {
			return nil, ErrDisconnected
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.ctx.Done():
			return nil, ErrDisconnected
		case <-changed:
		}
	}
}
func (l *Listener) Send(ctx context.Context, e nostr.Event) error {
	d, err := l.connection(ctx, false)
	if err != nil {
		return err
	}
	return d.Send(ctx, e)
}
func (l *Listener) Fetch(ctx context.Context, f nostr.Filter) (transport.Batch, error) {
	d, err := l.connection(ctx, false)
	if err != nil {
		return transport.Batch{}, err
	}
	return d.Fetch(ctx, f)
}
func (l *Listener) Watch(ctx context.Context, f nostr.Filter) (<-chan nostr.Event, error) {
	d, err := l.connection(ctx, true)
	if err != nil {
		return nil, err
	}
	return d.Watch(ctx, f)
}
func (l *Listener) Exchange(ctx context.Context, e nostr.Event, f nostr.Filter, match func(nostr.Event) bool) (nostr.Event, error) {
	d, err := l.connection(ctx, false)
	if err != nil {
		return nostr.Event{}, &transport.NotSubmittedError{Err: err}
	}
	return d.Exchange(ctx, e, f, match)
}
func (l *Listener) Close() error { l.cancel(); <-l.done; return nil }
