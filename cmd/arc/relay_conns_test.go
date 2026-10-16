package main

import (
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gezibash/arc/adapters/relay/limits"
)

// counting is a listener that counts the connections it accepts.
type counting struct {
	net.Listener
	accepted atomic.Int64
}

func (l *counting) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}

// One sync shares one WebSocket connection between all its calls to a relay.
// It also fetches the NIP-11 document of the relay once, over HTTP. Each new
// connection is one more chance to fail on a lossy network.
func TestSyncUsesOneConnectionPerRelay(t *testing.T) {
	rl, _, _, closeDB, err := newRelay(filepath.Join(t.TempDir(), "relay.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &counting{Listener: inner}
	var upgrades, documents atomic.Int64
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Upgrade") != "":
			upgrades.Add(1)
		case r.Header.Get("Accept") == "application/nostr+json":
			documents.Add(1)
		}
		rl.ServeHTTP(w, r)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	url := "ws://" + inner.Addr().String()

	alice, _ := providerHome(t, url)
	bob, bobKey := providerHome(t, url)
	ok(t, alice, "", "message", "send", bobKey, "hello")

	conns, ups, docs := listener.accepted.Load(), upgrades.Load(), documents.Load()
	ok(t, bob, "", "sync")
	conns, ups, docs = listener.accepted.Load()-conns, upgrades.Load()-ups, documents.Load()-docs
	if ups != 1 || docs != 1 || conns != 2 {
		t.Errorf("one sync opened %d connections: %d WebSocket and %d NIP-11 requests, want 1 WebSocket and 1 NIP-11 request", conns, ups, docs)
	}
	if inbox := ok(t, bob, "", "message", "inbox", "--json"); !strings.Contains(inbox, "hello") {
		t.Errorf("bob's inbox lacks the message: %s", inbox)
	}
}
