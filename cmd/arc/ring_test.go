package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/rings"
)

type ringHit struct{ auth, body string }

// ringRelay runs a relay that rings, and a TLS server that stands for the
// ring URL https://example.com/hook. It returns the relay URL and the calls
// to the ring URL.
func ringRelay(t *testing.T) (string, chan ringHit) {
	t.Helper()
	hits := make(chan ringHit, 20)
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits <- ringHit{auth: r.Header.Get("Authorization"), body: string(body)}
	}))
	target.StartTLS()
	t.Cleanup(target.Close)
	public := netip.MustParseAddr("203.0.113.10")

	config := &rings.Config{
		Secret: "a secret of the relay",
		Lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host != "example.com" {
				return nil, fmt.Errorf("no host %s", host)
			}
			return []netip.Addr{public}, nil
		},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != netip.AddrPortFrom(public, 443).String() {
				return nil, fmt.Errorf("the test dials only %s", public)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, target.Listener.Addr().String())
		},
		TLS: &tls.Config{RootCAs: target.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs},
	}
	rl, _, ringer, closeDB, err := newRelay(filepath.Join(t.TempDir(), "relay.db"), limits.Policy{}, config, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ringer.Run(ctx)
	server := httptest.NewServer(relayHandler(rl, nil, ringer))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), hits
}

func waitRing(t *testing.T, hits chan ringHit, what string) ringHit {
	t.Helper()
	select {
	case hit := <-hits:
		return hit
	case <-time.After(10 * time.Second):
		t.Fatalf("no ring: %s", what)
		return ringHit{}
	}
}

func noRing(t *testing.T, hits chan ringHit, what string) {
	t.Helper()
	select {
	case hit := <-hits:
		t.Fatalf("a ring came: %s: %s", what, hit.body)
	case <-time.After(3 * time.Second):
	}
}

// The proof of rings through the real relay and the normal CLI
// (docs/rings/SPEC.md, section 8). Bob registers a ring URL. Mail from
// alice rings it once; more mail before bob reads does not; arc sync is a
// read, and the next mail rings again.
func TestRingsWakeACitizenThroughTheCLI(t *testing.T) {
	url, hits := ringRelay(t)

	// A second relay offers no rings: it answers 404 on /ring, and ring set
	// skips it.
	plain, _, _, closePlain, err := newRelay(filepath.Join(t.TempDir(), "plain.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePlain)
	plainServer := httptest.NewServer(relayHandler(plain, nil, nil))
	t.Cleanup(plainServer.Close)
	response, err := http.Get(plainServer.URL + rings.Path)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("a relay without rings answers %d on /ring, want 404", response.StatusCode)
	}
	plainURL := "ws" + strings.TrimPrefix(plainServer.URL, "http")

	bob, bobHex := providerHome(t, url)
	ok(t, bob, "", "relay", "add", plainURL)
	alice, _ := providerHome(t, url)

	const key = "a key of the routine of bob"
	t.Setenv("ARC_RING_KEY", key)
	if _, err := run(t, bob, "", "relay", "ring", "set", "https://example.com/hook", "--key-env", "NO_SUCH_VARIABLE"); err == nil {
		t.Fatal("ring set took an empty environment variable")
	}
	set := ok(t, bob, "", "relay", "ring", "set", "https://example.com/hook", "--key-env", "ARC_RING_KEY")
	if !strings.Contains(set, url+": rings example.com, 3 at once then one each 300 s, 48 of 48 left today, until ") || !strings.Contains(set, plainURL+": offers no rings, skipped") {
		t.Fatalf("arc relay ring set:\n%s", set)
	}
	if strings.Contains(set, key) {
		t.Fatal("ring set printed the key")
	}

	// A test ring reaches the URL with the key.
	if out := ok(t, bob, "", "relay", "ring", "test"); !strings.Contains(out, url+": the URL answered 200 OK") {
		t.Fatalf("arc relay ring test:\n%s", out)
	}
	if hit := waitRing(t, hits, "the test ring"); hit.auth != "Bearer "+key {
		t.Fatalf("the test ring sent Authorization %q", hit.auth)
	}
	if out := ok(t, bob, "", "relay", "ring", "show"); !strings.Contains(out, "47 of 48 left today, until ") || !strings.Contains(out, "; last ring ") {
		t.Fatalf("arc relay ring show:\n%s", out)
	}

	// Mail from alice rings at once: the test ring holds nothing back.
	ok(t, alice, "", "message", "send", bobHex, "are you awake?")
	ok(t, alice, "", "sync")
	hit := waitRing(t, hits, "the first mail")
	if !strings.HasPrefix(hit.body, `{"type":"arc.mail","id":"`) || !strings.Contains(hit.body, `","citizen":"`+bobHex+`","relay":"`+url+`","since":`) {
		t.Fatalf("the ring body is %s", hit.body)
	}
	if strings.Contains(hit.body, "awake") {
		t.Fatal("the ring carried the mail")
	}

	ok(t, alice, "", "message", "send", bobHex, "hello?")
	ok(t, alice, "", "sync")
	noRing(t, hits, "a second mail before bob read")

	// arc sync reads. The next mail rings again.
	ok(t, bob, "", "sync")
	ok(t, alice, "", "message", "send", bobHex, "one more")
	ok(t, alice, "", "sync")
	waitRing(t, hits, "mail after bob read")

	if out := ok(t, bob, "", "relay", "ring", "clear"); !strings.Contains(out, url+": rings no more") {
		t.Fatalf("arc relay ring clear:\n%s", out)
	}
	if _, err := run(t, bob, "", "relay", "ring", "show"); err == nil {
		t.Fatal("ring show found a registration after clear")
	}
}
