package client_test

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gezibash/arc/citizen"
	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/relay"
	"github.com/gezibash/arc/wake"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// sleeper starts its citizen on the first wake, the way a wake hook starts a
// machine that paused.
type sleeper struct {
	start func() error

	mu      sync.Mutex
	awake   bool
	fail    error
	wakes   int
	answers int
}

func (s *sleeper) Wake(_ context.Context, _ []byte, _ func(context.Context, []byte) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.wakes++
	if s.fail != nil {
		return s.fail
	}
	if !s.awake {
		if err := s.start(); err != nil {
			return err
		}
		s.awake = true
	}
	return nil
}

func (s *sleeper) Answered(_ []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers++
}

// world starts a relay and a caller with the waker. Serve makes a citizen
// serve the echo provider on the relay.
func world(t *testing.T, waker client.Waker) (*client.Peers, func(*identity.Identity) error) {
	t.Helper()

	relayIdentity, _ := identity.Generate()
	server, err := relay.Listen(context.Background(), relay.Options{
		Identity: relayIdentity,
		Address:  "127.0.0.1:0",
		Log:      quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })

	binary := filepath.Join(t.TempDir(), "echo-provider")
	if output, err := exec.Command("go", "build", "-o", binary, "../citizen/testdata/echo").CombinedOutput(); err != nil {
		t.Fatalf("the provider did not build: %v: %s", err, output)
	}
	manifest, err := filepath.Abs(filepath.Join("..", "citizen", "testdata", "echo", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	serve := func(me *identity.Identity) error {
		serving, err := citizen.Serve(context.Background(), citizen.Options{
			Identity:       me,
			Relay:          server.Addr().String(),
			RelayPublicKey: server.PublicKey(),
			Serve:          "exec://" + binary + "?manifest=" + manifest,
			Log:            quiet,
		})
		if err != nil {
			return err
		}
		t.Cleanup(func() { serving.Close() })
		return nil
	}

	caller, _ := identity.Generate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connection, err := client.Dial(ctx, server.Addr().String(), client.Options{
		Identity:       caller,
		RelayPublicKey: server.PublicKey(),
		Waker:          waker,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })

	return connection.Peers(), serve
}

// asleep gives a caller whose citizen serves only after the wake.
func asleep(t *testing.T) (*sleeper, *client.Peers, []byte) {
	t.Helper()

	me, _ := identity.Generate()
	waker := &sleeper{}
	peers, serve := world(t, waker)
	waker.start = func() error { return serve(me) }
	return waker, peers, me.PublicKey
}

// The citizen serves nothing until the wake, so the call can only succeed
// because the wake came before the request.
func TestACallWakesTheCitizenFirst(t *testing.T) {
	waker, peers, key := asleep(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	answer, err := peers.Call(ctx, "echo+arc://"+hex.EncodeToString(key)+"/hello", []byte("body"), "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(answer.Body) != "ECHO /hello body" {
		t.Errorf("body = %q", answer.Body)
	}

	waker.mu.Lock()
	defer waker.mu.Unlock()
	if !waker.awake {
		t.Error("the waker did not wake the citizen")
	}
	// A call asks for the signed package, and then sends the request.
	if waker.wakes != 2 || waker.answers != 2 {
		t.Errorf("wakes = %d, answers = %d; each of the two requests wakes, and each answer counts", waker.wakes, waker.answers)
	}
}

// A wake that fails stops the request at once. Nothing goes to the relay.
func TestAFailedWakeStopsTheRequest(t *testing.T) {
	waker, peers, key := asleep(t)
	waker.fail = errors.New("wake_failed: the hook exited with status 1")

	started := time.Now()
	_, err := peers.Request(context.Background(), key, map[string]any{"method": "GET", "path": "/"}, nil)
	if err == nil || err.Error() != waker.fail.Error() {
		t.Fatalf("err = %v, and it must be the error of the wake", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("the request waited %s after the wake failed", elapsed)
	}

	waker.mu.Lock()
	defer waker.mu.Unlock()
	if waker.answers != 0 {
		t.Errorf("answers = %d, and nothing answered", waker.answers)
	}
}

func TestOnlineAsksTheRelay(t *testing.T) {
	me, _ := identity.Generate()
	peers, serve := world(t, nil)
	ctx := context.Background()

	online, err := peers.Online(ctx, me.PublicKey)
	if err != nil || online {
		t.Fatalf("before it serves: online = %v, err = %v", online, err)
	}

	if err := serve(me); err != nil {
		t.Fatal(err)
	}
	online, err = peers.Online(ctx, me.PublicKey)
	if err != nil || !online {
		t.Errorf("while it serves: online = %v, err = %v", online, err)
	}
}

// With the waker of the wake package, a request to a citizen that is not
// there ends at once with peer_offline. Without it, the request waits for an
// answer that never comes.
func TestARequestToAnAbsentCitizenIsPeerOffline(t *testing.T) {
	dir := t.TempDir()
	waker := wake.Load(filepath.Join(dir, wake.FileName), filepath.Join(dir, wake.StateDirName))
	peers, serve := world(t, waker)

	absent, _ := identity.Generate()
	started := time.Now()
	_, err := peers.Request(context.Background(), absent.PublicKey, map[string]any{"method": "GET", "path": "/"}, nil)
	if !errors.Is(err, wake.ErrPeerOffline) {
		t.Fatalf("err = %v, and it must be peer_offline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("peer_offline took %s", elapsed)
	}

	// The same caller reaches a citizen that serves.
	present, _ := identity.Generate()
	if err := serve(present); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	answer, err := peers.Call(ctx, "echo+arc://"+hex.EncodeToString(present.PublicKey)+"/hi", []byte("x"), "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(answer.Body) != "ECHO /hi x" {
		t.Errorf("body = %q", answer.Body)
	}
}
