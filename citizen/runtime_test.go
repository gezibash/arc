package citizen_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/citizen"
	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/relay"
)

// serveProgram starts a relay, a citizen that serves one program of
// testdata, and a caller.
func serveProgram(t *testing.T, program string) (*relay.Relay, *citizen.Citizen, *client.Peers) {
	t.Helper()

	relayIdentity, _ := identity.Generate()
	server, err := relay.Listen(context.Background(), relay.Options{
		Identity: relayIdentity, Address: "127.0.0.1:0", Log: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })

	binary := filepath.Join(t.TempDir(), program)
	if output, err := exec.Command("go", "build", "-o", binary, "./testdata/"+program).CombinedOutput(); err != nil {
		t.Fatalf("the program did not build: %v: %s", err, output)
	}
	manifest, err := filepath.Abs(filepath.Join("testdata", program, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	me, _ := identity.Generate()
	serving, err := citizen.Serve(context.Background(), citizen.Options{
		Identity:       me,
		Relay:          server.Addr().String(),
		RelayPublicKey: server.PublicKey(),
		Serve:          "exec://" + binary + "?manifest=" + manifest,
		Log:            quiet,
	})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { serving.Close() })

	caller, _ := identity.Generate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connection, err := client.Dial(ctx, server.Addr().String(), client.Options{
		Identity: caller, RelayPublicKey: server.PublicKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })

	return server, serving, connection.Peers()
}

// A capability that carries bytes gets them through the runtime connection
// as base64. No byte changes on the way.
func TestBytesCrossTheRuntimeAsBase64(t *testing.T) {
	_, serving, peers := serveProgram(t, "bytes")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	body := []byte{0x00, 0xff, 0xfe, 0x80, 'a', '\n'}
	answer, err := peers.Call(ctx, "bytes+arc://"+hexOf(serving.PublicKey())+"/", body, "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	want := []byte{'\n', 'a', 0x80, 0xfe, 0xff, 0x00}
	if string(answer.Body) != string(want) {
		t.Fatalf("body = %x, want %x", answer.Body, want)
	}
}

// A reply that says base64 and is not fails the call. Nothing of it reaches
// the caller as a body.
func TestAReplyThatIsNotBase64Fails(t *testing.T) {
	_, serving, peers := serveProgram(t, "bytes")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := peers.Call(ctx, "bytes+arc://"+hexOf(serving.PublicKey())+"/", []byte{0x01}, "")

	var remote *client.RemoteError
	if !asRemote(err, &remote) || remote.Code != "invalid_reply" {
		t.Fatalf("err = %v", err)
	}
}

// A text capability reads UTF-8 only. The client refuses other bytes before
// they leave, and the citizen refuses them from a caller that skips that
// check.
func TestATextCapabilityTakesUTF8Only(t *testing.T) {
	_, serving, peers := serveProgram(t, "echo")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	address := "echo+arc://" + hexOf(serving.PublicKey()) + "/"
	if _, err := peers.Call(ctx, address, []byte{0xff, 0xfe}, ""); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("the client sent bytes to a text capability: %v", err)
	}

	_, err := peers.Request(ctx, serving.PublicKey(),
		map[string]any{"method": "ECHO", "path": "/", "capability_id": "primary"}, []byte{0xff, 0xfe})

	var remote *client.RemoteError
	if !asRemote(err, &remote) || remote.Code != "invalid_body" {
		t.Fatalf("the citizen passed bytes to a text provider: %v", err)
	}
}

// A request id that already waits is refused, and so is a request over the
// limit of the provider.
func TestTheCitizenBoundsTheRequestsThatWait(t *testing.T) {
	_, serving, _ := serveProgram(t, "echo")
	peer, _ := identity.Generate()

	if code, _ := serving.Admit(peer.PublicKey, []byte("request-0")); code != "" {
		t.Fatalf("the first request was refused: %s", code)
	}
	if code, _ := serving.Admit(peer.PublicKey, []byte("request-0")); code != "duplicate_request" {
		t.Fatalf("the same id twice gave %q", code)
	}

	for n := 1; n < citizen.MaxPending; n++ {
		if code, _ := serving.Admit(peer.PublicKey, []byte("request-"+string(rune('a'+n%26))+strings.Repeat("x", n))); code != "" {
			t.Fatalf("request %d was refused: %s", n, code)
		}
	}
	if code, _ := serving.Admit(peer.PublicKey, []byte("one-too-many")); code != "provider_busy" {
		t.Fatalf("request %d gave %q", citizen.MaxPending+1, code)
	}
}

// When the relay goes away, the citizen stops and says so.
func TestTheCitizenSaysThatTheRelayEnded(t *testing.T) {
	server, serving, _ := serveProgram(t, "echo")

	server.Close()

	select {
	case <-serving.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the citizen did not stop after the relay ended")
	}
	if err := serving.Err(); err == nil || !strings.Contains(err.Error(), "the relay connection ended") {
		t.Fatalf("err = %v", err)
	}
	if err := serving.Close(); err != nil {
		t.Fatalf("the provider did not end cleanly: %v", err)
	}
}

// arc call speaks request and reply only. A capability of another mode is
// refused before any request leaves.
func TestACallRefusesACapabilityOfAnotherMode(t *testing.T) {
	relayIdentity, _ := identity.Generate()
	server, err := relay.Listen(context.Background(), relay.Options{
		Identity: relayIdentity, Address: "127.0.0.1:0", Log: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })

	body, err := os.ReadFile(filepath.Join("testdata", "echo", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	events := strings.Replace(string(body), `"mode": "request_reply"`, `"mode": "events"`, 1)
	if err := os.WriteFile(manifest, []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}

	me, _ := identity.Generate()
	serving, err := citizen.Serve(context.Background(), citizen.Options{
		Identity:       me,
		Relay:          server.Addr().String(),
		RelayPublicKey: server.PublicKey(),
		Serve:          "exec://" + build(t) + "?manifest=" + manifest,
		Log:            quiet,
	})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { serving.Close() })

	caller, _ := identity.Generate()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := client.Dial(ctx, server.Addr().String(), client.Options{
		Identity: caller, RelayPublicKey: server.PublicKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })

	_, err = connection.Peers().Call(ctx, "echo+arc://"+hexOf(serving.PublicKey())+"/", []byte("hello"), "")
	if err == nil || !strings.Contains(err.Error(), "events mode") {
		t.Fatalf("err = %v", err)
	}
}
