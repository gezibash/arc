package ble

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/wire"
)

type memoryLink struct {
	in, out chan []byte
	done    chan struct{}
	once    *sync.Once
	corrupt bool
}

func (l *memoryLink) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (l *memoryLink) write(ctx context.Context, b []byte) error {
	b = append([]byte(nil), b...)
	if l.corrupt {
		b[len(b)-1] ^= 1
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.done:
		return ErrDisconnected
	case l.out <- b:
		return nil
	}
}
func (l *memoryLink) read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, ErrDisconnected
	case b := <-l.in:
		return b, nil
	}
}
func memoryPair() (*memoryLink, *memoryLink) {
	a, b := make(chan []byte, 256), make(chan []byte, 256)
	done := make(chan struct{})
	once := &sync.Once{}
	return &memoryLink{in: a, out: b, done: done, once: once}, &memoryLink{in: b, out: a, done: done, once: once}
}
func config(t *testing.T, k keys.Key, peer keys.Key) DirectConfig {
	t.Helper()
	s, err := boltstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return DirectConfig{Signer: k, Peer: peer.Public, Store: s, Share: func(nostr.Event) bool { return true }}
}
func pair(t *testing.T) (*Direct, *Direct, keys.Key, keys.Key) {
	return pairWithShare(t, func(nostr.Event) bool { return true })
}
func pairWithShare(t *testing.T, share func(nostr.Event) bool) (*Direct, *Direct, keys.Key, keys.Key) {
	t.Helper()
	a, b := keys.Generate(), keys.Generate()
	ca, cb := config(t, a, b), config(t, b, a)
	cb.Share = share
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	la, lb := memoryPair()
	type result struct {
		s *secureLink
		e error
	}
	ch := make(chan result, 1)
	go func() { s, e := authenticate(ctx, lb, cb, false); ch <- result{s, e} }()
	sa, err := authenticate(ctx, la, ca, true)
	if err != nil {
		la.Close()
		t.Fatal(err)
	}
	r := <-ch
	if r.e != nil {
		t.Fatal(r.e)
	}
	da, db := newDirect(ctx, sa, ca), newDirect(ctx, r.s, cb)
	t.Cleanup(func() { da.Close(); db.Close() })
	return da, db, a, b
}
func signed(t *testing.T, k keys.Key, kind nostr.Kind, content string) nostr.Event {
	t.Helper()
	e := nostr.Event{Kind: kind, CreatedAt: nostr.Now(), Tags: nostr.Tags{}, Content: content}
	if err := e.Sign(k.Secret); err != nil {
		t.Fatal(err)
	}
	return e
}
func TestDirectSignedSync(t *testing.T) {
	a, b, ka, _ := pair(t)
	ctx := context.Background()
	event := signed(t, ka, 1, strings.Repeat("fragmented ", 1000))
	if err := a.Send(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, event); err != nil {
		t.Fatal(err)
	}
	got, err := b.config.Store.Query(nostr.Filter{IDs: []nostr.ID{event.ID}})
	if err != nil || len(got) != 1 || got[0].String() != event.String() {
		t.Fatalf("store roundtrip: %v %d", err, len(got))
	}
	batch, err := a.Fetch(ctx, nostr.Filter{IDs: []nostr.ID{event.ID}})
	if err != nil || len(batch.Events) != 1 || batch.Events[0].ID != event.ID {
		t.Fatalf("fetch: %v %+v", err, batch)
	}
	event.Sig[0] ^= 1
	fs, err := pack(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.request(ctx, message{Op: "send", Frames: fs}); err == nil {
		t.Fatal("invalid signature accepted")
	}
}
func TestDirectPrivateDelivery(t *testing.T) {
	a, b, ka, kb := pair(t)
	ctx := context.Background()
	rumor := private.Rumor(ka, 14, "private content", nostr.Tags{}, time.Now())
	wrap, err := private.Wrap(ctx, ka, kb.Public, rumor, private.RelayForm, private.WrapKind, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Send(ctx, wrap); err != nil {
		t.Fatal(err)
	}
	events, err := b.config.Store.Query(nostr.Filter{IDs: []nostr.ID{wrap.ID}})
	if err != nil || len(events) != 1 {
		t.Fatal("missing private event", err)
	}
	opened, err := private.Unwrap(ctx, kb, events[0])
	if err != nil || opened.Rumor.Content != "private content" {
		t.Fatal("recipient failed", err)
	}
	if _, err = private.Unwrap(ctx, keys.Generate(), events[0]); err == nil {
		t.Fatal("unrelated identity opened content")
	}
}
func TestDirectLiveAndDisconnect(t *testing.T) {
	a, b, ka, _ := pair(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events, err := b.Watch(ctx, nostr.Filter{Kinds: []nostr.Kind{21059}})
	if err != nil {
		t.Fatal(err)
	}
	e := signed(t, ka, 21059, "opaque live payload")
	if err = a.Send(ctx, e); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-events:
		if got.ID != e.ID {
			t.Fatal("wrong event")
		}
	case <-time.After(time.Second):
		t.Fatal("lost live event")
	}
	stored, err := b.config.Store.Query(nostr.Filter{IDs: []nostr.ID{e.ID}})
	if err != nil || len(stored) != 0 {
		t.Fatal("persisted ephemeral event", err)
	}
	a.Close()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("watch still open")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not close watch")
	}
}
func TestDirectCancellationAndSharePolicy(t *testing.T) {
	a, b, ka, _ := pair(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e := signed(t, ka, 1, "not submitted")
	if err := a.Send(ctx, e); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	has, err := b.config.Store.Has(e.ID)
	if err != nil || has {
		t.Fatal("canceled send persisted", err)
	}
}
func TestDirectWrongIdentity(t *testing.T) {
	a, b := keys.Generate(), keys.Generate()
	ca, cb := config(t, a, keys.Generate()), config(t, b, a)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	la, lb := memoryPair()
	defer la.Close()
	done := make(chan error, 1)
	go func() { _, err := authenticate(ctx, lb, cb, false); done <- err }()
	if _, err := authenticate(ctx, la, ca, true); err == nil {
		t.Fatal("wrong expected identity accepted")
	}
	la.Close()
	<-done
}
func TestDirectRejectsIncompleteFrames(t *testing.T) {
	e := signed(t, keys.Generate(), 1, strings.Repeat("x", 3000))
	fs, err := pack(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unpack(fs[:1]); err == nil {
		t.Fatal("accepted partial event")
	}
	fs[0][1] = 1
	if _, err = unpack(fs); err == nil {
		t.Fatal("accepted forwarding")
	}
	if _, err = unpack(make([][]byte, 129)); err == nil {
		t.Fatal("unbounded frame count")
	}
}

// The provider is injected at the existing core protocol boundary. The real
// core call server verifies and opens the request and seals the reply.
type echoProvider struct{ lines chan wire.Event }

func (p *echoProvider) Lines() <-chan wire.Event { return p.lines }
func (p *echoProvider) Send(ctx context.Context, e wire.Event) error {
	if e.Op != "request" {
		return nil
	}
	reply := "echo: " + *e.Message
	select {
	case p.lines <- wire.Event{Op: "reply", RequestID: e.RequestID, Reply: &reply}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestDirectCoreCapabilityCall(t *testing.T) {
	a, b, ka, kb := pair(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	p := &echoProvider{lines: make(chan wire.Event, 4)}
	server := call.NewServer(kb, "primary", p, 64<<10, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- server.ServeLive(ctx, b, func() { close(ready) }) }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("provider not ready")
	}
	reply, _, err := call.Live(ctx, ka, kb.Public, call.Request{Capability: "primary", Method: "ECHO", Path: "/", Body: "hello"}, a)
	if err != nil || reply.Body != "echo: hello" {
		t.Fatalf("call: %+v %v", reply, err)
	}
	cancel()
	<-done
	close(p.lines)
}
func TestSecureRejectsCiphertextCorruption(t *testing.T) {
	a, b, _, _ := pair(t)
	// Corrupt the ciphertext after both identities have been authenticated.
	a.conn.link.(*memoryLink).corrupt = true
	if err := a.Send(t.Context(), signed(t, keys.Generate(), 1, "tampered")); err == nil {
		t.Fatal("corrupted ciphertext accepted")
	}
	if b.Err() == nil {
		t.Fatal("peer did not report link failure")
	}
}
func TestDirectNoLiveSubscriber(t *testing.T) {
	a, _, ka, _ := pair(t)
	if err := a.Send(t.Context(), signed(t, ka, 21059, "no subscriber")); err == nil {
		t.Fatal("reported live delivery without a subscriber")
	}
}
func TestDirectSharePolicy(t *testing.T) {
	a, _, ka, _ := pairWithShare(t, func(nostr.Event) bool { return false })
	e := signed(t, ka, 1, "stored")
	if err := a.Send(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	batch, err := a.Fetch(t.Context(), nostr.Filter{IDs: []nostr.ID{e.ID}})
	if err != nil || len(batch.Events) != 0 {
		t.Fatal("unshared event disclosed", err)
	}
}
func TestDirectWatchCancellation(t *testing.T) {
	a, _, _, _ := pair(t)
	ctx, cancel := context.WithCancel(t.Context())
	events, err := a.Watch(ctx, nostr.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("unexpected event")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled watch leaked")
	}
}

func TestDirectSnapshotBounds(t *testing.T) {
	a, b, _, kb := pair(t)
	for i := range 65 {
		e := signed(t, kb, 1, fmt.Sprint(i))
		if _, err := b.config.Store.Save(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Fetch(t.Context(), nostr.Filter{Kinds: []nostr.Kind{1}}); err == nil {
		t.Fatal("silently truncated oversized query")
	}
}
func TestDirectConcurrentSends(t *testing.T) {
	a, b, ka, _ := pair(t)
	var wg sync.WaitGroup
	for i := range 8 {
		e := signed(t, ka, 1, fmt.Sprint(i))
		wg.Go(func() {
			if err := a.Send(t.Context(), e); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	events, err := b.config.Store.Query(nostr.Filter{Kinds: []nostr.Kind{1}})
	if err != nil || len(events) != 8 {
		t.Fatalf("concurrent delivery: %d %v", len(events), err)
	}
}
func TestRecordRejectsOversize(t *testing.T) {
	a, b := memoryPair()
	defer a.Close()
	if err := a.write(t.Context(), []byte{0xff, 0xff}); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecord(t.Context(), b); err == nil {
		t.Fatal("accepted oversized record")
	}
}
