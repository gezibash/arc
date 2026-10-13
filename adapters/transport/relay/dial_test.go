package relay_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/internal/testrelay"
)

// stalling is a listener that accepts each connection and never answers, as
// a network does that loses packets after the TCP handshake.
type stalling struct {
	net.Listener
	accepted atomic.Int64
	mu       sync.Mutex
	conns    []net.Conn
}

func (l *stalling) serve() {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		l.accepted.Add(1)
		l.mu.Lock()
		l.conns = append(l.conns, conn)
		l.mu.Unlock()
	}
}

func (l *stalling) close() {
	_ = l.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, conn := range l.conns {
		_ = conn.Close()
	}
}

// lossyIPv6 opens a port on ::1 that accepts and never answers, and a relay on
// the same port of 127.0.0.1. It returns the URL of the relay by the name
// localhost, and the IPv6 listener.
func lossyIPv6(t *testing.T) (string, *stalling) {
	t.Helper()
	for range 20 {
		six, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			t.Skipf("no IPv6 loopback: %v", err)
		}
		port := six.Addr().(*net.TCPAddr).Port
		four, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			_ = six.Close()
			continue
		}
		stall := &stalling{Listener: six}
		go stall.serve()
		t.Cleanup(stall.close)
		testrelay.Serve(t, four)
		return fmt.Sprintf("ws://localhost:%d", port), stall
	}
	t.Fatal("found no port that is free on both ::1 and 127.0.0.1")
	return "", nil
}

// A network that loses packets over IPv6 after the TCP handshake: the
// connection to ::1 opens and then stalls. The call goes over IPv4 instead,
// within its deadline, and later connections go there first.
func TestACallMovesToIPv4WhenIPv6Stalls(t *testing.T) {
	defer relay.SetStallLimit(300 * time.Millisecond)()
	url, six := lossyIPv6(t)
	if addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), "localhost"); err != nil || len(addrs) < 2 {
		t.Skipf("localhost does not name both loopback addresses: %v %v", addrs, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	filter := nostr.Filter{Kinds: []nostr.Kind{1}, Limit: 1}
	if _, err := (relay.Relay{URL: url}).Fetch(ctx, filter); err != nil {
		t.Fatalf("the call failed: %v", err)
	}
	if got := six.accepted.Load(); got != 1 {
		t.Fatalf("the IPv6 listener accepted %d connections, want 1: the test did not stall IPv6 first", got)
	}

	if _, err := (relay.Relay{URL: url}).Fetch(ctx, filter); err != nil {
		t.Fatalf("the second call failed: %v", err)
	}
	if got := six.accepted.Load(); got != 1 {
		t.Errorf("the second call dialed IPv6 again: %d connections, want 1", got)
	}
}
