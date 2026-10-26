package relay_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/coder/websocket"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/internal/testrelay"
)

// giftWrap is an event that the relay takes only after authentication.
func giftWrap(t *testing.T) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: 1059, CreatedAt: nostr.Now(), Content: "sealed", Tags: nostr.Tags{{"p", nostr.Generate().Public().Hex()}}}
	if err := event.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	return event
}

func note(t *testing.T, content string) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: content, Tags: nostr.Tags{}}
	if err := event.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	return event
}

// holds says whether the relay stored an event. It asks on its own connection.
func holds(t *testing.T, url string, event nostr.Event) bool {
	t.Helper()
	batch, err := relay.Relay{URL: url}.Fetch(context.Background(), nostr.Filter{IDs: []nostr.ID{event.ID}})
	if err != nil {
		t.Fatal(err)
	}
	return len(batch.Events) == 1
}

// A session sends one event for each chunk. Each event must not pay for a
// connection and an authentication of its own.
func TestSendKeepsOneConnection(t *testing.T) {
	url, conns := testrelay.StartTracked(t)
	r := relay.New(url, keyer.NewPlainKeySigner(nostr.Generate()))
	defer func() { _ = r.Close() }()

	for range 3 {
		if err := r.Send(context.Background(), giftWrap(t)); err != nil {
			t.Fatalf("the relay did not take a gift wrap: %v", err)
		}
	}
	if got := conns.Accepted(); got != 1 {
		t.Fatalf("3 sends opened %d connections, want 1", got)
	}
}

func TestSendsAtTheSameTimeShareTheConnection(t *testing.T) {
	url, conns := testrelay.StartTracked(t)
	r := relay.New(url, keyer.NewPlainKeySigner(nostr.Generate()))
	defer func() { _ = r.Close() }()

	const senders = 8
	errs := make(chan error, senders)
	var wg sync.WaitGroup
	for range senders {
		event := giftWrap(t)
		wg.Go(func() { errs <- r.Send(context.Background(), event) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a send failed: %v", err)
		}
	}
	if got := conns.Accepted(); got != 1 {
		t.Fatalf("%d sends at the same time opened %d connections, want 1", senders, got)
	}
}

func TestSendConnectsAgainAfterTheRelayClosedTheConnection(t *testing.T) {
	url, conns := testrelay.StartTracked(t)
	r := relay.New(url, nil)
	defer func() { _ = r.Close() }()

	if err := r.Send(context.Background(), note(t, "before")); err != nil {
		t.Fatal(err)
	}
	conns.Drop()
	after := note(t, "after")
	if err := r.Send(context.Background(), after); err != nil {
		t.Fatalf("the send after the close failed: %v", err)
	}
	if !holds(t, url, after) {
		t.Fatal("the relay does not hold the event that came after the close")
	}
}

// A machine that slept leaves a connection that looks open and answers
// nothing. The send must not wait on it until its caller gives up.
func TestSendLeavesAConnectionThatDoesNotAnswer(t *testing.T) {
	defer relay.SetReuseWait(200 * time.Millisecond)()
	url, conns := testrelay.StartTracked(t)
	r := relay.New(url, nil)
	defer func() { _ = r.Close() }()

	if err := r.Send(context.Background(), note(t, "before")); err != nil {
		t.Fatal(err)
	}
	conns.Silence()
	after := note(t, "after")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Send(ctx, after); err != nil {
		t.Fatalf("the send on a silent connection failed: %v", err)
	}
	if !holds(t, url, after) {
		t.Fatal("the relay does not hold the event that came after the silence")
	}
}

// The transport contract: a caller that stopped sends nothing. A connection
// that is open already must not change that.
func TestAStoppedCallerSendsNothingOnAUsedConnection(t *testing.T) {
	url, _ := testrelay.StartTracked(t)
	r := relay.New(url, nil)
	defer func() { _ = r.Close() }()

	if err := r.Send(context.Background(), note(t, "before")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	late := note(t, "late")
	if err := r.Send(ctx, late); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
	if holds(t, url, late) {
		t.Fatal("the relay holds an event of a caller that stopped")
	}
}

// A query that gets no answer on the shared connection leaves it, so the next
// call opens a new connection at once.
func TestAFetchThatStallsLeavesTheConnection(t *testing.T) {
	url, conns := testrelay.StartTracked(t)
	r := relay.New(url, nil)
	defer func() { _ = r.Close() }()
	filter := nostr.Filter{Kinds: []nostr.Kind{1}, Limit: 1}

	if _, err := r.Fetch(context.Background(), filter); err != nil {
		t.Fatal(err)
	}
	conns.Silence()
	stalled, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := r.Fetch(stalled, filter); err == nil {
		t.Fatal("a query on a silent connection got an answer")
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.Fetch(ctx, filter); err != nil {
		t.Fatalf("the query after the stall failed: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the query after the stall took %v, want a new connection at once", took)
	}
	if got := conns.Accepted(); got != 2 {
		t.Errorf("the relay accepted %d connections, want 2", got)
	}
}

// A machine that slept leaves a connection that looks open and answers
// nothing. After a pause, a call checks the connection, and opens a new one.
func TestACallAfterAPauseLeavesASilentConnection(t *testing.T) {
	defer relay.SetReuseWait(200 * time.Millisecond)()
	defer relay.SetIdleCheck(0)()
	url, conns := testrelay.StartTracked(t)
	r := relay.New(url, nil)
	defer func() { _ = r.Close() }()
	filter := nostr.Filter{Kinds: []nostr.Kind{1}, Limit: 1}

	if _, err := r.Fetch(context.Background(), filter); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Fetch(context.Background(), filter); err != nil {
		t.Fatalf("the check refused a connection that works: %v", err)
	}
	if got := conns.Accepted(); got != 1 {
		t.Fatalf("a check of a working connection opened another: %d connections, want 1", got)
	}

	conns.Silence()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := r.Fetch(ctx, filter); err != nil {
		t.Fatalf("the query after the pause failed: %v", err)
	}
	if got := conns.Accepted(); got != 2 {
		t.Errorf("the relay accepted %d connections, want 2", got)
	}
}

// blackhole is a relay that takes each connection and never answers.
func blackhole(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	return "ws://" + l.Addr().String()
}

// A relay that did not connect is down for a while: the next fetch fails at
// once with the same error, so one dead relay stalls one call of a command,
// not each one (#439). A watch still tries, so that a provider watches again
// as soon as its relay is back. After downFor, a fetch tries again.
func TestARelayThatDidNotConnectFailsAtOnce(t *testing.T) {
	defer relay.SetStallLimit(200 * time.Millisecond)()
	defer relay.SetDownFor(2 * time.Second)()
	r := relay.New(blackhole(t), keyer.NewPlainKeySigner(nostr.Generate()))
	filter := nostr.Filter{Kinds: []nostr.Kind{1}}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := r.Fetch(ctx, filter); err == nil {
		t.Fatal("a fetch from a relay that never answers worked")
	}
	start := time.Now()
	_, err := r.Fetch(t.Context(), filter)
	if err == nil || !strings.Contains(err.Error(), "arc asks again in") || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("the second fetch took %s and gave %v, want the error of the first at once", time.Since(start), err)
	}
	wctx, wcancel := context.WithTimeout(t.Context(), time.Second)
	defer wcancel()
	if _, err := r.Watch(wctx, filter); err != nil && strings.Contains(err.Error(), "arc asks again in") {
		t.Errorf("a watch did not try the relay: %v", err)
	}
	time.Sleep(2 * time.Second)
	again, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	if _, err := r.Fetch(again, filter); err == nil || strings.Contains(err.Error(), "arc asks again in") {
		t.Errorf("a fetch after downFor did not try again: %v", err)
	}
}

// A relay that refuses the connection answered: it is not down, and the
// next fetch asks it again (#439).
func TestARefusalIsNotDown(t *testing.T) {
	var asked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	r := relay.New("ws"+strings.TrimPrefix(server.URL, "http"), keyer.NewPlainKeySigner(nostr.Generate()))
	for range 2 {
		if _, err := r.Fetch(t.Context(), nostr.Filter{Kinds: []nostr.Kind{1}}); err == nil {
			t.Fatal("a fetch from a relay that refuses worked")
		}
	}
	if asked.Load() < 2 {
		t.Errorf("the relay was asked %d times, want each fetch to ask it", asked.Load())
	}
}

// muteRelay takes each WebSocket connection, and never answers on it.
func muteRelay(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conns.Add(1)
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), &conns
}

// A relay that connects and then gives no EOSE, OK or CLOSED within its
// own timeout is down: the next query and send fail at once, on no new
// connection, and the error folds as one that no relay answered (#439).
func TestARelayThatConnectsAndNeverAnswersIsDown(t *testing.T) {
	defer relay.SetCallTimeout(300 * time.Millisecond)()
	url, conns := muteRelay(t)
	r := relay.New(url, keyer.NewPlainKeySigner(nostr.Generate()))
	filter := nostr.Filter{Kinds: []nostr.Kind{1}}
	if _, err := r.Fetch(t.Context(), filter); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a query to a relay that never answers gave %v, want its timeout", err)
	}
	start := time.Now()
	_, err := r.Fetch(t.Context(), filter)
	sendErr := r.Send(t.Context(), note(t, "hello"))
	if err == nil || sendErr == nil || time.Since(start) > 100*time.Millisecond || conns.Load() != 1 {
		t.Errorf("after a timeout, a query and a send took %s, gave %v and %v, on %d connections; want both at once, on 1", time.Since(start), err, sendErr, conns.Load())
	}
}
