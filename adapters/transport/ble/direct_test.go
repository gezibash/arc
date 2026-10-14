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
	size    int
}

func (l *memoryLink) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (l *memoryLink) payload() int {
	if l.size == 0 {
		return minPacket
	}
	return l.size
}
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
	// The responder rejects the proof and closes its link, as OpenDirect does.
	go func() { _, err := authenticate(ctx, lb, cb, false); lb.Close(); done <- err }()
	if _, err := authenticate(ctx, la, ca, true); !errors.Is(err, errRejected) {
		t.Fatalf("wrong expected identity: %v", err)
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
	for range 3 {
		reply, _, err := call.Live(ctx, ka, kb.Public, call.Request{Capability: "primary", Method: "ECHO", Path: "/", Body: "hello"}, a)
		if err != nil || reply.Body != "echo: hello" {
			t.Fatalf("call: %+v %v", reply, err)
		}
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

func TestPacketSize(t *testing.T) {
	for mtu, want := range map[int]int{0: 20, 23: 20, 185: 182, 247: 244, 515: 512, 517: 512, 2048: 512} {
		if got := packetSize(mtu); got != want {
			t.Errorf("MTU %d: packet %d, want %d", mtu, got, want)
		}
	}
}

// Each side sends with its own packet size; a reader takes any size up to
// maxPacket, so one link can carry large packets one way and small the other.
func TestRecordsCrossPacketSizes(t *testing.T) {
	a, b := memoryPair()
	defer a.Close()
	a.size, b.size = maxPacket, minPacket
	record := make([]byte, recordBytes)
	for i := range record {
		record[i] = byte(i)
	}
	for _, link := range [][2]*memoryLink{{a, b}, {b, a}} {
		if err := writeRecord(t.Context(), link[0], record); err != nil {
			t.Fatal(err)
		}
		got, err := readRecord(t.Context(), link[1])
		if err != nil || string(got) != string(record) {
			t.Fatalf("record from a %d-byte sender: %v", link[0].size, err)
		}
	}
	if err := a.write(t.Context(), make([]byte, maxPacket+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecord(t.Context(), b); err == nil {
		t.Fatal("accepted a packet larger than maxPacket")
	}
}

func TestDirectLargePackets(t *testing.T) {
	a, b := keys.Generate(), keys.Generate()
	ca, cb := config(t, a, b), config(t, b, a)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	la, lb := memoryPair()
	la.size, lb.size = 182, 244
	ch := make(chan *secureLink, 1)
	go func() { s, _ := authenticate(ctx, lb, cb, false); ch <- s }()
	sa, err := authenticate(ctx, la, ca, true)
	if err != nil {
		t.Fatal(err)
	}
	sb := <-ch
	if sb == nil {
		t.Fatal("responder did not authenticate")
	}
	da, db := newDirect(ctx, sa, ca), newDirect(ctx, sb, cb)
	defer da.Close()
	defer db.Close()
	e := signed(t, a, 1, strings.Repeat("large packets ", 400))
	if err = da.Send(ctx, e); err != nil {
		t.Fatal(err)
	}
	if got, err := db.config.Store.Query(nostr.Filter{IDs: []nostr.ID{e.ID}}); err != nil || len(got) != 1 {
		t.Fatalf("event over large packets: %d %v", len(got), err)
	}
}

func TestDirectIndependentWatches(t *testing.T) {
	a, b, ka, _ := pair(t)
	ctx, cancel := context.WithCancel(t.Context())
	first, err := b.Watch(ctx, nostr.Filter{Kinds: []nostr.Kind{1}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Watch(t.Context(), nostr.Filter{Kinds: []nostr.Kind{1}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("watch did not end")
	}
	e := signed(t, ka, 1, "still connected")
	if err = a.Send(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-second:
		if got.ID != e.ID {
			t.Fatal("wrong event")
		}
	case <-time.After(time.Second):
		t.Fatal("other watch ended")
	}
	if _, err = b.Fetch(t.Context(), nostr.Filter{}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectWatchCapacity(t *testing.T) {
	a, _, _, _ := pair(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var subscriptions []<-chan nostr.Event
	for range maxWatches {
		ch, err := a.Watch(ctx, nostr.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		subscriptions = append(subscriptions, ch)
	}
	if _, err := a.Watch(ctx, nostr.Filter{}); err == nil {
		t.Fatal("unbounded watches")
	}
	cancel()
	for _, ch := range subscriptions {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("watch leaked")
		}
	}
	if _, err := a.Fetch(t.Context(), nostr.Filter{}); err != nil {
		t.Fatal("limit closed link", err)
	}
}
