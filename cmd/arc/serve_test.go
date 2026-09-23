package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/call"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/relaylist"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport/relay"
)

// output keeps what a command writes, for a test that reads it while the
// command runs.
type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// serving runs `arc serve` with the echo provider until ctx ends. It returns
// the output of the command, and a channel that gives the result of the
// command.
func serving(t *testing.T, ctx context.Context, home string) (*output, <-chan error) {
	t.Helper()
	dir := t.TempDir()
	echo := filepath.Join(dir, "echo")
	build := exec.Command("go", "build", "-o", echo, "../../provider/host/testdata/echo")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join("..", "exec-provider", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}

	out := &output{}
	command := root()
	command.SetOut(out)
	command.SetArgs([]string{"--home", home, "serve", "exec://" + echo + "?manifest=" + filepath.Join(dir, "manifest.json")})
	result := make(chan error, 1)
	go func() { result <- command.ExecuteContext(ctx) }()
	return out, result
}

// waitFor waits until the output holds text, or until the time ends.
func waitFor(out *output, text string, limit time.Duration) bool {
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(out.String(), text) {
			return true
		}
	}
	return false
}

// deadAddress returns the address of a port where no relay listens.
func deadAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	return address
}

// The only relay of a provider is down when `arc serve` starts. `arc serve`
// does not say "serves" while no relay has the watch, because a caller that
// reads the line calls at once. When the relay comes up, `arc serve` says
// "serves", and a live call through the relay gets an answer.
func TestServeSaysServesOnlyWhenARelayHasTheWatch(t *testing.T) {
	address := deadAddress(t)
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	ok(t, home, "", "relay", "add", "ws://"+address)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, result := serving(t, ctx, home)

	if waitFor(out, "serves", 2*time.Second) {
		t.Fatalf("arc serve said %q, but no relay has the watch", out.String())
	}

	url := testrelay.StartAt(t, address)
	if !waitFor(out, "serves", 15*time.Second) {
		t.Fatalf("arc serve did not say serves after the relay came up: %q", out.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	public, err := hex.DecodeString(lines[len(lines)-1])
	if err != nil || len(public) != 32 {
		t.Fatalf("arc serve gave no public key: %q", out.String())
	}
	id := strings.Fields(lines[0])[2]
	callCtx, cancelCall := context.WithTimeout(ctx, 5*time.Second)
	defer cancelCall()
	reply, _, err := callEcho(callCtx, nostr.PubKey(public), id, relay.Relay{URL: url})
	if err != nil {
		t.Fatalf("a live call just after serves: %v", err)
	}
	if reply.Body != "ECHO / hello" {
		t.Errorf("reply = %+v", reply)
	}

	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

// One relay of two is down, and stays down. `arc serve` says "serves" through
// the other relay, and does not wait for the one that is down.
func TestServeSaysServesWhenOneRelayOfTwoIsDown(t *testing.T) {
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	ok(t, home, "", "relay", "add", "ws://"+deadAddress(t))
	ok(t, home, "", "relay", "add", testrelay.Start(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, result := serving(t, ctx, home)

	if !waitFor(out, "serves", 10*time.Second) {
		t.Fatalf("arc serve did not say serves through the relay that is up: %q", out.String())
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

// `arc serve` is told to stop before any relay has the watch. It stops, and
// it never says "serves".
func TestServeStopsBeforeAnyRelayHasTheWatch(t *testing.T) {
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	ok(t, home, "", "relay", "add", "ws://"+deadAddress(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, result := serving(t, ctx, home)

	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("arc serve did not stop when its context ended")
	}
	if strings.Contains(out.String(), "serves") {
		t.Fatalf("arc serve said %q, but no relay had the watch", out.String())
	}
}

func callEcho(ctx context.Context, provider nostr.PubKey, id string, r relay.Relay) (call.Reply, time.Duration, error) {
	return call.Live(ctx, keys.Generate(), provider, call.Request{
		Capability: id, Method: "ECHO", Path: "/", Body: "hello",
	}, r)
}

// A relay and an indexer of a provider are down when `arc serve` starts, and
// come back after it says "serves". Each gets the NIP-65 relay list, because
// a caller that shares no relay with the provider finds its read relays
// there.
func TestServeSendsTheRelayListToARelayThatComesBack(t *testing.T) {
	down, indexer := deadAddress(t), deadAddress(t)
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	ok(t, home, "", "relay", "add", testrelay.Start(t))
	ok(t, home, "", "relay", "add", "ws://"+down)
	ok(t, home, "", "relay", "add", "--index", "ws://"+indexer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, result := serving(t, ctx, home)
	if !waitFor(out, "serves", 10*time.Second) {
		t.Fatalf("arc serve did not say serves through the relay that is up: %q", out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	public, err := hex.DecodeString(lines[len(lines)-1])
	if err != nil || len(public) != 32 {
		t.Fatalf("arc serve gave no public key: %q", out.String())
	}

	for _, address := range []string{down, indexer} {
		url := testrelay.StartAt(t, address)
		filter := nostr.Filter{Kinds: []nostr.Kind{relaylist.Kind}, Authors: []nostr.PubKey{nostr.PubKey(public)}}
		found := false
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !found; time.Sleep(200 * time.Millisecond) {
			batch, err := relay.Relay{URL: url}.Fetch(ctx, filter)
			found = err == nil && len(batch.Events) > 0
		}
		if !found {
			t.Errorf("%s came back, but it did not get the relay list of the provider", url)
		}
	}

	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
