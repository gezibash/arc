package relay_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/gezibash/arc/announce"
	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/frame"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/packet"
	"github.com/gezibash/arc/relay"
	"github.com/gezibash/arc/session"
)

func start(t *testing.T) *relay.Relay {
	t.Helper()

	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	server, err := relay.Listen(context.Background(), relay.Options{
		Identity: me,
		Address:  "127.0.0.1:0",
		Version:  "test",
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

func join(t *testing.T, server *relay.Relay, me *identity.Identity) *client.Client {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connection, err := client.Dial(ctx, server.Addr().String(), client.Options{
		Identity:       me,
		RelayPublicKey: server.PublicKey(),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

func citizen(t *testing.T, server *relay.Relay) (*identity.Identity, *client.Client) {
	t.Helper()

	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return me, join(t, server, me)
}

func TestAnswersStatusAndObserve(t *testing.T) {
	server := start(t)
	_, connection := citizen(t, server)
	ctx := context.Background()

	status, err := connection.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status["role"] != "relay" || status["state"] != "running" || status["version"] != "test" {
		t.Errorf("status = %v", status)
	}

	observed, err := connection.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Host != "127.0.0.1" || observed.Port == 0 {
		t.Errorf("observed = %+v", observed)
	}
}

// Close ends every connection, one that joined a moment ago too. A
// connection that registered after Close looked at the routes kept Close
// waiting until its client left.
func TestCloseEndsAConnectionThatJustJoined(t *testing.T) {
	for range 30 {
		server := start(t)
		me, _ := identity.Generate()
		join(t, server, me)

		closed := make(chan struct{})
		go func() {
			server.Close()
			close(closed)
		}()

		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("Close waited for a client that had just joined")
		}
	}
}

func TestRefusesAClientThatCannotProveItsKey(t *testing.T) {
	server := start(t)
	me, _ := identity.Generate()
	other, _ := identity.Generate()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.Dial(ctx, server.Addr().String(), client.Options{
		Identity:       me,
		RelayPublicKey: other.PublicKey,
	}); err == nil {
		t.Error("the client accepted a relay that is not the pinned relay")
	}
}

func TestHoldsAndFindsAnAnnouncement(t *testing.T) {
	server := start(t)
	me, connection := citizen(t, server)
	ctx := context.Background()

	record, err := announce.Create(me, []announce.Capability{{
		ID:      "exec",
		Title:   "Run a command",
		Summary: "Runs one command on this machine.",
	}}, announce.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Announce(ctx, record); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{me.ShortName(), me.Name(), me.EncodePublicKey()[:8]} {
		entries, err := connection.Resolve(ctx, query)
		if err != nil {
			t.Fatalf("resolve %s: %v", query, err)
		}
		if len(entries) != 1 || entries[0]["public_key"] != me.EncodePublicKey() {
			t.Errorf("resolve %s found %v", query, entries)
		}
	}

	page, err := connection.Search(ctx, "run a command", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Total != 1 || page.Next != "" {
		t.Errorf("search = %+v", page)
	}

	empty, err := connection.Search(ctx, "postgres", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Entries) != 0 {
		t.Errorf("the search found %v", empty.Entries)
	}
}

func TestSearchPages(t *testing.T) {
	server := start(t)
	ctx := context.Background()

	for index := 0; index < 5; index++ {
		me, connection := citizen(t, server)
		record, err := announce.Create(me, []announce.Capability{{ID: "exec", Title: "Run a command"}}, announce.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Announce(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	_, reader := citizen(t, server)

	seen := map[string]bool{}
	after := ""
	for pages := 0; pages < 10; pages++ {
		page, err := reader.Search(ctx, "run a command", 2, after)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 5 {
			t.Errorf("total = %d, want 5", page.Total)
		}
		for _, entry := range page.Entries {
			key := entry["public_key"].(string)
			if seen[key] {
				t.Errorf("the page repeated %s", key)
			}
			seen[key] = true
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}

	if len(seen) != 5 {
		t.Errorf("the pages held %d citizens, want 5", len(seen))
	}
}

func TestRefusesAnAnnouncementOfAnotherCitizen(t *testing.T) {
	server := start(t)
	_, connection := citizen(t, server)
	other, _ := identity.Generate()

	record, err := announce.Create(other, []announce.Capability{{ID: "exec"}}, announce.Options{})
	if err != nil {
		t.Fatal(err)
	}

	if err := connection.Announce(context.Background(), record); err == nil {
		t.Error("the relay took an announcement of another citizen")
	}
}

func TestTheDirectoryForgetsAClientThatLeaves(t *testing.T) {
	server := start(t)
	me, connection := citizen(t, server)
	ctx := context.Background()

	record, err := announce.Create(me, []announce.Capability{{ID: "exec"}}, announce.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Announce(ctx, record); err != nil {
		t.Fatal(err)
	}

	connection.Close()

	_, reader := citizen(t, server)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := reader.Resolve(ctx, me.ShortName())
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the directory still holds a citizen that left")
}

func TestCarriesAPacket(t *testing.T) {
	server := start(t)
	alice, from := citizen(t, server)
	bob, to := citizen(t, server)

	talk, err := session.Establish(alice, bob.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	body, err := frame.Encode(frame.Request, frame.NewRequestID(), map[string]any{"path": "/hello"}, []byte("hello bob"))
	if err != nil {
		t.Fatal(err)
	}

	nonce, ciphertext, seq, err := talk.Encrypt(body)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := packet.Encode(alice, bob.PublicKey, talk.ID, seq, nonce, ciphertext, packet.WithEphemeralKey(talk.EphemeralPublic))
	if err != nil {
		t.Fatal(err)
	}
	if err := from.SendPacket(raw); err != nil {
		t.Fatal(err)
	}

	select {
	case arrived := <-to.Packets():
		got, err := packet.Decode(arrived)
		if err != nil {
			t.Fatal(err)
		}
		joined, err := session.Accept(bob, got.Src, got.EphemeralPublic, got.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		plaintext, err := joined.Decrypt(got.Nonce, got.Ciphertext)
		if err != nil {
			t.Fatal(err)
		}
		answer, err := frame.Decode(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if string(answer.Body) != "hello bob" {
			t.Errorf("body = %q", answer.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the packet did not arrive")
	}
}

// A citizen may write only under its own key, and only with a fresh packet.
func TestDropsAPacketThatItCannotPlace(t *testing.T) {
	server := start(t)
	alice, from := citizen(t, server)
	bob, to := citizen(t, server)
	mallory, _ := identity.Generate()

	send := func(signer *identity.Identity, options ...packet.Option) {
		t.Helper()

		talk, err := session.Establish(signer, bob.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		nonce, ciphertext, seq, err := talk.Encrypt([]byte("x"))
		if err != nil {
			t.Fatal(err)
		}

		options = append(options, packet.WithEphemeralKey(talk.EphemeralPublic))
		raw, err := packet.Encode(signer, bob.PublicKey, talk.ID, seq, nonce, ciphertext, options...)
		if err != nil {
			t.Fatal(err)
		}
		if err := from.SendPacket(raw); err != nil {
			t.Fatal(err)
		}
	}

	// Another citizen signed this packet, so the connection of Alice may not
	// carry it.
	send(mallory)
	// This packet is far outside the skew.
	send(alice, packet.WithTimestamp(time.Now().Add(-time.Hour).UnixMilli()))

	select {
	case arrived := <-to.Packets():
		t.Fatalf("a packet that the relay must drop arrived: %d bytes", len(arrived))
	case <-time.After(500 * time.Millisecond):
	}

	// The connection still works after the drops.
	send(alice)
	select {
	case <-to.Packets():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stopped after a dropped packet")
	}
}

func TestOneCitizenHoldsOneRoute(t *testing.T) {
	server := start(t)
	alice, from := citizen(t, server)
	bob, first := citizen(t, server)

	// Bob joins again from another machine. The new connection takes the route.
	second := join(t, server, bob)

	talk, err := session.Establish(alice, bob.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, seq, err := talk.Encrypt([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := packet.Encode(alice, bob.PublicKey, talk.ID, seq, nonce, ciphertext, packet.WithEphemeralKey(talk.EphemeralPublic))
	if err != nil {
		t.Fatal(err)
	}
	if err := from.SendPacket(raw); err != nil {
		t.Fatal(err)
	}

	select {
	case <-second.Packets():
	case <-first.Packets():
		t.Error("the old connection still holds the route")
	case <-time.After(5 * time.Second):
		t.Fatal("the packet did not arrive")
	}
}
