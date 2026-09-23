package call_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/call"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/mail"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/private"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/delivery/transport/file"
	"github.com/gezibash/arc/delivery/transport/relay"
	"github.com/gezibash/arc/provider/host"
)

var echoBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "arc-call-test")
	if err != nil {
		panic(err)
	}
	echoBinary = filepath.Join(dir, "echo")
	build := exec.Command("go", "build", "-o", echoBinary, "../../provider/host/testdata/echo")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// provider starts the echo provider, and returns its server.
func provider(t *testing.T, k keys.Key) *call.Server {
	t.Helper()
	return providerWith(t, k, nil)
}

// providerWith starts the echo provider with a caller for its calls.
func providerWith(t *testing.T, k keys.Key, caller call.Caller) *call.Server {
	t.Helper()
	process, err := host.Start(echoBinary, nil, []string{"ARC_PUBLIC_KEY=" + k.Public.Hex()}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { process.Stop() })
	return call.NewServer(k, "primary", process, 64*1024, caller, quiet)
}

// handle sends one request with a body to a server, and returns the reply.
// A provider that does not answer in 10 seconds gives a provider_timeout.
func handle(t *testing.T, server *call.Server, k keys.Key, body string) call.Reply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rumor := call.RequestRumor(keys.Generate(), k.Public, call.Request{Capability: "primary", Method: "ECHO", Path: "/", Body: body}, time.Now())
	reply, err := server.Handle(ctx, rumor)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

// The echo provider calls a capability through its host. The caller gets the
// address and the body that the program wrote, and the reply goes back to
// the program.
func TestAProviderCallsThroughItsHost(t *testing.T) {
	serving := keys.Generate()
	seen := make(chan call.Outbound, 1)
	server := providerWith(t, serving, func(_ context.Context, out call.Outbound) (call.Reply, error) {
		seen <- out
		return call.Reply{Body: "pong"}, nil
	})

	if reply := handle(t, server, serving, "call sqlite+arc://k/main select 1"); reply.Body != "reply: pong" {
		t.Fatalf("reply = %+v", reply)
	}
	if out := <-seen; out.Address != "sqlite+arc://k/main" || out.Body != "select 1" {
		t.Errorf("the caller got %+v", out)
	}
}

// A refusal of the provider that got the call, and a call that the host
// could not make, reach the program apart.
func TestARefusalAndAFailureReachTheProgramApart(t *testing.T) {
	serving := keys.Generate()
	server := providerWith(t, serving, func(_ context.Context, out call.Outbound) (call.Reply, error) {
		if out.Address == "x+arc://k/refuse" {
			return call.Reply{Err: "invalid_request"}, nil
		}
		return call.Reply{}, errors.New("not_installed: install it first")
	})

	if reply := handle(t, server, serving, "call x+arc://k/refuse q"); reply.Body != "refused: invalid_request" {
		t.Errorf("a refusal gave %+v", reply)
	}
	if reply := handle(t, server, serving, "call x+arc://k/fail q"); !strings.HasPrefix(reply.Body, "failed: ") || !strings.Contains(reply.Body, "not_installed") {
		t.Errorf("a failure gave %+v", reply)
	}
}

// A server with no caller makes no call for its program.
func TestAProgramWithNoCallerCannotCall(t *testing.T) {
	serving := keys.Generate()
	reply := handle(t, provider(t, serving), serving, "call x+arc://k/ q")
	if !strings.HasPrefix(reply.Body, "failed: ") || !strings.Contains(reply.Body, "calls_off") {
		t.Errorf("reply = %+v", reply)
	}
}

func TestALiveCallOverARelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := relay.Relay{URL: testrelay.Start(t)}
	serving := keys.Generate()
	server := provider(t, serving)
	ready := make(chan struct{})
	go server.ServeLive(ctx, r, func() { close(ready) })
	<-ready

	caller := keys.Generate()
	reply, rtt, err := call.Live(ctx, caller, serving.Public, call.Request{
		Capability: "primary", Method: "ECHO", Path: "/", Body: "hello",
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Body != "ECHO / hello" || reply.Err != "" {
		t.Errorf("reply = %+v", reply)
	}
	if rtt <= 0 || rtt > 5*time.Second {
		t.Errorf("the round trip took %v", rtt)
	}
	t.Logf("live round trip over a local relay: %v", rtt)
}

// A relay can take a subscription some time after the request for it. The
// provider is ready only when the relay delivers a call to it. The relay here
// takes the provider's subscription late, and the caller's at once.
func TestALiveCallThroughARelayThatSubscribesLate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serving := keys.Generate()
	r := relay.Relay{URL: testrelay.StartSlow(t, 300*time.Millisecond, serving.Public)}
	ready := make(chan struct{})
	go provider(t, serving).ServeLive(ctx, r, func() { close(ready) })
	<-ready

	ctx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	reply, _, err := call.Live(ctx, keys.Generate(), serving.Public, call.Request{
		Capability: "primary", Method: "ECHO", Path: "/", Body: "late",
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Body != "ECHO / late" {
		t.Errorf("reply = %+v", reply)
	}
}

// The same request twice in one second is two calls. The provider refuses a
// replay of one request, not a second request with the same body.
func TestTwoEqualCallsAreBothAnswered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := relay.Relay{URL: testrelay.Start(t)}
	serving := keys.Generate()
	ready := make(chan struct{})
	go provider(t, serving).ServeLive(ctx, r, func() { close(ready) })
	<-ready

	caller := keys.Generate()
	for i := range 2 {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		reply, _, err := call.Live(ctx, caller, serving.Public, call.Request{
			Capability: "primary", Method: "ECHO", Path: "/", Body: "same",
		}, r)
		cancel()
		if err != nil || reply.Body != "ECHO / same" {
			t.Fatalf("call %d: %+v, %v", i+1, reply, err)
		}
	}
}

func TestTheProviderSeesTheCallerAsFrom(t *testing.T) {
	ctx := context.Background()
	serving := keys.Generate()
	server := provider(t, serving)
	caller := keys.Generate()

	rumor := call.RequestRumor(caller, serving.Public, call.Request{Capability: "primary", Method: "ECHO", Path: "/", Body: "x"}, time.Now())
	reply, err := server.Handle(ctx, rumor)
	if err != nil || reply.Body != "ECHO / x" {
		t.Fatalf("reply = %+v, %v", reply, err)
	}

	// The same request again is refused: a relay cannot replay it.
	if _, err := server.Handle(ctx, rumor); err != call.ErrDuplicate {
		t.Errorf("a second handle gave %v, want ErrDuplicate", err)
	}
}

func TestAStaleLiveRequestIsRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := relay.Relay{URL: testrelay.Start(t)}
	serving := keys.Generate()
	ready := make(chan struct{})
	go provider(t, serving).ServeLive(ctx, r, func() { close(ready) })
	<-ready

	// A request written ten minutes ago, as a relay replaying it would send.
	caller := keys.Generate()
	old := call.RequestRumor(caller, serving.Public, call.Request{Capability: "primary", Method: "ECHO", Path: "/", Body: "old"}, time.Now().Add(-10*time.Minute))
	wrap, err := private.Wrap(caller, serving.Public, old, private.RelayForm, private.LiveWrapKind, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	short, stop := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer stop()
	_, err = r.Exchange(short, wrap, nostr.Filter{Kinds: []nostr.Kind{private.LiveWrapKind}, Tags: nostr.TagMap{"p": {caller.Public.Hex()}}},
		func(nostr.Event) bool { return true })
	if err == nil {
		t.Error("the provider answered a request outside the live window")
	}
}

func TestACallToAnotherCapabilityIsRefused(t *testing.T) {
	serving := keys.Generate()
	rumor := call.RequestRumor(keys.Generate(), serving.Public, call.Request{Capability: "other", Body: "x"}, time.Now())
	reply, err := provider(t, serving).Handle(context.Background(), rumor)
	if err != nil || !strings.HasPrefix(reply.Err, "unknown_capability") {
		t.Errorf("reply = %+v, %v", reply, err)
	}
}

// citizen is a node with mail, for store-and-forward calls.
type citizen struct {
	key  keys.Key
	mail *mail.Mail
}

func newCitizen(t *testing.T, relays ...transport.Transport) citizen {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	k := keys.Generate()
	m, err := mail.Open(dir, k, &node.Node{Store: s}, relays)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return citizen{key: k, mail: m}
}

func (c citizen) sync(t *testing.T, tr transport.Transport) mail.Report {
	t.Helper()
	report, err := c.mail.Sync(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// The proof of phase 3: a store-and-forward call crosses the courier path.
func TestAStoreAndForwardCallCrossesACourier(t *testing.T) {
	alice, carol, service := newCitizen(t), newCitizen(t), newCitizen(t)
	service.mail.OnRequest = provider(t, service.key).Handle
	first, second := file.Dir{Path: t.TempDir()}, file.Dir{Path: t.TempDir()}

	if _, err := alice.mail.Request(context.Background(), service.key.Public, call.Request{
		Capability: "primary", Method: "ECHO", Path: "/", Body: "by hand",
	}); err != nil {
		t.Fatal(err)
	}
	alice.sync(t, first)
	carol.sync(t, first)
	carol.sync(t, second)

	if got := service.sync(t, second); got.Answered != 1 {
		t.Fatalf("the provider answered %d calls", got.Answered)
	}
	service.sync(t, second)
	carol.sync(t, second)
	carol.sync(t, first)

	if got := alice.sync(t, first); got.Replies != 1 {
		t.Fatalf("alice got %d replies", got.Replies)
	}
	out := alice.mail.Outbox()
	if len(out) != 1 || out[0].State(time.Now()) != "delivered" || out[0].Reply.Body != "ECHO / by hand" {
		t.Errorf("alice's outbox: %+v", out)
	}
}
