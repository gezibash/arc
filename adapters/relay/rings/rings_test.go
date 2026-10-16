package rings

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/adapters/relay/sealed"
	"github.com/gezibash/arc/adapters/relay/turn"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
	"go.etcd.io/bbolt"
)

// publicIP stands for the public address of the ring URL. The test dials
// the target server in its place.
var publicIP = netip.MustParseAddr("203.0.113.10")

type hit struct {
	auth, id, body string
	at             time.Time
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t      *testing.T
	ws     string
	http   string
	rings  *Rings
	db     *bbolt.DB
	offset atomic.Int64
	status atomic.Int32
	hits   chan hit
	logs   *safeBuffer

	mu  sync.Mutex
	dns map[string][]netip.Addr
}

func start(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{t: t, hits: make(chan hit, 100), logs: &safeBuffer{}, dns: map[string][]netip.Addr{"example.com": {publicIP}}}
	h.status.Store(200)

	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.hits <- hit{auth: r.Header.Get("Authorization"), id: r.Header.Get(IDHeader), body: string(body), at: time.Now()}
		w.WriteHeader(int(h.status.Load()))
	}))
	target.StartTLS()
	t.Cleanup(target.Close)
	roots := target.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs

	store := &boltdb.BoltBackend{Path: filepath.Join(t.TempDir(), "relay.db")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	h.db = store.DB
	rl := khatru.NewRelay()
	rl.Log = log.New(io.Discard, "", 0)
	rl.UseEventstore(store, 500)
	readers := sealed.Protect(rl)

	if cfg.Secret == "" {
		cfg.Secret = "a secret of the relay"
	}
	cfg.Readers = readers
	cfg.Now = func() time.Time { return time.Now().Add(time.Duration(h.offset.Load())) }
	cfg.Lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if addrs, ok := h.dns[host]; ok {
			return addrs, nil
		}
		return nil, errors.New("no such host")
	}
	cfg.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != netip.AddrPortFrom(publicIP, 443).String() {
			return nil, fmt.Errorf("the test dials only %s, not %s", publicIP, address)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, target.Listener.Addr().String())
	}
	cfg.TLS = &tls.Config{RootCAs: roots}
	cfg.Logger = slog.New(slog.NewTextHandler(h.logs, nil))
	rings, err := Apply(rl, store.DB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.rings = rings

	mux := http.NewServeMux()
	mux.Handle("/", rl)
	mux.Handle(Path, rings)
	mux.Handle(TestPath, rings)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	h.http = server.URL
	h.ws = "ws" + strings.TrimPrefix(server.URL, "http")
	return h
}

func (h *harness) advance(d time.Duration) { h.offset.Add(int64(d)) }

// tick waits for the rings on the way, fires each timer now, as if its
// time came on the clock of the test, and waits for the rings that it
// started.
func (h *harness) tick() {
	h.rings.sends.Wait()
	h.rings.mu.Lock()
	var due []nostr.PubKey
	for pk := range h.rings.timers {
		due = append(due, pk)
	}
	h.rings.mu.Unlock()
	for _, pk := range due {
		h.rings.wake(pk)
	}
	h.rings.sends.Wait()
}

func (h *harness) calls() []hit {
	var out []hit
	for {
		select {
		case c := <-h.hits:
			out = append(out, c)
		default:
			return out
		}
	}
}

// do sends a signed request to the rings of the relay.
func (h *harness) do(method, path string, signer keys.Key, body []byte, header http.Header) (int, string) {
	h.t.Helper()
	endpoint := h.http + path
	authorization, err := turn.AuthHeaderBody(context.Background(), signer, method, endpoint, body)
	if err != nil {
		h.t.Fatal(err)
	}
	request, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	request.Header.Set("Authorization", authorization)
	maps.Copy(request.Header, header)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func (h *harness) register(citizen keys.Key, request Request) (int, string) {
	h.t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.do(http.MethodPut, Path, citizen, body, nil)
}

const ringKey = "a ring key of the platform"

func (h *harness) mustRegister(citizen keys.Key) {
	h.t.Helper()
	if code, body := h.register(citizen, Request{URL: "https://example.com/hook", Key: ringKey}); code != http.StatusOK {
		h.t.Fatalf("register: %d %s", code, body)
	}
}

// mail sends a wrap of a stranger to the citizen, on a connection with no
// authentication.
func (h *harness) mail(to nostr.PubKey, form private.Form, kind nostr.Kind) {
	h.t.Helper()
	ctx := context.Background()
	author := keys.Generate()
	rumor := private.Rumor(author, 14, "hello", nil, time.Now())
	wrap, err := private.Wrap(ctx, author, to, rumor, form, kind, time.Now().Add(time.Hour))
	if err != nil {
		h.t.Fatal(err)
	}
	conn, err := nostr.RelayConnect(ctx, h.ws, nostr.RelayOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	err = conn.Publish(ctx, wrap)
	if err != nil && !(kind == private.LiveWrapKind && strings.Contains(err.Error(), "no one was listening")) {
		h.t.Fatal(err)
	}
}

// subscribe opens a subscription for the wraps of the citizen. If auth is
// set, the connection first authenticates with it.
func (h *harness) subscribe(citizen nostr.PubKey, auth *keys.Key) *nostr.Relay {
	h.t.Helper()
	ctx := context.Background()
	conn, err := nostr.RelayConnect(ctx, h.ws, nostr.RelayOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = conn.Close() })
	if auth != nil {
		authenticate(h.t, conn, *auth)
	}
	sub, err := conn.Subscribe(ctx, nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {citizen.Hex()}}}, nostr.SubscriptionOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	waitStored(h.t, sub)
	return conn
}

// waitStored reads the stored events of a subscription until its end of
// stored events: the library gives the end only after them.
func waitStored(t *testing.T, sub *nostr.Subscription) {
	t.Helper()
	for {
		select {
		case <-sub.Events:
		case <-sub.EndOfStoredEvents:
			return
		case <-time.After(5 * time.Second):
			t.Fatal("no end of stored events")
		}
	}
}

// read is one read of the citizen: a subscription that ends.
func (h *harness) read(citizen keys.Key) {
	h.t.Helper()
	conn := h.subscribe(citizen.Public, &citizen)
	_ = conn.Close()
	time.Sleep(50 * time.Millisecond)
}

func authenticate(t *testing.T, conn *nostr.Relay, key keys.Key) {
	t.Helper()
	var err error
	for range 40 {
		if err = conn.Auth(context.Background(), key.SignEvent); err == nil || !strings.Contains(err.Error(), "no challenge") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestARelayRingsOnceForEachUnreadPeriod(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)

	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	arrived := h.rings.cfg.Now().Unix()
	h.advance(2 * time.Second)
	h.tick()
	calls := h.calls()
	if len(calls) != 1 {
		t.Fatalf("the first wrap made %d rings, want 1", len(calls))
	}
	if calls[0].auth != "Bearer "+ringKey {
		t.Fatalf("the ring sent Authorization %q", calls[0].auth)
	}
	if len(calls[0].id) != 32 {
		t.Fatalf("the ring has the ID %q, want 32 hex characters", calls[0].id)
	}
	want := fmt.Sprintf(`{"type":"arc.mail","id":"%s","citizen":"%s","relay":"%s","since":%d}`, calls[0].id, bob.Public.Hex(), h.ws, arrived)
	if calls[0].body != want {
		t.Fatalf("the body is\n%s\nwant\n%s", calls[0].body, want)
	}

	// More mail in the same unread period rings no more.
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(time.Hour)
	h.tick()
	if n := len(h.calls()); n != 0 {
		t.Fatalf("a second wrap of one unread period made %d rings", n)
	}

	// A read ends the period, and the next wrap rings.
	h.read(bob)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(2 * time.Second)
	h.tick()
	if n := len(h.calls()); n != 1 {
		t.Fatalf("a wrap after a read made %d rings, want 1", n)
	}
}

func TestCourierAndLiveWrapsNeverRing(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	h.subscribe(bob.Public, nil) // a listener, so the live wrap goes out

	h.mail(bob.Public, private.CourierForm, private.WrapKind)
	h.mail(bob.Public, private.RelayForm, private.LiveWrapKind)
	h.advance(time.Hour)
	h.tick()
	if n := len(h.calls()); n != 0 {
		t.Fatalf("courier-form and live wraps made %d rings", n)
	}
}

func TestAReadNeedsAuthentication(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(2 * time.Second)
	h.tick()
	h.calls()

	// A query with no authentication, and a query by another key, end no
	// unread period.
	stranger := keys.Generate()
	_ = h.subscribe(bob.Public, nil).Close()
	_ = h.subscribe(bob.Public, &stranger).Close()
	time.Sleep(50 * time.Millisecond)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(time.Hour)
	h.tick()
	if n := len(h.calls()); n != 0 {
		t.Fatalf("a read that did not authenticate as bob made %d rings", n)
	}
}

func TestADeviceWithATokenReads(t *testing.T) {
	h := start(t, Config{})
	bob, device := keys.Generate(), keys.Generate()
	h.mustRegister(bob)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(2 * time.Second)
	h.tick()
	h.calls()

	ctx := context.Background()
	token := devices.NewToken(device.Public, nostr.Now())
	if err := bob.SignEvent(ctx, &token); err != nil {
		t.Fatal(err)
	}
	conn, err := nostr.RelayConnect(ctx, h.ws, nostr.RelayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	authenticate(t, conn, device)
	if err := conn.Publish(ctx, token); err != nil && !strings.Contains(err.Error(), "no one was listening") {
		t.Fatal(err)
	}
	sub, err := conn.Subscribe(ctx, nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {bob.Public.Hex()}}}, nostr.SubscriptionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitStored(t, sub)
	sub.Unsub()
	time.Sleep(50 * time.Millisecond)

	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(time.Hour)
	h.tick()
	if n := len(h.calls()); n != 1 {
		t.Fatalf("a wrap after the read of a device made %d rings, want 1", n)
	}
}

func TestAWatchingCitizenIsNotRung(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	conn := h.subscribe(bob.Public, &bob)

	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(time.Hour)
	h.tick()
	if n := len(h.calls()); n != 0 {
		t.Fatalf("a wrap that an open subscription of bob got made %d rings", n)
	}

	// When the watch ends, the next wrap rings.
	_ = conn.Close()
	time.Sleep(50 * time.Millisecond)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(2 * time.Second)
	h.tick()
	if n := len(h.calls()); n != 1 {
		t.Fatalf("a wrap after the watch ended made %d rings, want 1", n)
	}
}

func TestTheRelayRefusesARegistrationThatBreaksARule(t *testing.T) {
	h := start(t, Config{DailyCap: 48})
	h.dns["inside.example"] = []netip.Addr{netip.MustParseAddr("10.0.0.5")}
	h.dns["mixed.example"] = []netip.Addr{publicIP, netip.MustParseAddr("fd00::1")}
	h.dns["mapped.example"] = []netip.Addr{netip.MustParseAddr("::ffff:127.0.0.1")}
	bob := keys.Generate()
	ok := Request{URL: "https://example.com/hook", Key: ringKey}
	cases := map[string]func(*Request){
		"http":                  func(r *Request) { r.URL = "http://example.com/hook" },
		"loopback":              func(r *Request) { r.URL = "https://127.0.0.1/hook" },
		"private address":       func(r *Request) { r.URL = "https://10.1.2.3/hook" },
		"link-local":            func(r *Request) { r.URL = "https://[fe80::1]/hook" },
		"metadata address":      func(r *Request) { r.URL = "https://169.254.169.254/latest" },
		"shared address space":  func(r *Request) { r.URL = "https://100.100.100.100/hook" },
		"unspecified":           func(r *Request) { r.URL = "https://0.0.0.0/hook" },
		"multicast":             func(r *Request) { r.URL = "https://224.0.0.1/hook" },
		"host that is private":  func(r *Request) { r.URL = "https://inside.example/hook" },
		"host with one private": func(r *Request) { r.URL = "https://mixed.example/hook" },
		"mapped loopback":       func(r *Request) { r.URL = "https://mapped.example/hook" },
		"host that is unknown":  func(r *Request) { r.URL = "https://nowhere.example/hook" },
		"port 8443":             func(r *Request) { r.URL = "https://example.com:8443/hook" },
		"user in the URL":       func(r *Request) { r.URL = "https://me@example.com/hook" },
		"key of 15 bytes":       func(r *Request) { r.Key = strings.Repeat("k", 15) },
		"key of 513 bytes":      func(r *Request) { r.Key = strings.Repeat("k", 513) },
		"interval of 59 s":      func(r *Request) { r.MinInterval = 59 },
		"interval of 86401 s":   func(r *Request) { r.MinInterval = 86401 },
		"daily cap of 49":       func(r *Request) { r.DailyCap = 49 },
		"daily cap below 1":     func(r *Request) { r.DailyCap = -1 },
		"ring on stamped":       func(r *Request) { r.RingOn = RingOnStamped },
		"ring on other":         func(r *Request) { r.RingOn = "sometimes" },
	}
	for name, change := range cases {
		request := ok
		change(&request)
		if code, body := h.register(bob, request); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422", name, code, body)
		}
	}
	if code, body := h.do(http.MethodGet, Path, bob, nil, nil); code != http.StatusNotFound {
		t.Fatalf("a refused registration was kept: %d %s", code, body)
	}

	// The limits themselves pass, and the defaults fill in.
	request := ok
	request.Key = strings.Repeat("k", 16)
	request.MinInterval = 60
	if code, body := h.register(bob, request); code != http.StatusOK {
		t.Fatalf("the smallest key and interval: %d %s", code, body)
	}
	var status Status
	_, body := h.do(http.MethodGet, Path, bob, nil, nil)
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	if status.MinInterval != 60 || status.DailyCap != 48 || status.RingOn != RingOnAny || status.Left != 48 {
		t.Fatalf("the registration shows %+v", status)
	}
	request = ok
	if _, body := h.register(bob, request); !strings.Contains(body, `"min_interval":300`) {
		t.Fatalf("the default interval is not 300 s: %s", body)
	}
}

func TestAHostThatTurnsPrivateIsNotRung(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	h.mu.Lock()
	h.dns["example.com"] = []netip.Addr{netip.MustParseAddr("192.168.1.7")}
	h.mu.Unlock()

	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(2 * time.Second)
	h.tick()
	if n := len(h.calls()); n != 0 {
		t.Fatalf("a host that resolves to a private address got %d rings", n)
	}
	_, body := h.do(http.MethodGet, Path, bob, nil, nil)
	if !strings.Contains(body, `"last_status":0`) {
		t.Fatalf("the ring did not fail: %s", body)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "failed=true") || strings.Contains(logs, "example.com") || strings.Contains(logs, "192.168") {
		t.Fatalf("the log of a failed ring names the host, or no failure:\n%s", logs)
	}
}

func TestABadAuthorizationGets401(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	ctx := context.Background()
	sign := func(event nostr.Event, key keys.Key) string {
		if err := key.SignEvent(ctx, &event); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(event)
		return "Nostr " + base64.StdEncoding.EncodeToString(data)
	}
	event := func(at nostr.Timestamp, method, path string) nostr.Event {
		return nostr.Event{Kind: turn.KindHTTPAuth, CreatedAt: at, Tags: nostr.Tags{{"u", h.http + path}, {"method", method}}}
	}
	forged := func() string {
		e := event(nostr.Now(), http.MethodGet, Path)
		if err := bob.SignEvent(ctx, &e); err != nil {
			t.Fatal(err)
		}
		e.PubKey = keys.Generate().Public
		data, _ := json.Marshal(e)
		return "Nostr " + base64.StdEncoding.EncodeToString(data)
	}
	cases := map[string]string{
		"none":           "",
		"old":            sign(event(nostr.Now()-120, http.MethodGet, Path), bob),
		"another path":   sign(event(nostr.Now(), http.MethodGet, "/turn"), bob),
		"another method": sign(event(nostr.Now(), http.MethodDelete, Path), bob),
		"another key":    forged(),
	}
	for name, authorization := range cases {
		request, _ := http.NewRequest(http.MethodGet, h.http+Path, nil)
		request.Header.Set("Authorization", authorization)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, response.StatusCode)
		}
	}

	// A body that changed after the signature.
	signed, _ := json.Marshal(Request{URL: "https://example.com/hook", Key: ringKey})
	authorization, _ := turn.AuthHeaderBody(ctx, bob, http.MethodPut, h.http+Path, signed)
	changed := bytes.Replace(signed, []byte("example.com/hook"), []byte("example.com/evil"), 1)
	request, _ := http.NewRequest(http.MethodPut, h.http+Path, bytes.NewReader(changed))
	request.Header.Set("Authorization", authorization)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a changed body: %d, want 401", response.StatusCode)
	}
}

func TestTheKeyNeverLeavesTheRelay(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	h.status.Store(500)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(2 * time.Second)
	h.tick()
	h.calls()

	_, shown := h.do(http.MethodGet, Path, bob, nil, nil)
	_, tested := h.do(http.MethodPost, TestPath, bob, nil, nil)
	var stored bytes.Buffer
	_ = h.db.View(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(_ []byte, b *bbolt.Bucket) error {
			return b.ForEach(func(k, v []byte) error {
				stored.Write(k)
				stored.Write(v)
				return nil
			})
		})
	})
	for name, text := range map[string]string{"GET /ring": shown, "POST /ring/test": tested, "the log": h.logs.String(), "the store": stored.String()} {
		if strings.Contains(text, ringKey) || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(ringKey))) {
			t.Errorf("%s holds the ring key", name)
		}
		if strings.Contains(text, "example.com") && name == "the log" {
			t.Errorf("the log names the ring URL")
		}
	}
	if !strings.Contains(stored.String(), `"sealed_key"`) {
		t.Fatal("the store holds no sealed key: the test reads the wrong bucket")
	}
}

func TestANewRegistrationKeepsTheCountOfTheDay(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	request := Request{URL: "https://example.com/hook", DailyCap: 1, Key: ringKey}
	if code, body := h.register(bob, request); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if code, body := h.do(http.MethodPost, TestPath, bob, nil, nil); code != http.StatusOK || !strings.Contains(body, `"status":200`) {
		t.Fatalf("the test ring: %d %s", code, body)
	}
	if n := len(h.calls()); n != 1 {
		t.Fatalf("the test made %d rings, want 1", n)
	}
	h.register(bob, request)
	if code, _ := h.do(http.MethodPost, TestPath, bob, nil, nil); code != http.StatusTooManyRequests {
		t.Fatalf("a second test ring on a day with daily_cap 1: %d, want 429", code)
	}
}

func TestTheRelayCapsItsRingsInEachMinute(t *testing.T) {
	h := start(t, Config{PerMinute: 1})
	bob, carol := keys.Generate(), keys.Generate()
	h.mustRegister(bob)
	h.mustRegister(carol)
	// Start at the beginning of a minute, so that both ticks fall in it.
	now := h.rings.cfg.Now()
	h.advance(now.Truncate(time.Minute).Add(time.Minute).Sub(now))
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.mail(carol.Public, private.RelayForm, private.WrapKind)
	h.advance(2 * time.Second)
	h.tick()
	h.tick()
	if n := len(h.calls()); n != 1 {
		t.Fatalf("a minute with a cap of 1 made %d rings", n)
	}
	h.advance(time.Minute)
	h.tick()
	if n := len(h.calls()); n != 1 {
		t.Fatalf("the next minute made %d rings, want the held one", n)
	}
}

func TestADeleteEndsTheRings(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	if code, _ := h.do(http.MethodDelete, Path, bob, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.advance(time.Hour)
	h.tick()
	if n := len(h.calls()); n != 0 {
		t.Fatalf("a deleted registration got %d rings", n)
	}
}

func TestADeviceRegistersForItsCitizen(t *testing.T) {
	h := start(t, Config{})
	bob, device := keys.Generate(), keys.Generate()
	token := devices.NewToken(device.Public, nostr.Now())
	if err := bob.SignEvent(context.Background(), &token); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(token)
	header := http.Header{TokenHeader: {base64.StdEncoding.EncodeToString(data)}}
	body, _ := json.Marshal(Request{URL: "https://example.com/hook", Key: ringKey})
	if code, text := h.do(http.MethodPut, Path, device, body, header); code != http.StatusOK || !strings.Contains(text, bob.Public.Hex()) {
		t.Fatalf("a device with a token: %d %s", code, text)
	}
	if code, _ := h.do(http.MethodGet, Path, bob, nil, nil); code != http.StatusOK {
		t.Fatalf("the citizen does not see the registration of its device: %d", code)
	}

	// A token for another device does not pass.
	other := keys.Generate()
	if code, _ := h.do(http.MethodGet, Path, other, nil, header); code != http.StatusUnauthorized {
		t.Fatalf("a token of another device: %d, want 401", code)
	}
}

func TestRegistrationsOutliveARestart(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	again, err := Apply(khatru.NewRelay(), h.db, Config{Secret: "a secret of the relay", Lookup: h.rings.cfg.Lookup})
	if err != nil {
		t.Fatal(err)
	}
	s := again.by[bob.Public]
	if s == nil {
		t.Fatal("the registration did not survive")
	}
	key, err := again.open(s.SealedKey)
	if err != nil || string(key) != ringKey {
		t.Fatalf("the key opens as %q, %v", key, err)
	}
	if _, err := Apply(khatru.NewRelay(), h.db, Config{Secret: "another secret"}); err != nil {
		t.Fatal(err)
	}
}

// The default dialer checks the address that it connects to, which the
// other tests replace.
func TestTheDialerChecksTheAddressItConnectsTo(t *testing.T) {
	r := &Rings{own: ownAddrs([]netip.Addr{netip.MustParseAddr("198.51.100.7")})}
	for _, address := range []string{"127.0.0.1:443", "[::1]:443", "10.0.0.1:443", "[fd00::1]:443", "169.254.169.254:443", "198.51.100.7:443", "[::ffff:192.168.0.1]:443"} {
		if r.control("tcp", address, nil) == nil {
			t.Errorf("the dialer connects to %s", address)
		}
	}
	if err := r.control("tcp", "203.0.113.10:443", nil); err != nil {
		t.Errorf("the dialer refuses a public address: %v", err)
	}
}

func TestAWrapRingsWithinMilliseconds(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	for i := range 5 {
		sent := time.Now()
		h.mail(bob.Public, private.RelayForm, private.WrapKind)
		select {
		case hit := <-h.hits:
			took := hit.at.Sub(sent)
			if took > latencyBudget {
				t.Fatalf("ring %d came %s after the publish, want under %s", i+1, took, latencyBudget)
			}
			t.Logf("ring %d came %s after the publish", i+1, took.Round(100*time.Microsecond))
		case <-time.After(5 * time.Second):
			t.Fatalf("ring %d did not come: the relay waits for something", i+1)
		}
		h.rings.sends.Wait()
		h.read(bob)
		// The bucket holds 3 rings, and gets one back each 300 s.
		h.advance(5 * time.Minute)
	}
}

func TestATestRingDelaysNoMail(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	if code, body := h.do(http.MethodPost, TestPath, bob, nil, nil); code != http.StatusOK {
		t.Fatalf("test ring: %d %s", code, body)
	}
	h.calls()
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	select {
	case <-h.hits:
	case <-time.After(time.Second):
		t.Fatal("mail after a test ring did not ring at once")
	}
}

func TestARingTriesAgainWithOneIDAcrossARestart(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	h.mustRegister(bob)
	h.status.Store(503)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.rings.sends.Wait()
	first := h.calls()
	if len(first) != 1 {
		t.Fatalf("the first try made %d calls", len(first))
	}

	// The relay restarts. The store holds the job, and the new relay tries
	// it again with the same ID, after the backoff.
	again, err := Apply(khatru.NewRelay(), h.db, h.rings.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.rings = again
	h.tick()
	if n := len(h.calls()); n != 0 {
		t.Fatalf("the retry came before its backoff: %d calls", n)
	}
	var ids []string
	for range MaxAttempts - 1 {
		h.advance(10 * time.Minute)
		h.tick()
		for _, c := range h.calls() {
			ids = append(ids, c.id)
		}
	}
	if len(ids) != MaxAttempts-1 {
		t.Fatalf("the retries made %d calls, want %d", len(ids), MaxAttempts-1)
	}
	for _, id := range ids {
		if id != first[0].id {
			t.Fatalf("a retry has the ID %s, want %s", id, first[0].id)
		}
	}
	h.advance(time.Hour)
	h.tick()
	if n := len(h.calls()); n != 0 {
		t.Fatalf("a try came after %d tries", MaxAttempts)
	}
}

func TestARegistrationExpires(t *testing.T) {
	h := start(t, Config{TTL: time.Hour})
	bob := keys.Generate()
	h.mustRegister(bob)
	_, body := h.do(http.MethodGet, Path, bob, nil, nil)
	var status Status
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	if want := h.rings.cfg.Now().Add(time.Hour).Unix(); status.Expires < want-2 || status.Expires > want {
		t.Fatalf("the registration expires at %d, want about %d", status.Expires, want)
	}

	h.advance(2 * time.Hour)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.tick()
	if n := len(h.calls()); n != 0 {
		t.Fatalf("an expired registration got %d rings", n)
	}
	h.rings.mu.Lock()
	h.rings.sweep()
	_, kept := h.rings.by[bob.Public]
	h.rings.mu.Unlock()
	if kept {
		t.Fatal("the sweep kept an expired registration")
	}
}

func TestARegistrationOfVersion0330Rings(t *testing.T) {
	h := start(t, Config{})
	bob := keys.Generate()
	sealed, err := h.rings.seal(ringKey)
	if err != nil {
		t.Fatal(err)
	}
	// A record as v0.33.0 wrote it: no burst, no expiry, no bucket.
	old := map[string]any{
		"url": "https://example.com/hook", "min_interval": 300, "daily_cap": 48, "ring_on": "any",
		"sealed_key": sealed, "relay": h.ws, "created": h.rings.cfg.Now().Add(-time.Hour).Unix(),
		"last_read": h.rings.cfg.Now().Add(-time.Hour).Unix(), "last_ring": h.rings.cfg.Now().Add(-time.Minute).Unix(),
	}
	data, _ := json.Marshal(old)
	if err := h.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketName)).Put([]byte(bob.Public.Hex()), data)
	}); err != nil {
		t.Fatal(err)
	}
	again, err := Apply(khatru.NewRelay(), h.db, h.rings.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := again.by[bob.Public]
	if s.Burst != DefaultBurst || s.Expires != time.Unix(s.Created, 0).Add(DefaultTTL).Unix() {
		t.Fatalf("the old record loads with burst %d and expiry %d", s.Burst, s.Expires)
	}
	// A ring a minute ago does not hold back the next one.
	again.mu.Lock()
	s.wrap(again.cfg.Now(), false, "")
	what, _ := s.next(again.cfg.Now())
	again.mu.Unlock()
	if what != send {
		t.Fatalf("an old registration with a recent ring does not ring at once: %v", what)
	}
}

// expiresIn is how long the registration of a citizen still lives on the
// clock of the relay.
func (h *harness) expiresIn(pk nostr.PubKey) time.Duration {
	h.rings.mu.Lock()
	defer h.rings.mu.Unlock()
	s := h.rings.by[pk]
	if s == nil {
		return 0
	}
	return time.Unix(s.Expires, 0).Sub(h.rings.cfg.Now())
}

func TestAReadRenewsTheRegistration(t *testing.T) {
	h := start(t, Config{TTL: time.Hour})
	bob, stranger := keys.Generate(), keys.Generate()
	h.mustRegister(bob)

	// 50 minutes on, bob reads: his registration lives an hour from then.
	h.advance(50 * time.Minute)
	h.read(bob)
	if left := h.expiresIn(bob.Public); left < 59*time.Minute {
		t.Fatalf("after a read the registration lives %s, want an hour", left)
	}

	// A query that did not authenticate as bob renews nothing.
	h.advance(50 * time.Minute)
	_ = h.subscribe(bob.Public, &stranger).Close()
	time.Sleep(50 * time.Millisecond)
	if left := h.expiresIn(bob.Public); left > 11*time.Minute {
		t.Fatalf("a query by another key renewed the registration: %s left", left)
	}

	// 100 minutes after the PUT, a wrap still rings, because bob read.
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.tick()
	if n := len(h.calls()); n != 1 {
		t.Fatalf("a wrap 100 minutes after the PUT, 50 after a read, made %d rings, want 1", n)
	}

	// An open watch keeps the registration alive: the sweep renews it.
	watch := h.subscribe(bob.Public, &bob)
	h.advance(50 * time.Minute)
	h.rings.mu.Lock()
	h.rings.sweep()
	h.rings.mu.Unlock()
	if left := h.expiresIn(bob.Public); left < 59*time.Minute {
		t.Fatalf("the sweep left a watching citizen %s, want an hour", left)
	}
	_ = watch.Close()
	time.Sleep(50 * time.Millisecond)

	// With no read for longer than the TTL, the registration ends.
	h.advance(2 * time.Hour)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.tick()
	if n := len(h.calls()); n != 0 {
		t.Fatalf("a citizen that stopped reading for 2 hours got %d rings", n)
	}
}

// publish sends one wrap to the relay, on a connection with no
// authentication.
func (h *harness) publish(wrap nostr.Event) {
	h.t.Helper()
	ctx := context.Background()
	conn, err := nostr.RelayConnect(ctx, h.ws, nostr.RelayOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Publish(ctx, wrap); err != nil {
		h.t.Fatal(err)
	}
}

// TestTwoRelaysRingWithOneID gives the same wrap to two relays of one
// citizen, as a sender does. Both ring with the same ID, the one that
// RingID gives for the wrap, so the URL can drop the second ring.
func TestTwoRelaysRingWithOneID(t *testing.T) {
	one, two := start(t, Config{}), start(t, Config{})
	bob := keys.Generate()
	one.mustRegister(bob)
	two.mustRegister(bob)

	ctx := context.Background()
	author := keys.Generate()
	rumor := private.Rumor(author, 14, "to both relays", nil, time.Now())
	wrap, err := private.Wrap(ctx, author, bob.Public, rumor, private.RelayForm, private.WrapKind, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	one.publish(wrap)
	two.publish(wrap)
	one.rings.sends.Wait()
	two.rings.sends.Wait()

	first, second := one.calls(), two.calls()
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("the relays made %d and %d rings, want 1 each", len(first), len(second))
	}
	want := RingID(bob.Public.Hex(), wrap.ID.Hex())
	if first[0].id != want || second[0].id != want {
		t.Fatalf("the rings have IDs %s and %s, want %s on both", first[0].id, second[0].id, want)
	}
	if !strings.Contains(first[0].body, `"id":"`+want+`"`) {
		t.Fatalf("the body names another ID: %s", first[0].body)
	}
}

// TestAReadOfACitizenWithFailingRingsGetsANotice rings a URL that answers
// 401, as a platform does after its key changed. The relay tries once, and
// tells the citizen on its next authenticated read, once on that
// connection. A stranger's read gets nothing.
func TestAReadOfACitizenWithFailingRingsGetsANotice(t *testing.T) {
	h := start(t, Config{})
	bob, stranger := keys.Generate(), keys.Generate()
	h.mustRegister(bob)
	h.status.Store(401)
	h.mail(bob.Public, private.RelayForm, private.WrapKind)
	h.rings.sends.Wait()
	h.advance(time.Hour)
	h.tick()
	if n := len(h.calls()); n != 1 {
		t.Fatalf("a ring answered 401 made %d calls, want 1: a retry cannot fix the key", n)
	}

	read := func(as keys.Key, times int) []string {
		var mu sync.Mutex
		var notices []string
		ctx := context.Background()
		conn, err := nostr.RelayConnect(ctx, h.ws, nostr.RelayOptions{NoticeHandler: func(_ *nostr.Relay, text string) {
			mu.Lock()
			notices = append(notices, text)
			mu.Unlock()
		}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		authenticate(t, conn, as)
		for range times {
			sub, err := conn.Subscribe(ctx, nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {bob.Public.Hex()}}}, nostr.SubscriptionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			waitStored(t, sub)
			sub.Unsub()
		}
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return notices
	}

	if got := read(stranger, 1); len(got) != 0 {
		t.Fatalf("a stranger's read got %q", got)
	}
	got := read(bob, 3)
	if len(got) != 1 || !strings.HasPrefix(got[0], "arc-ring: the rings to example.com fail since ") || !strings.Contains(got[0], "last answer 401, 1 failed tries") {
		t.Fatalf("bob's three reads on one connection got %q, want one notice of the 401", got)
	}
	if strings.Contains(got[0], "/hook") {
		t.Fatalf("the notice names the path of the URL: %s", got[0])
	}
}
