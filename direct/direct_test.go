package direct_test

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/direct"
	"github.com/gezibash/arc/identity"
)

// pair sets up what a relay conversation agrees: two identities, two
// certificates, and one binding.
func pair(t *testing.T) (direct.Options, direct.Options) {
	t.Helper()

	alice, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	first, err := direct.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	second, err := direct.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}

	binding := make([]byte, 32)
	if _, err := rand.Read(binding); err != nil {
		t.Fatal(err)
	}

	return direct.Options{
			Identity: alice, Credentials: first,
			PeerKey: bob.PublicKey, PeerFingerprint: second.Fingerprint,
			Binding: binding, Timeout: 5 * time.Second,
		},
		direct.Options{
			Identity: bob, Credentials: second,
			PeerKey: alice.PublicKey, PeerFingerprint: first.Fingerprint,
			Binding: binding, Timeout: 5 * time.Second,
		}
}

// carry opens one carrier between two sides.
func carry(t *testing.T, server, client direct.Options) (*direct.Conn, *direct.Conn) {
	t.Helper()

	listener, err := direct.Listen("127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	type result struct {
		conn *direct.Conn
		err  error
	}
	accepted := make(chan result, 1)

	go func() {
		conn, err := listener.Accept(context.Background())
		accepted <- result{conn, err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dialed, err := direct.Dial(ctx, listener.Addr().String(), client)
	got := <-accepted

	if err != nil || got.err != nil {
		if dialed != nil {
			dialed.Close()
		}
		if got.conn != nil {
			got.conn.Close()
		}
		t.Fatalf("dial: %v, accept: %v", err, got.err)
	}

	t.Cleanup(func() { dialed.Close(); got.conn.Close() })
	return got.conn, dialed
}

func TestACarrierBetweenTwoCitizens(t *testing.T) {
	server, client := pair(t)
	waiting, dialed := carry(t, server, client)

	if string(dialed.PeerKey()) != string(server.Identity.PublicKey) {
		t.Error("the carrier names another peer")
	}

	if err := dialed.Send([]byte("the packet of a citizen")); err != nil {
		t.Fatal(err)
	}

	select {
	case arrived := <-waiting.Packets():
		if string(arrived) != "the packet of a citizen" {
			t.Errorf("packet = %q", arrived)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the packet did not arrive")
	}

	// The carrier runs both ways.
	if err := waiting.Send([]byte("and back")); err != nil {
		t.Fatal(err)
	}

	select {
	case arrived := <-dialed.Packets():
		if string(arrived) != "and back" {
			t.Errorf("packet = %q", arrived)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer did not arrive")
	}
}

func TestACarrierTakesALargePacket(t *testing.T) {
	server, client := pair(t)
	waiting, dialed := carry(t, server, client)

	body := []byte(strings.Repeat("a", direct.MaxPacketBytes))
	if err := dialed.Send(body); err != nil {
		t.Fatal(err)
	}

	select {
	case arrived := <-waiting.Packets():
		if len(arrived) != len(body) {
			t.Errorf("the packet is %d bytes, want %d", len(arrived), len(body))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the large packet did not arrive")
	}

	if err := dialed.Send(append(body, 'x')); err != direct.ErrPacketTooLarge {
		t.Errorf("a packet over the limit gave %v", err)
	}
}

// The certificate of the peer is named by the relay conversation. Another
// certificate ends the connection.
func TestACarrierRefusesAnotherCertificate(t *testing.T) {
	server, client := pair(t)

	other, err := direct.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	client.PeerFingerprint = other.Fingerprint

	listener, err := direct.Listen("127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept(context.Background())
		if err == nil {
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if conn, err := direct.Dial(ctx, listener.Addr().String(), client); err == nil {
		conn.Close()
		t.Error("a carrier stood with another certificate")
	}
}

// Each side proves the identity that the relay named.
func TestACarrierRefusesAnotherIdentity(t *testing.T) {
	server, client := pair(t)

	other, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	client.PeerKey = other.PublicKey

	listener, err := direct.Listen("127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept(context.Background())
		if err == nil {
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if conn, err := direct.Dial(ctx, listener.Addr().String(), client); err == nil {
		conn.Close()
		t.Error("a carrier stood with another identity")
	}
}

// The proof covers the conversation of the relay. Two sides that name
// different conversations do not meet.
func TestACarrierRefusesAnotherBinding(t *testing.T) {
	server, client := pair(t)

	binding := make([]byte, 32)
	if _, err := rand.Read(binding); err != nil {
		t.Fatal(err)
	}
	client.Binding = binding

	listener, err := direct.Listen("127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept(context.Background())
		if err == nil {
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if conn, err := direct.Dial(ctx, listener.Addr().String(), client); err == nil {
		conn.Close()
		t.Error("a carrier stood across two conversations")
	}
}

func TestOptionsMustHoldTogether(t *testing.T) {
	server, _ := pair(t)

	cases := map[string]func(*direct.Options){
		"no identity":      func(o *direct.Options) { o.Identity = nil },
		"no credentials":   func(o *direct.Options) { o.Credentials = nil },
		"a short peer key": func(o *direct.Options) { o.PeerKey = []byte{1} },
		"a short hash":     func(o *direct.Options) { o.PeerFingerprint = []byte{1} },
		"a short binding":  func(o *direct.Options) { o.Binding = []byte{1} },
	}

	for name, change := range cases {
		options := server
		change(&options)

		if _, err := direct.Listen("127.0.0.1:0", options); err == nil {
			t.Errorf("%s: the listener started", name)
		}
	}
}

func TestACarrierEndsWhenTheOtherSideGoes(t *testing.T) {
	server, client := pair(t)
	waiting, dialed := carry(t, server, client)

	dialed.Close()

	select {
	case <-waiting.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the carrier did not end")
	}

	if err := waiting.Send([]byte("x")); err == nil {
		t.Error("a closed carrier took a packet")
	}
}
