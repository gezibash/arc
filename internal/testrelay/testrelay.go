// Package testrelay runs a Nostr relay inside a test.
package testrelay

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/eventstore/slicestore"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/adapters/relay/groups"
	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/sealed"
)

// Start runs a relay that keeps events in memory and supports Negentropy, and
// returns its URL. The relay stops when the test ends.
func Start(t *testing.T) string {
	return start(t, true)
}

// StartKillable runs a relay, and returns a function that kills it: it closes
// the listener and every open connection at once, as a crash would.
func StartKillable(t *testing.T) (string, func()) {
	t.Helper()

	db := &slicestore.SliceStore{}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	relay := khatru.NewRelay()
	relay.Log = log.New(io.Discard, "", 0)
	relay.UseEventstore(db, 500)
	sealed.Protect(relay)

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &tracking{Listener: inner}
	server := &http.Server{Handler: relay}
	go func() { _ = server.Serve(listener) }()

	kill := func() {
		_ = listener.Close()
		listener.closeAll()
	}
	t.Cleanup(kill)
	return "ws://" + inner.Addr().String(), kill
}

// tracking is a listener that remembers every connection, so a test can close
// them all. A WebSocket connection leaves the HTTP server's own tracking.
type tracking struct {
	net.Listener
	mu       sync.Mutex
	conns    []*tracked
	accepted int
}

func (l *tracking) Accept() (net.Conn, error) {
	inner, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	conn := &tracked{Conn: inner, closed: make(chan struct{})}
	l.mu.Lock()
	l.conns = append(l.conns, conn)
	l.accepted++
	l.mu.Unlock()
	return conn, nil
}

func (l *tracking) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, conn := range l.conns {
		_ = conn.Close()
	}
	l.conns = nil
}

// tracked is a connection that a test can make silent.
type tracked struct {
	net.Conn
	silent atomic.Bool
	closed chan struct{}
	once   sync.Once
}

// Read gives the relay nothing more after the connection became silent.
func (c *tracked) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.silent.Load() {
		<-c.closed
		return 0, net.ErrClosed
	}
	return n, err
}

func (c *tracked) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// Connections is what a test can do with the connections of a relay.
type Connections struct{ listener *tracking }

// Accepted says how many connections the relay accepted.
func (c Connections) Accepted() int {
	c.listener.mu.Lock()
	defer c.listener.mu.Unlock()
	return c.listener.accepted
}

// Drop closes each open connection. The relay accepts new connections.
func (c Connections) Drop() { c.listener.closeAll() }

// Silence makes each open connection silent: the relay reads nothing more
// from it, and does not close it. A machine that slept leaves such a
// connection. The relay accepts new connections.
func (c Connections) Silence() {
	c.listener.mu.Lock()
	defer c.listener.mu.Unlock()
	for _, conn := range c.listener.conns {
		conn.silent.Store(true)
	}
}

// StartTracked runs a relay that takes a gift wrap only after NIP-42
// authentication, as the public deploy does, and returns its URL and its
// connections.
func StartTracked(t *testing.T) (string, Connections) {
	t.Helper()
	db := &slicestore.SliceStore{}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	relay := khatru.NewRelay()
	relay.Log = log.New(io.Discard, "", 0)
	relay.UseEventstore(db, 500)
	sealed.Protect(relay)
	limits.Apply(relay, nil, limits.Policy{WrapAuth: true})

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &tracking{Listener: inner}
	server := &http.Server{Handler: relay}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		listener.closeAll()
	})
	return "ws://" + inner.Addr().String(), Connections{listener}
}

// StartGuarded runs a relay that takes a gift wrap only after NIP-42
// authentication, as the public deploy does.
func StartGuarded(t *testing.T) string {
	t.Helper()
	db := &slicestore.SliceStore{}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	relay := khatru.NewRelay()
	relay.Log = log.New(io.Discard, "", 0)
	relay.UseEventstore(db, 500)
	sealed.Protect(relay)
	limits.Apply(relay, nil, limits.Policy{WrapAuth: true})

	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// StartPlain runs a relay that does not support Negentropy.
func StartPlain(t *testing.T) string {
	return start(t, false)
}

// StartDown runs a relay that is down, and returns its URL and a function
// that brings it up. Until then, the relay closes each connection when it
// accepts it, so a dial to it fails at once. The relay keeps its port while it
// is down, so no other process can take the port. The relay stops when the
// test ends.
func StartDown(t *testing.T) (string, func()) {
	t.Helper()

	db := &slicestore.SliceStore{}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	relay := khatru.NewRelay()
	relay.Log = log.New(io.Discard, "", 0)
	relay.UseEventstore(db, 500)
	sealed.Protect(relay)

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &gated{Listener: inner}
	server := &http.Server{Handler: relay}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "ws://" + inner.Addr().String(), func() { listener.up.Store(true) }
}

// gated is a listener that closes each connection at once until the relay is
// up.
type gated struct {
	net.Listener
	up atomic.Bool
}

func (l *gated) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil || l.up.Load() {
			return conn, err
		}
		_ = conn.Close()
	}
}

func start(t *testing.T, negentropy bool) string {
	t.Helper()

	db := &slicestore.SliceStore{}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}

	relay := khatru.NewRelay()
	relay.Log = log.New(io.Discard, "", 0)
	relay.UseEventstore(db, 500)
	sealed.Protect(relay)
	if negentropy {
		relay.Negentropy = true
		relay.Info.SupportedNIPs = append(relay.Info.SupportedNIPs, 77)
	}

	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// StartGroups runs a relay that hosts NIP-29 groups, with one open group and
// its admins. It returns the URL and the key of the relay.
func StartGroups(t *testing.T, id string, admins ...nostr.PubKey) (string, nostr.PubKey) {
	t.Helper()
	// Bolt, as arc uses: it cannot delete while a query of it is open.
	db := &boltdb.BoltBackend{Path: filepath.Join(t.TempDir(), "relay.db")}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	relay := khatru.NewRelay()
	relay.Log = log.New(io.Discard, "", 0)
	relay.UseEventstore(db, 500)
	sealed.Protect(relay)

	key := nostr.Generate()
	g, err := groups.Attach(relay, db, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Create(id, id, false, admins); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), key.Public()
}

// StartSlow runs a relay that waits for delay before it takes a subscription
// for events tagged to one key. Until then, the relay does not deliver an
// ephemeral event to that key. Subscriptions for other keys are not slow.
func StartSlow(t *testing.T, delay time.Duration, to nostr.PubKey) string {
	t.Helper()
	db := &slicestore.SliceStore{}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	relay := khatru.NewRelay()
	relay.Log = log.New(io.Discard, "", 0)
	relay.UseEventstore(db, 500)
	sealed.Protect(relay)

	// Khatru handles each message in its own goroutine, and adds the
	// listener only after OnRequest returns.
	onRequest := relay.OnRequest
	relay.OnRequest = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		if slices.Contains(filter.Tags["p"], to.Hex()) {
			time.Sleep(delay)
		}
		if onRequest != nil {
			return onRequest(ctx, filter)
		}
		return false, ""
	}

	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}
