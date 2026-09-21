// Package testrelay runs a Nostr relay inside a test.
package testrelay

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/eventstore/slicestore"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/delivery/groups"
	"github.com/gezibash/arc/delivery/sealed"
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
	go server.Serve(listener)

	kill := func() {
		listener.Close()
		listener.closeAll()
	}
	t.Cleanup(kill)
	return "ws://" + inner.Addr().String(), kill
}

// tracking is a listener that remembers every connection, so a test can close
// them all. A WebSocket connection leaves the HTTP server's own tracking.
type tracking struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *tracking) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, conn)
		l.mu.Unlock()
	}
	return conn, err
}

func (l *tracking) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, conn := range l.conns {
		conn.Close()
	}
	l.conns = nil
}

// StartPlain runs a relay that does not support Negentropy.
func StartPlain(t *testing.T) string {
	return start(t, false)
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
	// Bolt, as arcn uses: it cannot delete while a query of it is open.
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
