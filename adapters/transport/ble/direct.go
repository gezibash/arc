package ble

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/frame"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
)

const maxResults = 64
const maxWatches = 16
const operationTimeout = 30 * time.Second

// DirectConfig selects one explicitly trusted peer. Share must limit which local
// events that peer may fetch or watch. No store is exposed by default.
// The caller owns Store and must keep it open until Direct.Close returns.
type DirectConfig struct {
	Adapter string
	Address string // central when set; otherwise wait as a peripheral
	Signer  keys.Signer
	Peer    nostr.PubKey
	Store   *store.Store
	Share   func(nostr.Event) bool
}

// Direct is a single authenticated Bluetooth connection. It has no routing,
// automatic reconnect, replay, or forwarding. OpenDirect supplies the Linux I/O.
type Direct struct {
	ctx     context.Context
	cancel  context.CancelFunc
	conn    *secureLink
	config  DirectConfig
	gate    chan struct{}
	out     chan message
	replies chan message

	mu        sync.Mutex
	remote    map[uint64]nostr.Filter
	local     map[uint64]*subscription
	nextWatch uint64
	err       error
	next      uint64
	pending   uint64
	wg        sync.WaitGroup
}

var _ transport.Live = (*Direct)(nil)
var ErrDisconnected = errors.New("bluetooth: direct link disconnected")

type subscription struct {
	filter nostr.Filter
	events chan nostr.Event
}

type message struct {
	Sub    uint64        `json:"sub,omitempty"`
	Op     string        `json:"op"`
	ID     uint64        `json:"id,omitempty"`
	Filter *nostr.Filter `json:"filter,omitempty"`
	Frames [][]byte      `json:"frames,omitempty"`
	Error  string        `json:"error,omitempty"`
}

func newDirect(ctx context.Context, conn *secureLink, c DirectConfig) *Direct {
	ctx, cancel := context.WithCancel(ctx)
	d := &Direct{ctx: ctx, cancel: cancel, conn: conn, config: c, gate: make(chan struct{}, 1), out: make(chan message, 66), replies: make(chan message, 65), local: make(map[uint64]*subscription), remote: make(map[uint64]nostr.Filter)}
	d.wg.Add(2)
	go d.writeLoop()
	go d.readLoop()
	return d
}
func (d *Direct) Name() string { return "ble:" + d.config.Peer.Hex() }

// Err reports why the connection ended, including a lost watch.
func (d *Direct) Err() error { d.mu.Lock(); defer d.mu.Unlock(); return d.err }
func (d *Direct) stop(err error) {
	d.mu.Lock()
	if d.err == nil {
		d.err = err
	}
	d.mu.Unlock()
	d.cancel()
	_ = d.conn.Close()
}
func (d *Direct) Close() error { d.stop(ErrDisconnected); d.wg.Wait(); return nil }
func (d *Direct) enqueue(m message) error {
	if d.ctx.Err() != nil {
		return ErrDisconnected
	}
	select {
	case d.out <- m:
		return nil
	default:
		return errors.New("bluetooth: output capacity exceeded")
	}
}
func (d *Direct) writeLoop() {
	defer d.wg.Done()
	for {
		select {
		case <-d.ctx.Done():
			_ = d.conn.Close()
			return
		case m := <-d.out:
			b, err := json.Marshal(m)
			if err == nil {
				ctx, cancel := context.WithTimeout(d.ctx, operationTimeout)
				err = d.conn.send(ctx, b)
				cancel()
			}
			if err != nil {
				d.stop(err)
				return
			}
		}
	}
}
func (d *Direct) readLoop() {
	defer d.wg.Done()
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for id, sub := range d.local {
			close(sub.events)
			delete(d.local, id)
		}
	}()
	for {
		b, err := d.conn.receive(d.ctx)
		if err != nil {
			d.stop(err)
			return
		}
		var m message
		if err = json.Unmarshal(b, &m); err == nil {
			err = d.handle(m)
		}
		if err != nil {
			d.stop(err)
			return
		}
	}
}
func pack(e nostr.Event) ([][]byte, error) {
	if len(e.String()) > store.MaxEvent {
		return nil, errors.New("bluetooth: event exceeds ARC size limit")
	}
	fs, err := frame.Split(e, 1024, 0)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(fs))
	for _, f := range fs {
		b, e := frame.Encode(f)
		if e != nil {
			return nil, e
		}
		out = append(out, b)
	}
	return out, nil
}
func unpack(fs [][]byte) (nostr.Event, error) {
	if len(fs) == 0 || len(fs) > 128 {
		return nostr.Event{}, errors.New("bluetooth: invalid frame count")
	}
	var a frame.Assembler
	var result *frame.Completed
	now := time.Now()
	for _, b := range fs {
		if result != nil || len(b) > 1024 {
			return nostr.Event{}, errors.New("bluetooth: extra event frame")
		}
		f, err := frame.Decode(b)
		if err != nil {
			return nostr.Event{}, err
		}
		if f.Hops != 0 {
			return nostr.Event{}, errors.New("bluetooth: forwarding is unsupported")
		}
		result, err = a.Add(f, now)
		if err != nil {
			return nostr.Event{}, err
		}
	}
	if result == nil {
		return nostr.Event{}, errors.New("bluetooth: incomplete event")
	}
	if len(result.Event.String()) > store.MaxEvent || store.Expired(result.Event, time.Now()) {
		return nostr.Event{}, errors.New("bluetooth: oversized or expired event")
	}
	return result.Event, store.Verify(result.Event)
}
func (d *Direct) handle(m message) error {
	switch m.Op {
	case "ack", "result", "end":
		d.mu.Lock()
		pending := d.pending
		d.mu.Unlock()
		if pending == 0 || m.ID != pending {
			return errors.New("bluetooth: unsolicited reply")
		}
		select {
		case d.replies <- m:
			return nil
		default:
			return errors.New("bluetooth: reply capacity exceeded")
		}
	case "event":
		e, err := unpack(m.Frames)
		if err != nil {
			return err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		sub := d.local[m.Sub]
		// A canceled watch can still have packets in flight.
		if sub == nil {
			return nil
		}
		if !sub.filter.Matches(e) {
			return errors.New("bluetooth: event outside subscription")
		}
		select {
		case sub.events <- e:
			return nil
		default:
			return errors.New("bluetooth: slow watch consumer")
		}
	case "send":
		e, err := unpack(m.Frames)
		d.mu.Lock()
		defer d.mu.Unlock()
		matched := false
		for _, sub := range d.local {
			if sub.filter.Matches(e) {
				matched = true
			}
		}
		if err == nil && e.Kind.IsEphemeral() && !matched {
			err = errors.New("bluetooth: no live subscriber")
		}
		if err == nil && !e.Kind.IsEphemeral() {
			var r store.Result
			r, err = d.config.Store.Save(e)
			if err == nil && r.Outcome == store.Refused {
				err = errors.New(r.Reason)
			}
		}
		reply := message{Op: "ack", ID: m.ID}
		if err != nil {
			reply.Error = "event refused"
		} else {
			for _, sub := range d.local {
				if sub.filter.Matches(e) {
					select {
					case sub.events <- e:
					default:
						return errors.New("bluetooth: slow watch consumer")
					}
				}
			}
		}
		return d.enqueue(reply)
	case "fetch", "watch":
		if m.Filter == nil {
			return errors.New("bluetooth: missing filter")
		}
		filter := *m.Filter
		// Bound work before querying the store; never silently truncate a result.
		filter.Limit = maxResults + 1
		es, err := d.config.Store.Query(filter)
		if err != nil {
			return d.enqueue(message{Op: "end", ID: m.ID, Error: "query failed"})
		}
		allowed := make([]nostr.Event, 0, len(es))
		for _, e := range es {
			if d.config.Share(e) {
				allowed = append(allowed, e)
			}
		}
		if len(es) > maxResults {
			return d.enqueue(message{Op: "end", ID: m.ID, Error: "query exceeds 64 events; narrow the filter"})
		}
		if limit := m.Filter.Limit; limit > 0 && len(allowed) > limit {
			allowed = allowed[:limit]
		}
		if m.Op == "watch" {
			d.mu.Lock()
			if m.Sub == 0 || len(d.remote) >= maxWatches {
				d.mu.Unlock()
				return d.enqueue(message{Op: "end", ID: m.ID, Error: "watch capacity exceeded"})
			}
			f := *m.Filter
			d.remote[m.Sub] = f
			d.mu.Unlock()
		}
		for _, e := range allowed {
			fs, err := pack(e)
			if err != nil {
				return err
			}
			op := "result"
			if m.Op == "watch" {
				op = "event"
			}
			if err = d.enqueue(message{Op: op, ID: m.ID, Sub: m.Sub, Frames: fs}); err != nil {
				return err
			}
		}
		return d.enqueue(message{Op: "end", ID: m.ID})
	case "unwatch":
		d.mu.Lock()
		delete(d.remote, m.Sub)
		d.mu.Unlock()
		return nil
	default:
		return errors.New("bluetooth: unknown operation")
	}
}
func (d *Direct) request(ctx context.Context, m message) (transport.Batch, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return transport.Batch{}, err
	}
	select {
	case d.gate <- struct{}{}:
	case <-ctx.Done():
		return transport.Batch{}, ctx.Err()
	case <-d.ctx.Done():
		return transport.Batch{}, ErrDisconnected
	}
	defer func() { <-d.gate }()
	if err := ctx.Err(); err != nil {
		return transport.Batch{}, err
	}
	d.next++
	m.ID = d.next
	d.mu.Lock()
	d.pending = m.ID
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.pending = 0; d.mu.Unlock() }()
	if err := d.enqueue(m); err != nil {
		d.stop(err)
		return transport.Batch{}, err
	}
	var batch transport.Batch
	for {
		select {
		case <-ctx.Done():
			d.stop(ctx.Err())
			return batch, ctx.Err()
		case <-d.ctx.Done():
			return batch, ErrDisconnected
		case r := <-d.replies:
			if r.ID != m.ID {
				d.stop(errors.New("bluetooth: unexpected reply"))
				return batch, ErrDisconnected
			}
			if r.Error != "" {
				return batch, fmt.Errorf("bluetooth: %s", r.Error)
			}
			if (m.Op == "send" && r.Op == "ack") || ((m.Op == "fetch" || m.Op == "watch") && r.Op == "end") {
				return batch, nil
			}
			if m.Op != "fetch" || r.Op != "result" {
				d.stop(errors.New("bluetooth: invalid response operation"))
				return batch, ErrDisconnected
			}
			e, err := unpack(r.Frames)
			if err != nil {
				d.stop(err)
				return batch, err
			}
			if m.Filter == nil || !m.Filter.Matches(e) || len(batch.Events) >= maxResults {
				d.stop(errors.New("bluetooth: invalid query response"))
				return batch, ErrDisconnected
			}
			batch.Events = append(batch.Events, e)
		}
	}
}

// Send acknowledges remote store acceptance, not recipient processing.
func (d *Direct) Send(ctx context.Context, e nostr.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.Verify(e); err != nil {
		return err
	}
	fs, err := pack(e)
	if err != nil {
		return err
	}
	_, err = d.request(ctx, message{Op: "send", Frames: fs})
	return err
}
func (d *Direct) Fetch(ctx context.Context, f nostr.Filter) (transport.Batch, error) {
	return d.request(ctx, message{Op: "fetch", Filter: &f})
}

// Watch owns an independent bounded subscription. Canceling it releases only
// that subscription. A lost connection closes all subscriptions.
func (d *Direct) Watch(ctx context.Context, f nostr.Filter) (<-chan nostr.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.ctx.Err() != nil {
		d.mu.Unlock()
		return nil, ErrDisconnected
	}
	if len(d.local) >= maxWatches {
		d.mu.Unlock()
		return nil, errors.New("bluetooth: watch capacity exceeded")
	}
	d.nextWatch++
	id := d.nextWatch
	sub := &subscription{filter: f, events: make(chan nostr.Event, maxResults)}
	d.local[id] = sub
	d.mu.Unlock()
	remove := func() {
		d.mu.Lock()
		if d.local[id] != nil {
			delete(d.local, id)
			close(sub.events)
		}
		d.mu.Unlock()
		if err := d.enqueue(message{Op: "unwatch", Sub: id}); err != nil && d.ctx.Err() == nil {
			d.stop(err)
		}
	}
	if _, err := d.request(ctx, message{Op: "watch", Sub: id, Filter: &f}); err != nil {
		remove()
		return nil, err
	}
	go func() {
		select {
		case <-ctx.Done():
			remove()
		case <-d.ctx.Done():
		}
	}()
	return sub.events, nil
}

// Exchange subscribes before submission. A failure after Send starts is uncertain
// and must not trigger automatic retry on another carrier.
func (d *Direct) Exchange(ctx context.Context, event nostr.Event, answers nostr.Filter, match func(nostr.Event) bool) (nostr.Event, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := d.Watch(ctx, answers)
	if err != nil {
		return nostr.Event{}, &transport.NotSubmittedError{Err: err}
	}
	if err = d.Send(ctx, event); err != nil {
		return nostr.Event{}, err
	}
	for {
		select {
		case <-ctx.Done():
			return nostr.Event{}, ctx.Err()
		case e, ok := <-events:
			if !ok {
				return nostr.Event{}, ErrDisconnected
			}
			if match(e) {
				return e, nil
			}
		}
	}
}
