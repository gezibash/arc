package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	neturl "net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/coder/websocket"
	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/rings"
)

type ringHit struct{ auth, body string }

// ringRelay runs a relay that rings, and a TLS server that stands for the
// ring URL https://example.com/hook. It returns the relay URL and the calls
// to the ring URL.
func ringRelay(t *testing.T, status int) (string, chan ringHit) {
	t.Helper()
	hits := make(chan ringHit, 20)
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits <- ringHit{auth: r.Header.Get("Authorization"), body: string(body)}
		w.WriteHeader(status)
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
	url, hits := ringRelay(t, http.StatusOK)

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

// With --relay, each ring command acts on one relay. Bob has two relays
// that ring, and registers with the first only: the second has no
// registration, and test, show and clear touch the first only. A relay
// that --relay names and that offers no rings fails the command.
func TestARingCommandActsOnOneRelay(t *testing.T) {
	first, firstHits := ringRelay(t, http.StatusOK)
	second, secondHits := ringRelay(t, http.StatusOK)
	bob, _ := providerHome(t, first)
	ok(t, bob, "", "relay", "add", second)
	t.Setenv("ARC_RING_KEY", "a key of the routine of bob")

	set := ok(t, bob, "", "relay", "ring", "set", "https://example.com/hook", "--key-env", "ARC_RING_KEY", "--relay", first)
	if !strings.Contains(set, first+": rings example.com") || strings.Contains(set, second) {
		t.Fatalf("arc relay ring set --relay %s:\n%s", first, set)
	}
	if out, err := run(t, bob, "", "relay", "ring", "show", "--relay", second); err == nil {
		t.Fatalf("the second relay has a registration:\n%s", out)
	}
	if out := ok(t, bob, "", "relay", "ring", "test", "--relay", first); !strings.Contains(out, first+": the URL answered 200 OK") || strings.Contains(out, second) {
		t.Fatalf("arc relay ring test --relay %s:\n%s", first, out)
	}
	waitRing(t, firstHits, "the test ring of the first relay")
	noRing(t, secondHits, "a test ring of the first relay only")
	if out := ok(t, bob, "", "relay", "ring", "show", "--relay", first); !strings.Contains(out, "47 of 48 left today") || strings.Contains(out, second) {
		t.Fatalf("arc relay ring show --relay %s:\n%s", first, out)
	}
	if out := ok(t, bob, "", "relay", "ring", "clear", "--relay", first); out != first+": rings no more\n" {
		t.Fatalf("arc relay ring clear --relay %s:\n%s", first, out)
	}
	if _, err := run(t, bob, "", "relay", "ring", "show", "--relay", first); err == nil {
		t.Fatal("ring show found a registration after clear")
	}

	// A given but empty --relay, as from an unset variable in a script,
	// must fail and touch no relay.
	ok(t, bob, "", "relay", "ring", "set", "https://example.com/hook", "--key-env", "ARC_RING_KEY")
	for _, args := range [][]string{{"--relay", ""}, {"--relay="}} {
		out, err := run(t, bob, "", append([]string{"relay", "ring", "clear"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "--relay needs a URL") {
			t.Fatalf("ring clear %q: %v\n%s", args, err, out)
		}
	}
	for _, relay := range []string{first, second} {
		if out, err := run(t, bob, "", "relay", "ring", "show", "--relay", relay); err != nil {
			t.Fatalf("an empty --relay cleared %s: %v\n%s", relay, err, out)
		}
	}

	plain, _, _, closePlain, err := newRelay(filepath.Join(t.TempDir(), "plain.db"), limits.Policy{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePlain)
	plainServer := httptest.NewServer(relayHandler(plain, nil, nil))
	t.Cleanup(plainServer.Close)
	plainURL := "ws" + strings.TrimPrefix(plainServer.URL, "http")
	if out, err := run(t, bob, "", "relay", "ring", "show", "--relay", plainURL); err == nil || !strings.Contains(err.Error(), plainURL+" offers no rings") || strings.Contains(out, "skipped") {
		t.Fatalf("ring show --relay on a relay without rings: %v\n%s", err, out)
	}
	if _, err := run(t, bob, "", "relay", "ring", "show", "--relay", "https://example.com"); err == nil || !strings.Contains(err.Error(), "ws:// or wss://") {
		t.Fatalf("ring show took an https relay: %v", err)
	}
}

// TestSyncSaysThatRingsFail registers a URL that answers 401, as a
// platform does after its key changed. The next arc sync prints the notice
// of the relay on standard error, with the relay.
func TestSyncSaysThatRingsFail(t *testing.T) {
	url, hits := ringRelay(t, http.StatusUnauthorized)
	bob, bobHex := providerHome(t, url)
	alice, _ := providerHome(t, url)
	t.Setenv("ARC_RING_KEY", "a key that the platform no longer takes")
	ok(t, bob, "", "relay", "ring", "set", "https://example.com/hook", "--key-env", "ARC_RING_KEY")

	ok(t, alice, "", "message", "send", bobHex, "are you there?")
	ok(t, alice, "", "sync")
	waitRing(t, hits, "the ring that gets 401")

	var stderr strings.Builder
	command := root()
	command.SetOut(&strings.Builder{})
	command.SetErr(&stderr)
	command.SetArgs([]string{"--home", bob, "sync"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	want := url + ": arc-ring: the rings to example.com fail since "
	if !strings.Contains(stderr.String(), want) || !strings.Contains(stderr.String(), "last answer 401") {
		t.Fatalf("arc sync said on standard error:\n%s\nwant a line starting %q", stderr.String(), want)
	}
}

// TestSyncSaysThatRingsRunLow registers with a daily cap of 2 and uses one
// ring with ring test. The next arc sync prints the notice of the relay on
// standard error: 1 of 2 rings left today.
func TestSyncSaysThatRingsRunLow(t *testing.T) {
	url, hits := ringRelay(t, http.StatusOK)
	bob, _ := providerHome(t, url)
	t.Setenv("ARC_RING_KEY", "a key of the routine of bob")
	ok(t, bob, "", "relay", "ring", "set", "https://example.com/hook", "--key-env", "ARC_RING_KEY", "--daily-cap", "2")
	ok(t, bob, "", "relay", "ring", "test")
	waitRing(t, hits, "the test ring")

	var stderr strings.Builder
	command := root()
	command.SetOut(&strings.Builder{})
	command.SetErr(&stderr)
	command.SetArgs([]string{"--home", bob, "sync"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := url + ": arc-ring: 1 of 2 rings left today; then rings wait until 00:00 UTC\n"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("arc sync said on standard error:\n%s\nwant %q", stderr.String(), want)
	}
}

// refuseAuth runs a proxy in front of the relay at url, and returns its
// URL. The proxy passes each message on, but answers each AUTH itself with
// a refusal that gives reason, a text that the relay chose. Each other HTTP
// request, such as the one for the NIP-11 document, goes to the relay.
func refuseAuth(t *testing.T, url, reason string) string {
	t.Helper()
	target, err := neturl.Parse("http" + strings.TrimPrefix(url, "ws"))
	if err != nil {
		t.Fatal(err)
	}
	plain := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			plain.ServeHTTP(w, r)
			return
		}
		client, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = client.CloseNow() }()
		ctx := r.Context()
		// The proxy sends its own challenge at once: on a loaded machine
		// the challenge of the relay can come after arc stops waiting.
		if client.Write(ctx, websocket.MessageText, []byte(`["AUTH","a challenge of the proxy"]`)) != nil {
			return
		}
		upstream, response, err := websocket.Dial(ctx, url, nil)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err != nil {
			return
		}
		defer func() { _ = upstream.CloseNow() }()
		client.SetReadLimit(-1)
		upstream.SetReadLimit(-1)
		var mu sync.Mutex
		write := func(kind websocket.MessageType, data []byte) error {
			mu.Lock()
			defer mu.Unlock()
			return client.Write(ctx, kind, data)
		}
		go func() {
			defer func() { _ = client.CloseNow() }()
			for {
				kind, data, err := upstream.Read(ctx)
				if err != nil || write(kind, data) != nil {
					return
				}
			}
		}()
		for {
			kind, data, err := client.Read(ctx)
			if err != nil {
				return
			}
			var message []json.RawMessage
			var label string
			var event nostr.Event
			if json.Unmarshal(data, &message) == nil && len(message) == 2 && json.Unmarshal(message[0], &label) == nil &&
				label == "AUTH" && json.Unmarshal(message[1], &event) == nil {
				refusal, _ := json.Marshal([]any{"OK", event.ID.Hex(), false, reason})
				if write(websocket.MessageText, refusal) != nil {
					return
				}
				continue
			}
			if upstream.Write(ctx, kind, data) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// TestSyncSaysThatARelayRefusedTheAuthentication: a relay that rings
// counts a read only on a connection that authenticated (docs/rings/SPEC.md,
// section 3.3). Two relays that ring refuse the authentication of bob, with
// a text that holds terminal controls. arc sync still reads the mail, and
// prints one line for each relay, with the text escaped. The self data of
// bob needs the authentication too, so the sync ends with an error.
func TestSyncSaysThatARelayRefusedTheAuthentication(t *testing.T) {
	url, _ := ringRelay(t, http.StatusOK)
	const reason = "restricted: \x1b[31mno\u202e\nforged line"
	first, second := refuseAuth(t, url, reason), refuseAuth(t, url, reason)
	bob, bobHex := providerHome(t, first)
	ok(t, bob, "", "relay", "add", second)
	alice, _ := providerHome(t, url)
	ok(t, alice, "", "message", "send", bobHex, "are you there?")
	ok(t, alice, "", "sync")

	var stderr strings.Builder
	command := root()
	command.SetOut(&strings.Builder{})
	command.SetErr(&stderr)
	command.SetArgs([]string{"--home", bob, "sync"})
	_ = command.Execute()
	for _, relay := range []string{first, second} {
		start := relay + ": could not authenticate before it read mail, so the relay does not count the read for rings: "
		var lines []string
		for line := range strings.Lines(stderr.String()) {
			if strings.HasPrefix(line, start) {
				lines = append(lines, line)
			}
		}
		if len(lines) != 1 {
			t.Errorf("arc sync printed %d lines that start %q, want 1", len(lines), start)
		} else if want := `restricted: \x1b[31mno\u202e\nforged line` + "\n"; !strings.HasSuffix(lines[0], want) {
			t.Errorf("the line of %s does not end with the text of the relay escaped, %q", relay, want)
		}
	}
	if strings.ContainsAny(stderr.String(), "\x1b\u202e") || strings.Contains(stderr.String(), "\nforged") {
		t.Errorf("arc sync printed the text of a relay raw")
	}
	if t.Failed() {
		t.Fatalf("arc sync said on standard error:\n%s", stderr.String())
	}
	if inbox := ok(t, bob, "", "message", "inbox", "--json"); !strings.Contains(inbox, "are you there?") {
		t.Fatalf("bob did not read the mail of alice:\n%s", inbox)
	}
}
