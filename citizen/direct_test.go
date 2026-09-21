package citizen_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/citizen"
	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/direct"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/relay"
)

// policyFile writes the rules of one owner: it may carry this one
// conversation with that one peer, over the loopback.
func policyFile(t *testing.T, peer []byte, listens bool) string {
	t.Helper()

	rule := map[string]any{
		"peer":       hex.EncodeToString(peer),
		"capability": "primary",
		"scheme":     "echo",
		"path":       "/",
		"lease_ms":   30000,
		"dial":       []any{"127.0.0.1"},
	}
	if listens {
		rule["listen"] = map[string]any{"bind": "127.0.0.1", "address": "127.0.0.1", "port": 0}
	}

	body, err := json.Marshal(map[string]any{"version": 1, "rules": []any{rule}})
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "direct.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A conversation leaves the relay, and the calls that follow travel the
// carrier.
func TestACallLeavesTheRelay(t *testing.T) {
	relayIdentity, _ := identity.Generate()
	server, err := relay.Listen(context.Background(), relay.Options{
		Identity: relayIdentity, Address: "127.0.0.1:0", Log: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })

	binary := build(t)
	manifest, err := filepath.Abs(filepath.Join("testdata", "echo", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	me, _ := identity.Generate()
	caller, _ := identity.Generate()

	serving, err := citizen.Serve(context.Background(), citizen.Options{
		Identity:       me,
		Relay:          server.Addr().String(),
		RelayPublicKey: server.PublicKey(),
		Serve:          "exec://" + binary + "?manifest=" + manifest,
		DirectPolicy:   policyFile(t, caller.PublicKey, true),
		Log:            quiet,
	})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { serving.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	connection, err := client.Dial(ctx, server.Addr().String(), client.Options{
		Identity: caller, RelayPublicKey: server.PublicKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })

	peers := connection.Peers()

	rules, err := direct.LoadPolicy(policyFile(t, serving.PublicKey(), false))
	if err != nil {
		t.Fatal(err)
	}
	peers.Direct(rules, quiet)

	address := "echo+arc://" + hex.EncodeToString(serving.PublicKey()) + "/"

	// The first call travels the relay.
	answer, err := peers.Call(ctx, address, []byte("over the relay"), "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.HasSuffix(string(answer.Body), "over the relay") {
		t.Errorf("body = %q", answer.Body)
	}

	// The conversation leaves the relay.
	if err := peers.Promote(ctx, address, ""); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// The calls that follow travel the carrier, and answer the same way.
	answer, err = peers.Call(ctx, address, []byte("off the relay"), "")
	if err != nil {
		t.Fatalf("the call after the promotion failed: %v", err)
	}
	if !strings.HasSuffix(string(answer.Body), "off the relay") {
		t.Errorf("body = %q", answer.Body)
	}

	// The relay carries nothing of this conversation now.
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}

	answer, err = peers.Call(ctx, address, []byte("the relay is gone"), "")
	if err != nil {
		t.Fatalf("the call without the relay failed: %v", err)
	}
	if !strings.HasSuffix(string(answer.Body), "the relay is gone") {
		t.Errorf("body = %q", answer.Body)
	}
}

// Without a rule of the owner, a conversation stays on the relay.
func TestWithoutARuleTheConversationStays(t *testing.T) {
	serving, peers := stack(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	address := "echo+arc://" + hexOf(serving.PublicKey()) + "/"
	if err := peers.Promote(ctx, address, ""); err == nil {
		t.Error("a conversation left the relay without a rule")
	}
}

// A control message of a direct route travels the relay, even while a
// carrier stands for the peer. The peer reads a carrier only after its route
// stands, so a control message on the carrier never arrives.
func TestControlTravelsTheRelayWhileACarrierStands(t *testing.T) {
	relayIdentity, _ := identity.Generate()
	server, err := relay.Listen(context.Background(), relay.Options{
		Identity: relayIdentity, Address: "127.0.0.1:0", Log: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })

	binary := build(t)
	manifest, err := filepath.Abs(filepath.Join("testdata", "echo", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	me, _ := identity.Generate()
	caller, _ := identity.Generate()

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connection, err := client.Dial(ctx, server.Addr().String(), client.Options{
		Identity: caller, RelayPublicKey: server.PublicKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })

	// A carrier that the caller never reads stands for the caller.
	serving.HoldCarrier(caller.PublicKey, standIn(t))

	if err := serving.SendControl(caller.PublicKey, []byte(`{"id":"route","type":"ready"}`)); err != nil {
		t.Fatal(err)
	}

	select {
	case <-connection.Packets():
	case <-time.After(5 * time.Second):
		t.Fatal("the control message did not travel the relay")
	}
}

// standIn opens a carrier between two strangers, and returns one end of it.
// Nothing reads the other end.
func standIn(t *testing.T) *direct.Conn {
	t.Helper()

	alice, _ := identity.Generate()
	bob, _ := identity.Generate()
	first, err := direct.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	second, err := direct.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	binding := make([]byte, 32)

	listener, err := direct.Listen("127.0.0.1:0", direct.Options{
		Identity: alice, Credentials: first,
		PeerKey: bob.PublicKey, PeerFingerprint: second.Fingerprint, Binding: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan *direct.Conn, 1)
	go func() {
		conn, _ := listener.Accept(context.Background())
		accepted <- conn
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	near, err := direct.Dial(ctx, listener.Addr().String(), direct.Options{
		Identity: bob, Credentials: second,
		PeerKey: alice.PublicKey, PeerFingerprint: first.Fingerprint, Binding: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	far := <-accepted
	if far == nil {
		t.Fatal("the stand-in carrier did not stand")
	}

	t.Cleanup(func() { near.Close(); far.Close() })
	return near
}
