package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/turn"
)

// TestTURNFromTheFirstRelayThatServesIt runs arc relay turn for a citizen
// whose first relay is down and whose second relay serves no TURN. The
// credential comes from the third relay (docs/direct/SPEC.md, section 3.6).
func TestTURNFromTheFirstRelayThatServesIt(t *testing.T) {
	// A relay that is down: nothing listens on its port.
	gone, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := "ws://" + gone.Addr().String()
	_ = gone.Close()

	plain, _, _, closePlain, err := newRelay(filepath.Join(t.TempDir(), "plain.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePlain)
	plainServer := httptest.NewServer(relayHandler(plain, nil, nil))
	t.Cleanup(plainServer.Close)

	rl, _, _, closeDB, err := newRelay(filepath.Join(t.TempDir(), "turn.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	turnServer, err := turn.Serve(conn, turn.Config{Secret: "a secret", PublicIP: net.ParseIP("127.0.0.1"), RelayHost: "127.0.0.1", MinPort: 50100, MaxPort: 50110, Quota: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = turnServer.Close() })
	turnHTTP := httptest.NewServer(relayHandler(rl, turnServer, nil))
	t.Cleanup(turnHTTP.Close)

	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	for _, relay := range []string{down, "ws" + strings.TrimPrefix(plainServer.URL, "http"), "ws" + strings.TrimPrefix(turnHTTP.URL, "http")} {
		ok(t, home, "", "relay", "add", relay)
	}

	var credential turn.Credential
	out := ok(t, home, "", "relay", "turn")
	if err := json.Unmarshal([]byte(out), &credential); err != nil || credential.Username == "" || len(credential.URLs) == 0 {
		t.Fatalf("arc relay turn printed %q, want a credential of the third relay", out)
	}
	if !strings.Contains(credential.URLs[0], conn.LocalAddr().(*net.UDPAddr).String()[len("127.0.0.1"):]) {
		t.Errorf("the credential names %v, want the TURN server at %s", credential.URLs, conn.LocalAddr())
	}
}

// TestTURNPrefersARelayThatTakesTCP runs arc relay turn for a citizen with
// two relays that serve TURN: the first over UDP only, the second also over
// TCP, as behind a TLS proxy on port 443. A citizen that cannot send UDP
// relays only through the second, so the credential comes from it.
func TestTURNPrefersARelayThatTakesTCP(t *testing.T) {
	const tls = "turns:relay.example:443?transport=tcp"
	serve := func(name string, config turn.Config) string {
		rl, _, _, closeDB, err := newRelay(filepath.Join(t.TempDir(), name+".db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(closeDB)
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server, err := turn.Serve(conn, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		web := httptest.NewServer(relayHandler(rl, server, nil))
		t.Cleanup(web.Close)
		return "ws" + strings.TrimPrefix(web.URL, "http")
	}
	base := turn.Config{Secret: "a secret", PublicIP: net.ParseIP("127.0.0.1"), RelayHost: "127.0.0.1", Quota: 1 << 20}
	udpOnly, withTCP := base, base
	udpOnly.MinPort, udpOnly.MaxPort = 50120, 50130
	withTCP.MinPort, withTCP.MaxPort = 50140, 50150
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	withTCP.TCP, withTCP.URLs = tcp, []string{tls}

	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	ok(t, home, "", "relay", "add", serve("udp", udpOnly))
	ok(t, home, "", "relay", "add", serve("tcp", withTCP))

	var credential turn.Credential
	out := ok(t, home, "", "relay", "turn")
	if err := json.Unmarshal([]byte(out), &credential); err != nil || !slices.Contains(credential.URLs, tls) {
		t.Fatalf("arc relay turn printed %q, want the credential of the relay that takes TCP", out)
	}
}
